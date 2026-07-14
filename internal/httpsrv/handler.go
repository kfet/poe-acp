// Package httpsrv wires Poe HTTP requests into the router.
package httpsrv

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	kitlog "github.com/kfet/acp-kit/log"
	"github.com/kfet/poe-acp/internal/command"
	"github.com/kfet/poe-acp/internal/poeproto"
	"github.com/kfet/poe-acp/internal/router"
	"github.com/kfet/poe-acp/internal/statusline"
)

// fastCancelThreshold is the elapsed-time floor below which a client
// disconnect during a turn is logged as a permanent (always-on) WARN:
// Poe dropped the bot-facing connection before the turn could realistically
// start. See handleQuery. Declared as a var so tests can tighten/loosen it
// deterministically without wall-clock waits.
var fastCancelThreshold = 2 * time.Second

// Config configures a Handler.
type Config struct {
	Router *router.Router
	// Settings is the static response for `settings` requests. Parameter
	// controls may be overridden per-request by ParameterControlsProvider.
	Settings poeproto.SettingsResponse
	// HeartbeatInterval is the SSE heartbeat tick. The heartbeat
	// emits an animated `> _Thinking._` spinner via replace_response
	// until the first user-visible write closes the gate. <=0 disables
	// the heartbeat. Doubles as the spinner animation rate, so values
	// in the 1–2s range read well to humans.
	HeartbeatInterval time.Duration
	// ParameterControlsProvider, if set, is called on each `settings`
	// request to populate SettingsResponse.ParameterControls. If nil,
	// Settings.ParameterControls is used as-is.
	ParameterControlsProvider func() *poeproto.ParameterControls
	// Commands, if set, intercepts relay chat-commands (login family,
	// !help, !status, !models, !model, !new — any of the sigils /, !, .)
	// and pasted redirect URLs from in-flight logins before they reach
	// the router. Optional; nil disables the command surface.
	Commands CommandHandler
	// TurnTimeout is an OPTIONAL absolute wall-clock ceiling on a prompt
	// turn run on a context DECOUPLED from the request ctx. Poe tears
	// down the bot-facing HTTP connection pre-output on a transport drop;
	// decoupling lets the in-flight turn finish so its answer can be
	// buffered and served on the redrive, instead of aborting and losing
	// the work.
	//
	// <=0 (the default) means NO absolute ceiling: a turn is bounded
	// SOLELY by the progress-resetting IdleWriteTimeout backstop. While
	// the agent keeps producing user-visible output the turn runs for as
	// long as it needs — a long, actively-working turn is never cut. The
	// absolute cap is opt-in for operators who deliberately want a hard
	// upper bound regardless of progress; it is NOT a wedge guard (that
	// is IdleWriteTimeout's job).
	TurnTimeout time.Duration
	// AnswerTTL bounds how long a buffered (absorbed) turn answer is held
	// for a redrive before it is discarded. <=0 falls back to
	// defaultAnswerTTL (2m).
	AnswerTTL time.Duration
	// IdleWriteTimeout is the per-stream backstop for a WEDGED turn: the
	// agent has hung, no SSE content byte has been written, and the
	// client never disconnected. If no user-visible chunk lands within
	// this window the single wedged turn is cancelled so it cannot block
	// a graceful drain forever; every other in-flight stream keeps
	// draining. Heartbeat keepalive frames do NOT reset it — only real
	// agent output does — so a genuinely wedged turn is detected even
	// though its spinner keeps ticking. A tool_call session/update DOES
	// reset it: a legitimately long-running tool is genuine progress, so
	// it must not be cut. <=0 falls back to defaultIdleWriteTimeout (2m).
	IdleWriteTimeout time.Duration
	// StallThreshold is how long the SSE stream may go without a
	// user-visible content write before the heartbeat re-arms the
	// mid-turn keepalive spinner. Poe drops the bot-facing connection on
	// content-starvation, so during a long tool-heavy turn (no tokens
	// for minutes) the spinner must resume to keep the stream alive.
	// It re-arms via `replace_response` (a content event that renders in
	// place), preserving the text so far and animating a transient
	// status line below it; the moment real output resumes the spinner
	// is stripped. Before the first output there is no stall gate — the
	// cold-start spinner fires immediately (see heartbeat). <=0 falls
	// back to defaultStallThreshold (8s), conservatively under Poe's
	// drop tolerance. Reuses HeartbeatInterval for the animation cadence.
	StallThreshold time.Duration
}

// TurnTimeout has no default: <=0 means no absolute ceiling, leaving the
// progress-resetting IdleWriteTimeout backstop as the sole guard (see the
// Config.TurnTimeout doc).

// defaultAnswerTTL bounds buffered-answer retention when Config.AnswerTTL
// is unset. Poe redrives a transport drop within a few seconds; 2m is
// generous headroom while keeping memory bounded.
const defaultAnswerTTL = 2 * time.Minute

// defaultIdleWriteTimeout bounds a wedged turn when Config.IdleWriteTimeout
// is unset. Generous enough that a slow-to-first-token model is not cut,
// tight enough that a hung agent cannot stall a drain indefinitely.
const defaultIdleWriteTimeout = 2 * time.Minute

// defaultStallThreshold bounds output silence before the mid-turn
// keepalive spinner re-arms, when Config.StallThreshold is unset. 8s is
// conservatively under Poe's content-starvation drop tolerance while
// keeping re-arm frames rare on a normally-streaming turn.
const defaultStallThreshold = 8 * time.Second

// CommandHandler is the surface httpsrv depends on; *command.Broker
// implements it. Extracted so tests can inject handlers that return
// odd combinations the real one can't produce.
type CommandHandler interface {
	HasPending(convID string) bool
	Handle(ctx context.Context, convID, text string) (*command.Outcome, error)
	// Passthrough reports whether text is an allowlisted agent command;
	// if ok, rewritten is the prompt text to forward to the agent.
	Passthrough(text string) (rewritten string, ok bool)
}

// Handler serves the /poe endpoint.
type Handler struct {
	cfg     Config
	answers *answerBuffer
}

// New creates a Handler. HeartbeatInterval <=0 disables heartbeat;
// otherwise no defaulting is applied — pass an explicit value.
// AnswerTTL and IdleWriteTimeout default when <=0 (see their docs).
// TurnTimeout is NOT defaulted: <=0 means no absolute turn ceiling, so the
// progress-resetting IdleWriteTimeout backstop is the sole guard.
func New(cfg Config) *Handler {
	if cfg.AnswerTTL <= 0 {
		cfg.AnswerTTL = defaultAnswerTTL
	}
	if cfg.IdleWriteTimeout <= 0 {
		cfg.IdleWriteTimeout = defaultIdleWriteTimeout
	}
	if cfg.StallThreshold <= 0 {
		cfg.StallThreshold = defaultStallThreshold
	}
	return &Handler{cfg: cfg, answers: newAnswerBuffer(cfg.AnswerTTL)}
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	req, err := poeproto.Decode(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	switch req.Type {
	case poeproto.TypeSettings:
		s := h.cfg.Settings
		if h.cfg.ParameterControlsProvider != nil {
			s.ParameterControls = h.cfg.ParameterControlsProvider()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s)

	case poeproto.TypeQuery:
		h.handleQuery(r.Context(), w, req)

	case poeproto.TypeReportReaction:
		h.handleReaction(r.Context(), req)
		w.WriteHeader(http.StatusOK)

	case poeproto.TypeReportFeedback, poeproto.TypeReportError:
		w.WriteHeader(http.StatusOK)

	default:
		http.Error(w, "unknown request type: "+req.Type, http.StatusBadRequest)
	}
}

// DebugHandler returns an http.Handler that dumps router state as JSON.
func DebugHandler(r *router.Router) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sessions": r.Debug(),
			"count":    r.Len(),
		})
	})
}

func (h *Handler) handleQuery(ctx context.Context, w http.ResponseWriter, req *poeproto.Request) {
	sse, err := poeproto.NewSSEWriter(w)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Flush a padded SSE comment immediately, before any session work, so
	// a buffering proxy (Tailscale Funnel) forwards first bytes to Poe
	// right away. Without this Poe sees nothing during the ~400ms session
	// resume and drops the bot connection at ~15ms. See SSEWriter.Preamble.
	if err := sse.Preamble(); err != nil {
		log.Printf("sse preamble: %v", err)
		return
	}
	if err := sse.Meta(); err != nil {
		log.Printf("sse meta: %v", err)
		return
	}

	turns := make([]router.Turn, 0, len(req.Query))
	for _, m := range req.Query {
		t := router.Turn{Role: m.Role, Content: m.Content, MessageID: m.MessageID}
		// Defensive: if the operator turned attachments off, strip them
		// even if a misbehaving / stale Poe client still sends some.
		if h.cfg.Settings.AllowAttachments && len(m.Attachments) > 0 {
			t.Attachments = make([]router.Attachment, 0, len(m.Attachments))
			for _, a := range m.Attachments {
				t.Attachments = append(t.Attachments, router.Attachment{
					URL:           a.URL,
					ContentType:   a.ContentType,
					Name:          a.Name,
					ParsedContent: a.ParsedContent,
				})
			}
		}
		turns = append(turns, t)
	}

	// Command intercept: relay-owned chat commands (login family, !help,
	// !status/!models/!model/!new — any accepted sigil) and pasted
	// redirect URLs for an in-flight login are handled out-of-band, never
	// reaching the agent. Allowlisted agent commands (e.g. !reload) are
	// instead rewritten to their slash form and forwarded through the
	// normal prompt path so the agent executes them and streams a reply.
	if h.cfg.Commands != nil {
		latest := latestUserTurn(turns)
		if latest != "" {
			if h.cfg.Commands.HasPending(req.ConversationID) || command.IsCommand(latest) {
				h.handleAuth(ctx, sse, req.ConversationID, latest)
				return
			}
			if rewritten, ok := h.cfg.Commands.Passthrough(latest); ok {
				turns = rewriteLatestUserTurn(turns, rewritten)
			}
		}
	}

	opts := router.ParseOptions(req.LatestParameters(), h.cfg.Router.Defaults())

	if kitlog.Enabled() {
		kitlog.Debugf("query: conv=%q user=%q msg=%q turns=%d",
			req.ConversationID, req.UserID, req.MessageID, len(req.Query))
		for i, m := range req.Query {
			contentPreview := truncateRunes(m.Content, 80)
			pj, _ := json.Marshal(m.Parameters)
			kitlog.Debugf("  turn[%d] role=%s msg_id=%q att=%d params=%s content=%q",
				i, m.Role, m.MessageID, len(m.Attachments), string(pj), contentPreview)
		}
		latestPJ, _ := json.Marshal(req.LatestParameters())
		defaults := h.cfg.Router.Defaults()
		kitlog.Debugf("  latest_params=%s", string(latestPJ))
		kitlog.Debugf("  defaults: model=%q thinking=%q hide_thinking=%v",
			defaults.Model, defaults.Thinking, defaults.HideThinking)
		kitlog.Debugf("  parsed_opts: model=%q thinking=%q hide_thinking=%v",
			opts.Model, opts.Thinking, opts.HideThinking)
	}

	// Sink: SSE writer + heartbeat coordination + disconnect → cancel.
	// The heartbeat animates a status-line spinner (provider emoji +
	// mood + plan + Thinking…) that the orderedWriter clears the
	// moment the first real chunk lands. (hide_thinking is a
	// router-level concern: it suppresses agent_thought_chunk content
	// from the stream, not the spinner.)
	s := newSink(sse, h.cfg.HeartbeatInterval, h.cfg.StallThreshold)
	// Pre-seed the provider emoji from the handler-resolved model so
	// tick #1 of the spinner already carries it. The router re-runs
	// the resolution after applyOptions returns (success or failure)
	// so the header tracks the actually-applied model, not the
	// requested one. Unknown providers return "" → segment dropped.
	s.SetProviderEmoji(statusline.ProviderEmojiForModel(opts.Model))
	defer s.stop()

	// Redrive fast-path: if Poe re-sends a query whose original response
	// we absorbed (client dropped pre-output) and we buffered, serve the
	// completed answer from the buffer instead of re-running the agent.
	// Keyed by conv + latest user message_id (stable across a redrive).
	key := answerKey(req.ConversationID, latestUserMessageID(turns))
	if key != "" {
		if calls, ok := h.answers.take(key); ok {
			kitlog.Debugf("redrive served from buffer: conv=%s msg=%s", req.ConversationID, req.MessageID)
			replay(calls, s)
			return
		}
	}

	// Decouple the prompt turn from the request ctx: Poe drops the
	// bot-facing HTTP connection pre-output on a transport drop, which
	// would otherwise abort the in-flight turn. Run on a context that
	// drops the caller's cancellation. By default there is NO absolute
	// deadline — an actively-progressing turn is never cut; the
	// progress-resetting idle-write backstop (watchIdle, below) is the
	// sole guard and cancels only a genuinely wedged turn. An operator
	// may opt into a hard wall-clock ceiling via TurnTimeout>0.
	var turnCtx context.Context
	var cancelTurn context.CancelFunc
	if h.cfg.TurnTimeout > 0 {
		turnCtx, cancelTurn = context.WithTimeout(context.WithoutCancel(ctx), h.cfg.TurnTimeout)
	} else {
		turnCtx, cancelTurn = context.WithCancel(context.WithoutCancel(ctx))
	}
	defer cancelTurn()

	rec := &answerRecorder{inner: s}

	// Gated cancel propagation. When the HTTP client goes away mid-turn:
	//   - first output ALREADY landed (realWritten) → a real user Stop;
	//     forward session/cancel so the agent stops burning tokens.
	//   - no output yet → a transport drop (all observed drops are
	//     pre-output at 9–16ms); ABSORB it: do not cancel, let the
	//     decoupled turn finish, and buffer the answer for the redrive.
	// Poe has no cancel signal, so a user Stop and a transport drop are
	// indistinguishable upstream — the first-output gate is the
	// discriminator, with the redrive-absence backstop discarding a
	// mis-absorbed Stop's buffer when no redrive arrives (it TTL-expires).
	var absorbed atomic.Bool
	done := make(chan struct{})
	watcherDone := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(watcherDone)
		select {
		case <-ctx.Done():
			elapsed := time.Since(start)
			if elapsed < fastCancelThreshold {
				log.Printf("WARN fast client disconnect: conv=%s elapsed=%s — Poe dropped the bot connection before the turn started", req.ConversationID, elapsed.Round(time.Millisecond))
			}
			// Latch the decision at the instant the client went away:
			// realWritten then is the discriminator. (Re-reading it after
			// the turn completes would be wrong — output lands by then.)
			if s.realWritten() {
				_ = h.cfg.Router.Cancel(context.Background(), req.ConversationID)
			} else {
				absorbed.Store(true)
			}
			if absorbDecidedHook != nil {
				absorbDecidedHook()
			}
		case <-done:
		}
	}()

	// Wedged-turn backstop. The agent has hung if no user-visible byte
	// lands within IdleWriteTimeout while the client is still connected
	// (a caller-cancel is handled by the watcher above). When it fires we
	// cancel this one turn so it cannot block a graceful drain forever;
	// every other in-flight stream keeps draining. Heartbeat keepalives
	// do not reset the idle clock (see sink.touch), so the spinner
	// ticking does not mask a wedge. Exits when done is closed.
	idleDone := make(chan struct{})
	go func() { defer close(idleDone); h.watchIdle(cancelTurn, s, done, req.ConversationID) }()

	err = h.cfg.Router.Prompt(turnCtx, req.ConversationID, req.UserID, turns, opts, rec)
	close(done)
	<-watcherDone
	<-idleDone
	if err != nil {
		log.Printf("router prompt (conv=%s): %v", req.ConversationID, err)
	}
	if absorbed.Load() && key != "" {
		h.answers.put(key, rec.snapshot())
		kitlog.Debugf("absorbed pre-output drop: buffered answer conv=%s msg=%s", req.ConversationID, req.MessageID)
	}
}

// watchIdle is the wedged-turn backstop goroutine. It polls the sink's
// idle clock and cancels the turn if no user-visible byte has landed
// within IdleWriteTimeout. It exits as soon as the turn completes (done
// closed) — so a turn that ends normally, or is cut by an opt-in
// TurnTimeout ceiling, never trips the idle path. idleWriteCancelHook is a test-only seam fired when
// the idle path cancels.
func (h *Handler) watchIdle(cancelTurn context.CancelFunc, s *sink, done <-chan struct{}, convID string) {
	t := time.NewTicker(idleCheckInterval(h.cfg.IdleWriteTimeout))
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			if s.idleSince() >= h.cfg.IdleWriteTimeout {
				log.Printf("WARN idle-write timeout: conv=%s no agent output in %s — cutting wedged stream",
					convID, h.cfg.IdleWriteTimeout)
				cancelTurn()
				if idleWriteCancelHook != nil {
					idleWriteCancelHook()
				}
				return
			}
		}
	}
}

// idleWriteCancelHook, when non-nil, is invoked after watchIdle cancels a
// wedged turn. Test-only seam. nil in production.
var idleWriteCancelHook func()

// idleCheckInterval derives the idle poll cadence from the timeout: a
// quarter of the window, floored at 10ms so tiny test timeouts still tick.
func idleCheckInterval(timeout time.Duration) time.Duration {
	if iv := timeout / 4; iv >= 10*time.Millisecond {
		return iv
	}
	return 10 * time.Millisecond
}

// absorbDecidedHook, when non-nil, is invoked after the disconnect watcher
// latches its absorb/cancel decision. Test-only seam so a test can release
// a blocked turn only AFTER the decision is latched, avoiding a race
// between client-disconnect and turn-completion. nil in production.
var absorbDecidedHook func()

// sink adapts SSEWriter to router.ChunkSink, with an animated
// status-line spinner that doubles as the SSE keepalive. There is no
// separate "invisible heartbeat" mode — the spinner IS the keepalive.
//
// Unlike the earlier design (spinner ran only until the first
// user-visible write, then self-disarmed forever), the spinner now runs
// for the WHOLE turn. Poe drops the bot-facing connection on
// content-starvation, so a long tool-heavy turn that emits no tokens for
// minutes would be idle-dropped mid-answer. To prevent that, orderedWriter
// keeps an accumulator `acc` of all user-visible text emitted so far and
// re-arms the spinner whenever the stream stalls:
//
//   - Normal path (output flowing): cheap `text` appends, no spinner.
//     `acc` grows by each appended chunk.
//   - Stall (no output past StallThreshold): the heartbeat emits
//     `Replace(acc + "\n\n" + <spinner line>)` — a content event that
//     re-renders the text so far plus a transient animated status line
//     below it, satisfying Poe's keepalive without corrupting markdown.
//   - Resume: the next user write strips the spinner with `Replace(acc)`
//     (clearSpinnerLocked) then continues cheap appends.
//
// The cold-start spinner is the `acc==""` special case: the same
// re-arm path, with an empty prefix, fired immediately (see heartbeat)
// so Poe sees a content event within milliseconds of `meta`.
//
// Concurrency model — single-writer invariant:
//
// The heartbeat goroutine is a SECOND writer to the SSE stream
// concurrent with the router-driven chunk path. The gate lives INSIDE
// the writer: orderedWriter owns the SSEWriter, `acc`, a `realWritten`
// flag, `spinnerVisible`, and a single mutex; every frame — user write
// or heartbeat — is composed and written under that mutex, so a
// heartbeat frame can never interleave with or lose user content. A
// heartbeat frame emitted after user content is legitimate now (mid-turn
// keepalive) BUT always carries `acc`, so it re-renders (never discards)
// the text so far. The heartbeat self-disarms only when the stream is
// sealed (Done/Error close the gate).
type orderedWriter struct {
	w  *poeproto.SSEWriter
	mu sync.Mutex
	// realWritten flips true the first time a user-visible write lands.
	// Never reset. The handler's gated-cancel path reads it to tell a
	// real user Stop (output streaming) from a pre-output transport drop.
	realWritten bool
	closed      bool // Done emitted; further writes are no-ops
	// acc accumulates every user-visible text byte emitted so far, so a
	// mid-turn keepalive spinner (Replace) can re-render the answer with a
	// transient status line appended, and strip back to exactly acc when
	// output resumes. Guarded by mu.
	acc string
	// spinnerVisible is true if the last frame on the wire was a spinner
	// (a Replace whose body ends in the transient status line). The next
	// user write must Replace(acc) to strip it before appending, since
	// Poe `text` events append to whatever the renderer's body is.
	spinnerVisible bool
	// spinnerSealed disables further heartbeat frames after a terminal
	// user event (Error or Done). It is distinct from `closed`: an
	// `error` event is followed by a mandatory `done`, so Error must NOT
	// set `closed` (that would no-op the trailing userDone) — but it MUST
	// stop the spinner so a tick already past its stall check cannot land
	// a `replace_response` AFTER the error on the wire. hbFrame gates on
	// both.
	spinnerSealed bool
}

// userText writes a `text` SSE event and marks the stream as having
// real content. It appends s to the accumulator so a later keepalive
// spinner can re-render the full answer. If a spinner frame is on
// screen, it is stripped with Replace(acc) first — Poe `text` events
// append to the renderer's current body, so without the strip the
// answer would render below the spinner line. The strip's IO error is
// intentionally swallowed: if the SSE connection has dropped, the
// subsequent o.w.Text(s) will surface the same failure.
func (o *orderedWriter) userText(s string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil
	}
	o.clearSpinnerLocked()
	o.realWritten = true
	o.acc += s
	return o.w.Text(s)
}

// userReplace writes a user-driven `replace_response` event. The
// replace overwrites the whole body, so it becomes the new accumulator
// value and no pre-clear is needed.
func (o *orderedWriter) userReplace(s string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil
	}
	o.realWritten = true
	o.acc = s
	o.spinnerVisible = false
	return o.w.Replace(s)
}

// userError writes an `error` SSE event. Pre-strips a visible spinner
// so the error rendering isn't preceded by the transient status line.
func (o *orderedWriter) userError(text, et string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil
	}
	o.clearSpinnerLocked()
	o.realWritten = true
	o.spinnerSealed = true
	return o.w.Error(text, et)
}

// userDone writes the terminal `done` event and seals the stream so
// nothing else (heartbeat or stray user write) can be emitted after.
// If a spinner is the last thing on screen, strip it first (back to
// acc) so the user doesn't see a frozen status line as their final
// content.
func (o *orderedWriter) userDone() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil
	}
	o.clearSpinnerLocked()
	o.spinnerSealed = true
	o.closed = true
	return o.w.Done()
}

// userFile emits a `file` SSE event advertising an output attachment.
// Like userText it counts as real content: it strips any visible
// spinner and marks realWritten. A file event does not add to the text
// accumulator (it is a distinct event type, not body text).
func (o *orderedWriter) userFile(url, contentType, name, inlineRef string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil
	}
	o.clearSpinnerLocked()
	o.realWritten = true
	return o.w.File(url, contentType, name, inlineRef)
}

// clearSpinnerLocked strips a visible spinner frame ahead of a user
// write by re-rendering the body as exactly the accumulated text.
// Caller must hold o.mu. When acc=="" this is Replace("") — the
// cold-start clear. Errors are swallowed; see userText.
func (o *orderedWriter) clearSpinnerLocked() {
	if o.spinnerVisible {
		_ = o.w.Replace(o.acc)
		o.spinnerVisible = false
	}
}

// hbFrame writes a heartbeat-driven keepalive spinner as a
// `replace_response` event. The body is the accumulated user text so
// far plus the given transient status line, so the frame re-renders
// (never discards) the answer and shows the live status below it. When
// acc=="" (cold start, no output yet) the body is just the line.
// Returns gateOpen=true iff the frame went on the wire — false means the
// stream has been sealed by a terminal user event (Error or Done) and
// the heartbeat goroutine should self-disarm. Gating on spinnerSealed
// (not just closed) prevents a tick already past its stall check from
// landing a spinner frame AFTER an `error` event, whose trailing `done`
// is still pending.
func (o *orderedWriter) hbFrame(line string) (gateOpen bool, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed || o.spinnerSealed {
		return false, nil
	}
	body := line
	if o.acc != "" {
		body = o.acc + "\n\n" + line
	}
	o.spinnerVisible = true
	return true, o.w.Replace(body)
}

// hasOutput reports whether the first user-visible write has landed.
func (o *orderedWriter) hasOutput() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.realWritten
}

// isClosed reports whether the stream has been sealed (Done emitted).
func (o *orderedWriter) isClosed() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.closed
}

type sink struct {
	o *orderedWriter

	// lastWrite is the unix-nano timestamp of the most recent event that
	// counts as liveness for the WEDGE backstop: a user-visible write
	// (Text/Replace/Error/Done/File) OR a tool_call session/update
	// (touchTool). Heartbeat keepalive frames deliberately do NOT update
	// it. A genuinely hung agent — no text AND no tool activity — trips
	// IdleWriteTimeout even while its spinner keeps ticking; a
	// legitimately long-running tool keeps resetting it and is not cut.
	lastWrite atomic.Int64

	// lastContent is the unix-nano timestamp of the most recent
	// user-visible content write ONLY. Tool_call updates do NOT reset it
	// (a running tool produces no SSE content, so Poe still starves) and
	// neither do heartbeat frames. The heartbeat measures stall against
	// this clock to decide when to re-arm the mid-turn keepalive spinner.
	lastContent atomic.Int64

	// stall is how long lastContent may go stale before the heartbeat
	// re-arms the spinner mid-turn. Copied from Config.StallThreshold.
	stall time.Duration

	// hbDone is closed exactly once via hbStop to wake the heartbeat
	// goroutine for prompt shutdown (Done / Error). The heartbeat also
	// self-exits on its next tick once the orderedWriter gate has closed
	// (stream sealed) — so even if every explicit stop call were removed,
	// the goroutine would terminate within one tick of Done/Error.
	hbDone chan struct{}
	hbStop sync.Once
	// hbExited is closed by the heartbeat goroutine on exit. Tests
	// wait on this to read the SSE body race-free; production callers
	// don't observe it. Pre-closed when no heartbeat goroutine is spawned.
	hbExited chan struct{}

	// statusMu guards the dev.acp-kit.status-line/v1 state below plus
	// the transient tool-activity label. Kept separate from
	// orderedWriter.mu because the heartbeat goroutine reads it on every
	// tick, the router's drain goroutine writes it on every
	// session/update, and the chunk path reads it once on header-prepend.
	statusMu      sync.Mutex
	status        statusline.Status
	activity      string // transient spinner label from the running tool
	headerEmitted bool   // true once a final-header prepend has been considered
}

func newSink(w *poeproto.SSEWriter, hb, stall time.Duration) *sink {
	s := &sink{
		o:        &orderedWriter{w: w},
		stall:    stall,
		hbDone:   make(chan struct{}),
		hbExited: make(chan struct{}),
	}
	now := time.Now().UnixNano()
	s.lastWrite.Store(now)
	s.lastContent.Store(now)
	if hb > 0 {
		go s.heartbeat(hb)
	} else {
		// Heartbeat disabled: pre-close so stop() is a no-op and tests
		// that wait on hbExited don't block.
		s.hbStop.Do(func() { close(s.hbDone) })
		close(s.hbExited)
	}
	return s
}

// heartbeatTickHook, when non-nil, is invoked after each heartbeat tick
// completes. Test-only seam so spinner tests can wait on real ticks
// instead of wall-clock sleeps. nil in production.
var heartbeatTickHook func()

func (s *sink) heartbeat(every time.Duration) {
	defer close(s.hbExited)
	t := time.NewTicker(every)
	defer t.Stop()
	// spinTick is owned solely by this goroutine; no synchronisation.
	var spinTick int
	// Emit tick #0 IMMEDIATELY (the loop condition's first evaluation),
	// before waiting on the ticker, so a real `replace_response`
	// content frame lands within milliseconds of `meta`. Poe drops a
	// new-conversation bot connection that sees only the preamble +
	// meta (a non-content event) during the 0..HeartbeatInterval gap —
	// emitting the first spinner frame at t≈0 closes that
	// content-starvation window. Each subsequent ticker tick re-evaluates
	// the condition. emitSpinnerFrame returns false (ending the loop)
	// only when the stream is sealed (Done/Error) — the heartbeat runs
	// the WHOLE turn, re-arming the spinner on every detected stall.
	for s.emitSpinnerFrame(&spinTick) {
		select {
		case <-s.hbDone:
			return
		case <-t.C:
		}
	}
}

// emitSpinnerFrame is called once per heartbeat tick. It fires
// heartbeatTickHook (so tick #0 is observed by tests just like
// ticker-driven ticks) and returns keepGoing=false only when the stream
// is sealed and the goroutine should exit.
//
// The spinner is emitted on a tick when EITHER no user-visible output
// has landed yet (cold start — close Poe's content-starvation window
// immediately) OR the stream has gone stall-threshold silent since the
// last content write (mid-turn keepalive during a long tool call). On a
// normally-streaming turn neither holds, so the tick is a no-op and the
// cheap `text` append fast-path is preserved. When it does fire, the
// frame is a `replace_response` carrying the accumulated answer plus a
// transient status line (see orderedWriter.hbFrame): replace overwrites
// the prior frame so the dots animate in place, and re-rendering `acc`
// means the keepalive never discards the answer or corrupts Poe's
// Markdown the way an appended `text` keepalive would. *spinTick is the
// goroutine-private animation counter, advanced only on an emitted frame.
//
// Bandwidth note: during a stall each animation frame re-sends the full
// `acc` at HeartbeatInterval cadence, so a large answer stalled for a
// long time costs O(len(acc) × stall-ticks) bytes. This is bounded to
// actual stall gaps (the normal streaming path stays on cheap `text`
// appends) and is acceptable in practice; if it ever matters, the
// animation (but not the first re-arm frame, which is what defeats
// starvation) could be decimated to every Nth tick.
func (s *sink) emitSpinnerFrame(spinTick *int) (keepGoing bool) {
	if heartbeatTickHook != nil {
		heartbeatTickHook()
	}
	stalled := !s.o.hasOutput() || s.contentIdleSince() >= s.stall
	if !stalled {
		// Output is flowing (or just landed): nothing to keepalive.
		// Keep ticking unless the stream has been sealed.
		return !s.o.isClosed()
	}
	*spinTick++
	dots := strings.Repeat(".", 1+(*spinTick-1)%3)
	frame := statusline.Spinner(s.snapshotStatus(), s.snapshotActivity(), dots)
	gateOpen, _ := s.o.hbFrame(frame)
	return gateOpen
}

// stop wakes the heartbeat goroutine for prompt shutdown. Idempotent.
// Safe to call any number of times from any goroutine. Even if never
// called, the heartbeat self-disarms via the orderedWriter gate on its
// next tick after the stream is sealed (Done/Error).
func (s *sink) stop() {
	s.hbStop.Do(func() { close(s.hbDone) })
}

// FirstChunk — router calls this on the first real agent chunk. In the
// mid-turn-keepalive design the heartbeat runs the WHOLE turn (re-arming
// on every stall), so first output must NOT stop it. Kept as a no-op to
// satisfy the ChunkSink interface; the spinner is disarmed only when the
// stream is sealed (Done/Error via stop()).
func (s *sink) FirstChunk() {}

// realWritten reports whether the first user-visible write has landed on
// the stream. The handler's gated-cancel path uses this to distinguish a
// real user Stop (output already streaming) from a pre-output transport
// drop (nothing written yet → absorb + buffer).
func (s *sink) realWritten() bool {
	s.o.mu.Lock()
	defer s.o.mu.Unlock()
	return s.o.realWritten
}

// touch records a user-visible content write: it resets BOTH the wedge
// clock (lastWrite) and the spinner-stall clock (lastContent), and
// clears any transient tool-activity spinner label — real output
// supersedes a "running tool…" indicator. Heartbeat frames never call
// it. Called by every user-visible write (Text/Replace/Error/Done/File).
func (s *sink) touch() {
	now := time.Now().UnixNano()
	s.lastWrite.Store(now)
	s.lastContent.Store(now)
	s.statusMu.Lock()
	s.activity = ""
	s.statusMu.Unlock()
}

// touchTool records tool-call progress: it resets ONLY the wedge clock
// (lastWrite), so a legitimately long-running tool is not cut by the
// idle-write backstop. It deliberately does NOT reset lastContent — a
// running tool produces no SSE content, so the mid-turn keepalive
// spinner must still re-arm to keep Poe from starving — and it does NOT
// mark realWritten or emit body text.
func (s *sink) touchTool() { s.lastWrite.Store(time.Now().UnixNano()) }

// idleSince reports how long since the last wedge-liveness event
// (user-visible write OR tool-call progress).
func (s *sink) idleSince() time.Duration {
	return time.Since(time.Unix(0, s.lastWrite.Load()))
}

// contentIdleSince reports how long since the last user-visible content
// write. The heartbeat re-arms the spinner once this exceeds s.stall.
func (s *sink) contentIdleSince() time.Duration {
	return time.Since(time.Unix(0, s.lastContent.Load()))
}

func (s *sink) Text(t string) error      { s.touch(); return s.o.userText(s.maybePrependHeader(t)) }
func (s *sink) Replace(t string) error   { s.touch(); return s.o.userReplace(t) }
func (s *sink) Error(t, et string) error { s.touch(); s.stop(); return s.o.userError(t, et) }
func (s *sink) Done() error              { s.touch(); s.stop(); return s.o.userDone() }

func (s *sink) File(url, contentType, name, inlineRef string) error {
	s.touch()
	s.stop()
	return s.o.userFile(url, contentType, name, inlineRef)
}

// ToolActivity signals agent tool-call progress (an ACP tool_call /
// tool_call_update session/update). It resets the wedge clock so a
// legitimately long tool is not cut, and records the running tool's
// label so the mid-turn keepalive spinner can show it. It MUST NOT mark
// realWritten or emit body text: a tool_call is not user-visible content
// and does not satisfy Poe's content-starvation keepalive (only the
// spinner replace_response does).
func (s *sink) ToolActivity(label string) {
	s.touchTool()
	s.statusMu.Lock()
	s.activity = label
	s.statusMu.Unlock()
}

// SetProviderEmoji records the relay-resolved provider emoji for the
// active turn. Router calls this once after applyOptions resolves the
// effective model. Empty string means the provider is unknown and the
// segment will be dropped by the renderer.
func (s *sink) SetProviderEmoji(emoji string) {
	s.statusMu.Lock()
	s.status.ProviderEmoji = emoji
	s.statusMu.Unlock()
}

// SetStatus records the agent-supplied mood/plan labels from the
// latest session/update._meta carrying dev.acp-kit.status-line/v1.
// Both fields are already trimmed and length-capped by the parser.
// May be called many times per turn; the renderer keeps the latest.
func (s *sink) SetStatus(mood, plan string) {
	s.statusMu.Lock()
	s.status.Mood = mood
	s.status.Plan = plan
	s.statusMu.Unlock()
}

// snapshotStatus returns a value copy of the current status line state
// for the heartbeat / header renderer.
func (s *sink) snapshotStatus() statusline.Status {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	return s.status
}

// snapshotActivity returns the current transient tool-activity label for
// the heartbeat spinner (empty means the default "Thinking").
func (s *sink) snapshotActivity() string {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	return s.activity
}

// maybePrependHeader injects the final-message status header in front
// of the first user-visible text chunk, exactly once. Subsequent
// chunks pass through unchanged. If the rendered header is empty
// (unknown provider + no agent _meta), nothing is prepended. Replace /
// Error / Done paths intentionally do NOT prepend: they overwrite or
// terminate the body, so a header there would be erased or out of
// place.
func (s *sink) maybePrependHeader(t string) string {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	if s.headerEmitted {
		return t
	}
	s.headerEmitted = true
	h := statusline.Header(s.status)
	if h == "" {
		return t
	}
	return h + "\n\n" + t
}

// handleAuth runs an auth-flow turn end-to-end on the SSE stream. Always
// emits a single text payload + done, regardless of broker outcome.
func (h *Handler) handleAuth(ctx context.Context, sse *poeproto.SSEWriter, convID, text string) {
	out, err := h.cfg.Commands.Handle(ctx, convID, text)
	if err != nil {
		log.Printf("command (conv=%s): %v", convID, err)
		_ = sse.Error(err.Error(), "user_caused_error")
		_ = sse.Done()
		return
	}
	if out == nil {
		// Should not happen — broker returned nil for an auth turn.
		_ = sse.Done()
		return
	}
	if out.Text != "" {
		_ = sse.Text(out.Text)
	}
	_ = sse.Done()
}

// handleReaction queues an out-of-band reaction turn against the
// session. Returns immediately; the agent's response is discarded
// because Poe gives us no channel to deliver it on. Queue overflow is
// logged inside the router but the HTTP response is always 200 OK.
func (h *Handler) handleReaction(ctx context.Context, req *poeproto.Request) {
	if kitlog.Enabled() {
		kitlog.Debugf("report_reaction: conv=%q user=%q msg=%q reaction=%q action=%q",
			req.ConversationID, req.UserID, req.MessageID, req.Reaction, req.ReactionAction)
	}
	if req.Reaction == "" {
		// Malformed payload: nothing to forward. Log and ack.
		log.Printf("report_reaction (conv=%s): missing reaction kind; dropping",
			req.ConversationID)
		return
	}
	if err := h.cfg.Router.ReportReaction(
		ctx, req.ConversationID, req.UserID, req.MessageID,
		req.Reaction, string(req.ReactionAction),
	); err != nil {
		log.Printf("report_reaction (conv=%s): %v", req.ConversationID, err)
	}
}

// latestUserTurn returns the content of the most recent user turn, or ""
// if there isn't one.
func latestUserTurn(turns []router.Turn) string {
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Role == "user" {
			return turns[i].Content
		}
	}
	return ""
}

// latestUserMessageID returns the Poe message_id of the most recent user
// turn, or "" if there isn't one (or it carries no id). Used to key the
// answer buffer so a redrive of the same query maps to its buffered
// response.
func latestUserMessageID(turns []router.Turn) string {
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Role == "user" {
			return turns[i].MessageID
		}
	}
	return ""
}

// rewriteLatestUserTurn replaces the Content of the most recent user turn
// with text (used to forward an allowlisted agent command as its slash
// form). Returns turns unchanged if there is no user turn.
func rewriteLatestUserTurn(turns []router.Turn, text string) []router.Turn {
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Role == "user" {
			turns[i].Content = text
			return turns
		}
	}
	return turns
}

// truncateRunes shortens s to at most n runes, appending an ellipsis
// when truncation occurs. Rune-aware so it never splits a multi-byte
// UTF-8 sequence (which would corrupt debug-log output).
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

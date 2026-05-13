// Package httpsrv wires Poe HTTP requests into the router.
package httpsrv

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/kfet/poe-acp/internal/authbroker"
	"github.com/kfet/poe-acp/internal/debuglog"
	"github.com/kfet/poe-acp/internal/poeproto"
	"github.com/kfet/poe-acp/internal/router"
)

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
	// AuthBroker, if set, intercepts /login commands and pasted redirect
	// URLs from in-flight logins before they reach the router. Optional;
	// nil disables interactive auth.
	AuthBroker AuthBroker
}

// AuthBroker is the surface httpsrv depends on; *authbroker.Broker
// implements it. Extracted so tests can inject brokers that return
// odd combinations the real broker can't produce.
type AuthBroker interface {
	HasPending(convID string) bool
	Handle(ctx context.Context, convID, text string) (*authbroker.Outcome, error)
}

// Handler serves the /poe endpoint.
type Handler struct {
	cfg Config
}

// New creates a Handler. HeartbeatInterval <=0 disables heartbeat;
// otherwise no defaulting is applied — pass an explicit value.
func New(cfg Config) *Handler {
	return &Handler{cfg: cfg}
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

	// Auth broker intercept: /login commands or pasted redirect URLs for
	// an in-flight login are handled out-of-band, never reaching the
	// router.
	if h.cfg.AuthBroker != nil {
		latest := latestUserTurn(turns)
		if latest != "" && (h.cfg.AuthBroker.HasPending(req.ConversationID) || authbroker.IsLoginCommand(latest)) {
			h.handleAuth(ctx, sse, req.ConversationID, latest)
			return
		}
	}

	opts := router.ParseOptions(req.LatestParameters(), h.cfg.Router.Defaults())

	if debuglog.Enabled() {
		debuglog.Logf("query: conv=%q user=%q msg=%q turns=%d",
			req.ConversationID, req.UserID, req.MessageID, len(req.Query))
		for i, m := range req.Query {
			contentPreview := truncateRunes(m.Content, 80)
			pj, _ := json.Marshal(m.Parameters)
			debuglog.Logf("  turn[%d] role=%s msg_id=%q att=%d params=%s content=%q",
				i, m.Role, m.MessageID, len(m.Attachments), string(pj), contentPreview)
		}
		latestPJ, _ := json.Marshal(req.LatestParameters())
		defaults := h.cfg.Router.Defaults()
		debuglog.Logf("  latest_params=%s", string(latestPJ))
		debuglog.Logf("  defaults: model=%q thinking=%q hide_thinking=%v",
			defaults.Model, defaults.Thinking, defaults.HideThinking)
		debuglog.Logf("  parsed_opts: model=%q thinking=%q hide_thinking=%v",
			opts.Model, opts.Thinking, opts.HideThinking)
	}

	// Sink: SSE writer + heartbeat coordination + disconnect → cancel.
	// The heartbeat always animates a `> _Thinking._` spinner; the
	// orderedWriter clears it the moment the first real chunk lands,
	// regardless of hide_thinking. (hide_thinking is a router-level
	// concern: it suppresses agent_thought_chunk content from the
	// stream, not the spinner.)
	s := newSink(sse, h.cfg.HeartbeatInterval)
	defer s.stop()

	// Cancel propagation: if the HTTP client goes away while a prompt
	// is in flight, issue ACP session/cancel so the agent stops burning
	// tokens. Once the prompt returns (clean or error), stop watching —
	// we don't want to cancel a session that has already completed.
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = h.cfg.Router.Cancel(context.Background(), req.ConversationID)
		case <-done:
		}
	}()

	err = h.cfg.Router.Prompt(ctx, req.ConversationID, req.UserID, turns, opts, s)
	close(done)
	if err != nil {
		log.Printf("router prompt (conv=%s): %v", req.ConversationID, err)
	}
}

// sink adapts SSEWriter to router.ChunkSink, with an animated
// `> _Thinking._` spinner that runs until the first user-visible write
// arrives. The spinner doubles as the SSE keepalive — there's no
// separate "invisible heartbeat" mode. orderedWriter clears the
// spinner the moment the first real chunk lands, regardless of
// whether the user opted into seeing thoughts (hide_thinking is a
// router-level filter on agent_thought_chunk content; it does not
// affect the spinner).
//
// Concurrency model — single-writer invariant:
//
// The heartbeat goroutine is a SECOND writer to the SSE stream
// concurrent with the router-driven chunk path. Earlier designs gave
// each user-write method (Text, Replace, Error, Done) the obligation
// to "stop the heartbeat first" — a footgun: any new write site that
// forgot would let a stale heartbeat tick land AFTER the user content
// and silently overwrite it with Replace("") (or a "Thinking…" frame),
// leaving the user with garbled or missing output.
//
// The fix moves the gate INTO the writer. orderedWriter owns the
// SSEWriter, a `realWritten` flag, and a single mutex; the
// flag-check-and-write is atomic. Heartbeat frames go through
// hbReplace, which is a no-op once any user write has landed; user
// writes go through the user* methods, which set realWritten under the
// same mutex. The heartbeat goroutine self-disarms (returns) the first
// time hbReplace reports the gate has closed — no caller has to
// remember to stop it. The sink-layer Done() / Error() also close
// hbDone so the goroutine wakes immediately rather than waiting for
// the next tick.
type orderedWriter struct {
	w  *poeproto.SSEWriter
	mu sync.Mutex
	// realWritten flips true the first time a user-visible write lands.
	// Once true, hbReplace becomes a no-op so heartbeat ticks can never
	// appear after user content in the SSE event sequence.
	realWritten bool
	closed      bool // Done emitted; further writes are no-ops
	// spinnerVisible is true if the last hbReplace wrote a non-empty
	// body (i.e. a visible "Thinking…" frame is currently on screen).
	// userText uses this to emit a Replace("") clear before its append,
	// since Poe `text` events append to whatever the renderer thinks
	// the body is — without the clear, "answer" would render as
	// "> _Thinking._answer".
	spinnerVisible bool
}

// userText writes a `text` SSE event and marks the stream as having
// real content. Subsequent heartbeat ticks become no-ops. If a visible
// spinner frame is on screen, it is cleared with Replace("") first.
// The spinner-clear's IO error is intentionally swallowed: if the SSE
// connection has dropped, the subsequent o.w.Text(s) will surface the
// same failure.
func (o *orderedWriter) userText(s string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil
	}
	o.clearSpinnerLocked()
	o.realWritten = true
	return o.w.Text(s)
}

// userReplace writes a user-driven `replace_response` event. The
// replace itself overwrites any visible spinner, so no pre-clear is
// needed.
func (o *orderedWriter) userReplace(s string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil
	}
	o.realWritten = true
	o.spinnerVisible = false
	return o.w.Replace(s)
}

// userError writes an `error` SSE event. Pre-clears a visible spinner
// so the error rendering isn't preceded by "Thinking…" in the body.
func (o *orderedWriter) userError(text, et string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil
	}
	o.clearSpinnerLocked()
	o.realWritten = true
	return o.w.Error(text, et)
}

// userDone writes the terminal `done` event and seals the stream so
// nothing else (heartbeat or stray user write) can be emitted after.
// If a visible spinner is the only thing on screen, clear it first so
// the user doesn't see a frozen "Thinking…" as their final content.
func (o *orderedWriter) userDone() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil
	}
	o.clearSpinnerLocked()
	o.closed = true
	return o.w.Done()
}

// clearSpinnerLocked drops a visible spinner frame ahead of a user
// write. Caller must hold o.mu. Errors are swallowed; see userText.
func (o *orderedWriter) clearSpinnerLocked() {
	if !o.realWritten && o.spinnerVisible {
		_ = o.w.Replace("")
		o.spinnerVisible = false
	}
}

// hbReplace writes a heartbeat-driven `replace_response` event.
// Returns gateOpen=true iff the frame actually went on the wire (i.e.,
// the gate is still open — no user write has landed and the stream is
// not closed). The heartbeat goroutine uses gateOpen=false as its
// self-disarm signal.
func (o *orderedWriter) hbReplace(s string) (gateOpen bool, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.realWritten || o.closed {
		return false, nil
	}
	o.spinnerVisible = s != ""
	return true, o.w.Replace(s)
}

type sink struct {
	o *orderedWriter

	// hbDone is closed exactly once via hbStop to wake the heartbeat
	// goroutine for prompt shutdown (Done / Error / FirstChunk). The
	// heartbeat ALSO self-exits on its next tick when the orderedWriter
	// gate has closed — so even if every explicit stop call were
	// removed, the goroutine would terminate within one tick of the
	// first user write (and produce no garbled output in the meantime).
	hbDone chan struct{}
	hbStop sync.Once
	// hbExited is closed by the heartbeat goroutine on exit. Tests
	// wait on this to read the SSE body race-free; production callers
	// don't observe it. Pre-closed when no heartbeat goroutine is spawned.
	hbExited chan struct{}
}

func newSink(w *poeproto.SSEWriter, hb time.Duration) *sink {
	s := &sink{
		o:        &orderedWriter{w: w},
		hbDone:   make(chan struct{}),
		hbExited: make(chan struct{}),
	}
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
	for {
		select {
		case <-s.hbDone:
			return
		case <-t.C:
			if heartbeatTickHook != nil {
				heartbeatTickHook()
			}
			spinTick++
			dots := strings.Repeat(".", 1+(spinTick-1)%3)
			// replace_response overwrites the prior frame so the
			// dots animate in place rather than accumulating. We use
			// replace (not text-append) for keepalive too: text events
			// would *append* each tick's payload, which Poe's Markdown
			// renderer then sees as leading content and can mis-parse
			// subsequent headings, lists or fenced blocks emitted by
			// the agent. Replace + spinner doubles as user-visible
			// liveness AND keepalive, so one path covers both.
			gateOpen, _ := s.o.hbReplace("> _Thinking" + dots + "_")
			if !gateOpen {
				// A user write has landed (or the stream is closed):
				// any further tick would be a wasted mutex acquire.
				// Self-disarm.
				return
			}
		}
	}
}

// stop wakes the heartbeat goroutine for prompt shutdown. Idempotent.
// Safe to call any number of times from any goroutine. Even if never
// called, the heartbeat self-disarms via the orderedWriter gate on its
// next tick after the first user write.
func (s *sink) stop() {
	s.hbStop.Do(func() { close(s.hbDone) })
}

// FirstChunk — router calls this on the first real agent chunk.
// Optimisation only: prompts heartbeat shutdown so the goroutine
// doesn't sit until the next tick before exiting. Correctness comes
// from orderedWriter, not from this call.
func (s *sink) FirstChunk() { s.stop() }

func (s *sink) Text(t string) error      { return s.o.userText(t) }
func (s *sink) Replace(t string) error   { return s.o.userReplace(t) }
func (s *sink) Error(t, et string) error { s.stop(); return s.o.userError(t, et) }
func (s *sink) Done() error              { s.stop(); return s.o.userDone() }

// handleAuth runs an auth-flow turn end-to-end on the SSE stream. Always
// emits a single text payload + done, regardless of broker outcome.
func (h *Handler) handleAuth(ctx context.Context, sse *poeproto.SSEWriter, convID, text string) {
	out, err := h.cfg.AuthBroker.Handle(ctx, convID, text)
	if err != nil {
		log.Printf("authbroker (conv=%s): %v", convID, err)
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
	if debuglog.Enabled() {
		debuglog.Logf("report_reaction: conv=%q user=%q msg=%q reaction=%q action=%q",
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

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

	"github.com/kfet/poe-acp-relay/internal/authbroker"
	"github.com/kfet/poe-acp-relay/internal/poeproto"
	"github.com/kfet/poe-acp-relay/internal/router"
)

// Config configures a Handler.
type Config struct {
	Router *router.Router
	// Settings is the static response for `settings` requests. Parameter
	// controls may be overridden per-request by ParameterControlsProvider.
	Settings poeproto.SettingsResponse
	// HeartbeatInterval is the SSE heartbeat tick while waiting for the
	// first agent chunk. <=0 disables the heartbeat.
	HeartbeatInterval time.Duration
	// SpinnerInterval overrides the tick rate when hide_thinking=true,
	// so the animated "Thinking…" spinner cycles at a human-readable
	// pace regardless of the heartbeat interval. <=0 falls back to
	// HeartbeatInterval.
	SpinnerInterval time.Duration
	// ParameterControlsProvider, if set, is called on each `settings`
	// request to populate SettingsResponse.ParameterControls. If nil,
	// Settings.ParameterControls is used as-is.
	ParameterControlsProvider func() *poeproto.ParameterControls
	// AuthBroker, if set, intercepts /login commands and pasted redirect
	// URLs from in-flight logins before they reach the router. Optional;
	// nil disables interactive auth.
	AuthBroker *authbroker.Broker
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

	case poeproto.TypeReportFeedback, poeproto.TypeReportReaction, poeproto.TypeReportError:
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
		t := router.Turn{Role: m.Role, Content: m.Content}
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

	// Sink: SSE writer + heartbeat coordination + disconnect → cancel.
	// When hide_thinking is on, swap the tick for SpinnerInterval so the
	// spinner animates at a human-readable pace regardless of the
	// configured heartbeat interval.
	tick := h.cfg.HeartbeatInterval
	if opts.HideThinking && tick > 0 && h.cfg.SpinnerInterval > 0 {
		tick = h.cfg.SpinnerInterval
	}
	s := newSink(sse, tick, opts.HideThinking)
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

// sink adapts SSEWriter to router.ChunkSink, with a "still working…"
// heartbeat that stops as soon as the first real chunk arrives. When
// hideThinking is set the heartbeat doubles as a visible animated
// "Thinking…" indicator (rendered via replace_response so each tick
// overwrites the previous one), since the user has opted out of seeing
// the agent's actual thought stream and would otherwise stare at a
// blank reply for the duration of the thinking phase.
type sink struct {
	w            *poeproto.SSEWriter
	hideThinking bool

	mu       sync.Mutex
	started  bool
	spinTick int  // number of spinner frames emitted so far
	cleared  bool // spinner has been replaced with empty body
	stopped  atomic.Bool
	hbDone   chan struct{}
}

func newSink(w *poeproto.SSEWriter, hb time.Duration, hideThinking bool) *sink {
	s := &sink{w: w, hideThinking: hideThinking, hbDone: make(chan struct{})}
	if hb > 0 {
		go s.heartbeat(hb)
	} else {
		// Heartbeat disabled: mark as already-stopped so stop()/FirstChunk()
		// are no-ops and don't double-close the channel.
		s.stopped.Store(true)
		close(s.hbDone)
	}
	return s
}

func (s *sink) heartbeat(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-s.hbDone:
			return
		case <-t.C:
			s.mu.Lock()
			if s.started || s.stopped.Load() {
				s.mu.Unlock()
				return
			}
			if s.hideThinking {
				s.spinTick++
				dots := strings.Repeat(".", 1+(s.spinTick-1)%3)
				// replace_response overwrites the prior frame so the
				// dots animate in place rather than accumulating.
				_ = s.w.Replace("> _Thinking" + dots + "_")
			} else {
				// Zero-width space keeps the SSE stream alive without
				// polluting the final rendered response.
				_ = s.w.Text("\u200b")
			}
			s.mu.Unlock()
		}
	}
}

// stop halts the heartbeat goroutine. If a thinking spinner was emitted,
// it is cleared with an empty replace_response so the upcoming real
// content (or error/done) starts from a blank slate.
func (s *sink) stop() {
	if !s.stopped.CompareAndSwap(false, true) {
		return
	}
	close(s.hbDone)
	s.mu.Lock()
	wasSpinning := s.hideThinking && s.spinTick > 0 && !s.cleared
	s.cleared = true
	s.mu.Unlock()
	if wasSpinning {
		_ = s.w.Replace("")
	}
}

// FirstChunk — router calls this on the first real agent chunk.
func (s *sink) FirstChunk() {
	s.mu.Lock()
	s.started = true
	s.mu.Unlock()
	s.stop()
}

func (s *sink) Text(t string) error      { return s.w.Text(t) }
func (s *sink) Replace(t string) error   { return s.w.Replace(t) }
func (s *sink) Error(t, et string) error { s.stop(); return s.w.Error(t, et) }
func (s *sink) Done() error              { s.stop(); return s.w.Done() }

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

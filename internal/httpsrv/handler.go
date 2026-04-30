// Package httpsrv wires Poe HTTP requests into the router.
package httpsrv

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

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
	// ParameterControlsProvider, if set, is called on each `settings`
	// request to populate SettingsResponse.ParameterControls. If nil,
	// Settings.ParameterControls is used as-is.
	ParameterControlsProvider func() *poeproto.ParameterControls
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
		turns = append(turns, router.Turn{Role: m.Role, Content: m.Content})
	}

	opts := router.ParseOptions(req.LatestParameters(), h.cfg.Router.Defaults())

	// Sink: SSE writer + heartbeat coordination + disconnect → cancel.
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

// sink adapts SSEWriter to router.ChunkSink, with a "still working…"
// heartbeat that stops as soon as the first real chunk arrives.
type sink struct {
	w *poeproto.SSEWriter

	mu      sync.Mutex
	started bool
	stopped atomic.Bool
	hbDone  chan struct{}
}

func newSink(w *poeproto.SSEWriter, hb time.Duration) *sink {
	s := &sink{w: w, hbDone: make(chan struct{})}
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
			started := s.started
			s.mu.Unlock()
			if started {
				return
			}
			// Zero-width space keeps the SSE stream alive without
			// polluting the final rendered response.
			_ = s.w.Text("\u200b")
		}
	}
}

func (s *sink) stop() {
	if s.stopped.CompareAndSwap(false, true) {
		close(s.hbDone)
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
func (s *sink) Error(t, et string) error { return s.w.Error(t, et) }
func (s *sink) Done() error              { return s.w.Done() }

// Package httpsrv wires Poe HTTP requests into the router.
package httpsrv

import (
	"context"
	"encoding/json"
	"log"
	"net/http"

	"github.com/kfet/fir/internal/poeacp/poeproto"
	"github.com/kfet/fir/internal/poeacp/router"
)

// Handler is the /poe HTTP handler.
type Handler struct {
	Router *router.Router

	// Settings returned for `settings` requests.
	Settings poeproto.SettingsResponse
}

// sseSink adapts a SSEWriter to router.ChunkSink.
type sseSink struct{ w *poeproto.SSEWriter }

func (s sseSink) Text(t string) error               { return s.w.Text(t) }
func (s sseSink) Replace(t string) error            { return s.w.Replace(t) }
func (s sseSink) Error(t, et string) error          { return s.w.Error(t, et) }
func (s sseSink) Done() error                       { return s.w.Done() }

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
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(h.Settings)
		return

	case poeproto.TypeQuery:
		h.handleQuery(r.Context(), w, req)
		return

	case poeproto.TypeReportFeedback, poeproto.TypeReportReaction, poeproto.TypeReportError:
		// v1: accept and drop.
		w.WriteHeader(http.StatusOK)
		return

	default:
		http.Error(w, "unknown request type: "+req.Type, http.StatusBadRequest)
	}
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

	text := req.LatestUserText()
	if text == "" {
		_ = sse.Error("empty user message", "user_caused_error")
		_ = sse.Done()
		return
	}

	if err := h.Router.Prompt(ctx, req.ConversationID, req.UserID, text, sseSink{w: sse}); err != nil {
		log.Printf("router prompt (conv=%s): %v", req.ConversationID, err)
	}
}

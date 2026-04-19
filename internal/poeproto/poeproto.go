// Package poeproto is a minimal subset of the Poe server-bot protocol
// needed by the ACP relay: request decoding, SSE response writing, and
// bearer auth. Intentionally small and self-contained.
package poeproto

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// RequestType values Poe sends.
const (
	TypeQuery          = "query"
	TypeSettings       = "settings"
	TypeReportFeedback = "report_feedback"
	TypeReportReaction = "report_reaction"
	TypeReportError    = "report_error"
)

// Message is one turn in a Poe query.
type Message struct {
	Role        string `json:"role"`
	Content     string `json:"content"`
	ContentType string `json:"content_type,omitempty"`
	MessageID   string `json:"message_id,omitempty"`
}

// Request is the shape of an inbound Poe POST.
type Request struct {
	Type           string    `json:"type"`
	Query          []Message `json:"query,omitempty"`
	MessageID      string    `json:"message_id,omitempty"`
	UserID         string    `json:"user_id,omitempty"`
	ConversationID string    `json:"conversation_id,omitempty"`
}

// LatestUserText returns the content of the last `user` message in the query.
func (r *Request) LatestUserText() string {
	for i := len(r.Query) - 1; i >= 0; i-- {
		if r.Query[i].Role == "user" {
			return r.Query[i].Content
		}
	}
	return ""
}

// Decode parses a Poe request from the HTTP body.
func Decode(body io.Reader) (*Request, error) {
	var req Request
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		return nil, fmt.Errorf("decode poe request: %w", err)
	}
	if req.Type == "" {
		return nil, fmt.Errorf("poe request missing type")
	}
	return &req, nil
}

// SettingsResponse is the JSON returned for a `settings` request.
//
// `Commands` is a list of slash-command names surfaced in the Poe UI
// autocomplete menu.
type SettingsResponse struct {
	AllowAttachments    bool     `json:"allow_attachments"`
	IntroductionMessage string   `json:"introduction_message,omitempty"`
	Commands            []string `json:"commands,omitempty"`
}

// SSEWriter streams SSE events to an open HTTP response.
type SSEWriter struct {
	mu     sync.Mutex
	w      http.ResponseWriter
	f      http.Flusher
	closed bool
}

// NewSSEWriter prepares headers and returns a writer. Caller must call
// Done() (or Error+Done) to complete the response.
func NewSSEWriter(w http.ResponseWriter) (*SSEWriter, error) {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("response writer does not support flushing")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	return &SSEWriter{w: w, f: f}, nil
}

// event writes one SSE event frame.
func (s *SSEWriter) event(name string, data any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("sse: closed")
	}
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, b); err != nil {
		return err
	}
	s.f.Flush()
	return nil
}

// Meta sends the mandatory initial `meta` event.
func (s *SSEWriter) Meta() error {
	return s.event("meta", map[string]any{"content_type": "text/markdown"})
}

// Text appends a text chunk.
func (s *SSEWriter) Text(chunk string) error {
	if chunk == "" {
		return nil
	}
	return s.event("text", map[string]any{"text": chunk})
}

// Replace replaces the entire response with chunk.
func (s *SSEWriter) Replace(chunk string) error {
	return s.event("replace_response", map[string]any{"text": chunk})
}

// Error emits an error event.
func (s *SSEWriter) Error(text, errorType string) error {
	data := map[string]any{"allow_retry": true, "text": text}
	if errorType != "" {
		data["error_type"] = errorType
	}
	return s.event("error", data)
}

// Done terminates the stream with a `done` event.
func (s *SSEWriter) Done() error {
	err := s.event("done", map[string]any{})
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return err
}

// BearerAuth returns middleware that enforces a bearer token.
func BearerAuth(secret string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		h := r.Header.Get("Authorization")
		if secret == "" || len(h) < len(prefix) || h[:len(prefix)] != prefix || h[len(prefix):] != secret {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

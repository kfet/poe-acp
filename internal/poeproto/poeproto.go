// Package poeproto is a minimal subset of the Poe server-bot protocol
// needed by the ACP relay: request decoding, SSE response writing, and
// bearer auth. Intentionally small and self-contained.
package poeproto

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/kfet/poe-acp/internal/debuglog"
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
	Role        string         `json:"role"`
	Content     string         `json:"content"`
	ContentType string         `json:"content_type,omitempty"`
	MessageID   string         `json:"message_id,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
	Attachments []Attachment   `json:"attachments,omitempty"`
}

// Attachment is a file attached to a Poe message. Poe serves these as
// signed public URLs; the relay forwards them to the agent as ACP
// content blocks. Text-ish attachments where Poe has computed
// `parsed_content` (because we set `expand_text_attachments=true`) are
// preferred over a bare URL link, because they avoid an agent-side
// fetch round-trip.
type Attachment struct {
	URL           string `json:"url"`
	ContentType   string `json:"content_type,omitempty"`
	Name          string `json:"name,omitempty"`
	ParsedContent string `json:"parsed_content,omitempty"`
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
	var (
		req Request
		raw []byte
	)
	if debuglog.Enabled() {
		// Tee the body so we can log the raw JSON exactly as Poe sent
		// it. Capped at 16 KiB to avoid allocating huge buffers on
		// requests with large parsed_content attachment payloads.
		buf, err := io.ReadAll(io.LimitReader(body, 16*1024))
		if err != nil {
			return nil, fmt.Errorf("read poe body: %w", err)
		}
		raw = buf
		body = bytes.NewReader(buf)
	}
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		return nil, fmt.Errorf("decode poe request: %w", err)
	}
	if req.Type == "" {
		return nil, fmt.Errorf("poe request missing type")
	}
	if debuglog.Enabled() {
		debuglog.Logf("raw poe body (%d bytes, type=%s): %s", len(raw), req.Type, string(raw))
	}
	return &req, nil
}

// SettingsResponse is the JSON returned for a `settings` request.
//
// ResponseVersion gates which Poe protocol features are honoured. Per
// fastapi_poe.types.SettingsResponse: "If not provided, Poe will use
// the default values for response version 0." Response version 0 does
// not honour `parameter_controls`. We always emit 2.
type SettingsResponse struct {
	ResponseVersion       int                `json:"response_version"`
	AllowAttachments      bool               `json:"allow_attachments"`
	ExpandTextAttachments bool               `json:"expand_text_attachments"`
	IntroductionMessage   string             `json:"introduction_message,omitempty"`
	ParameterControls     *ParameterControls `json:"parameter_controls,omitempty"`
}

// SettingsResponseVersion is the only response_version this relay
// emits. Required for `parameter_controls` to be honoured by Poe.
const SettingsResponseVersion = 2

// LatestParameters returns the parameters dict from the most recent
// user message in the query, or nil if absent.
func (r *Request) LatestParameters() map[string]any {
	for i := len(r.Query) - 1; i >= 0; i-- {
		if r.Query[i].Role == "user" {
			return r.Query[i].Parameters
		}
	}
	return nil
}

// ParameterControls is the schema returned in SettingsResponse.parameter_controls.
// It tells Poe what UI controls to render for the bot.
//
// APIVersion is required by Poe and must be "2"; older values are rejected
// silently (the whole parameter_controls object is dropped). See
// fastapi_poe.types.ParameterControls.
type ParameterControls struct {
	APIVersion string    `json:"api_version"`
	Sections   []Section `json:"sections"`
}

// ParameterControlsAPIVersion is the only api_version Poe currently accepts
// for parameter_controls. Pinned per fastapi_poe.types.ParameterControls
// (Literal["2"]).
const ParameterControlsAPIVersion = "2"

// Section groups a set of controls under a heading. A section must
// contain controls (we don't use tabs in v1).
type Section struct {
	Name               string    `json:"name"`
	CollapsedByDefault bool      `json:"collapsed_by_default,omitempty"`
	Controls           []Control `json:"controls"`
}

// Control is one renderable UI element. It is a tagged union over the
// `control` field; we only emit `drop_down` and `toggle_switch` in v1.
//
// Wire values for Control.Control match fastapi_poe.types literals
// exactly: "drop_down" (NOT "dropdown") and "toggle_switch". Poe's
// validator runs Pydantic with extra="forbid" and silently drops the
// whole parameter_controls object on any mismatch.
type Control struct {
	Control       string          `json:"control"` // "drop_down" | "toggle_switch"
	Label         string          `json:"label"`
	ParameterName string          `json:"parameter_name"`
	Description   string          `json:"description,omitempty"`
	DefaultValue  any             `json:"default_value,omitempty"`
	Options       []ValueNamePair `json:"options,omitempty"` // drop_down only
}

// ValueNamePair is a dropdown option: `value` is what arrives in
// `parameters`, `name` is the user-facing label.
type ValueNamePair struct {
	Value string `json:"value"`
	Name  string `json:"name"`
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

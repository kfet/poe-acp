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

	kitlog "github.com/kfet/acp-kit/log"
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

// ReactionAction is the normalised add/remove polarity of a reaction
// event. Empty string is treated as "added" by default — Poe's earliest
// payload shape only had a single `reaction` field with no explicit
// action and represented adds.
type ReactionAction string

const (
	ReactionAdded   ReactionAction = "added"
	ReactionRemoved ReactionAction = "removed"
)

// Request is the shape of an inbound Poe POST.
type Request struct {
	Type           string    `json:"type"`
	Query          []Message `json:"query,omitempty"`
	MessageID      string    `json:"message_id,omitempty"`
	UserID         string    `json:"user_id,omitempty"`
	ConversationID string    `json:"conversation_id,omitempty"`

	// Reaction fields populated only when Type == TypeReportReaction.
	// Poe has shipped at least two payload shapes for this event:
	//
	//   (a) single field, prefix-encoded sign:
	//         {"reaction":"like"} | {"reaction":"+👍"} | {"reaction":"-👍"}
	//
	//   (b) split fields:
	//         {"reaction":"👍","action":"added"|"removed"}
	//
	// Decode() normalises both into Reaction (emoji/kind, with any
	// '+'/'-' prefix stripped) + ReactionAction (added|removed). The
	// raw bytes are logged at debug level so the wire shape stays
	// visible in prod even after normalisation.
	Reaction       string         `json:"reaction,omitempty"`
	ReactionAction ReactionAction `json:"action,omitempty"`
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
		req   Request
		tee   *capWriter
		debug = kitlog.Enabled()
	)
	if debug {
		// Tee the body into a capped buffer so we can log the head of
		// the raw JSON exactly as Poe sent it, without holding the
		// full request in memory — large parsed_content payloads on
		// PDF / transcript / image attachments can run well past tens
		// of MiB. The decoder still streams from the original body.
		tee = &capWriter{max: 16 * 1024}
		body = io.TeeReader(body, tee)
	}
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		return nil, fmt.Errorf("decode poe request: %w", err)
	}
	if req.Type == "" {
		return nil, fmt.Errorf("poe request missing type")
	}
	if debug {
		suffix := ""
		if tee.truncated {
			suffix = "...[truncated]"
		}
		kitlog.Debugf("raw poe body (type=%s, head=%dB): %s%s", req.Type, len(tee.buf), string(tee.buf), suffix)
	}
	if req.Type == TypeReportReaction {
		req.Reaction, req.ReactionAction = normaliseReaction(req.Reaction, req.ReactionAction)
	}
	return &req, nil
}

// normaliseReaction collapses the two known Poe reaction payload shapes
// into (kind, added|removed). When action is non-empty, it wins. When
// action is empty, a leading '+' / '-' on the reaction string encodes
// the polarity; otherwise the event is treated as an add.
func normaliseReaction(reaction string, action ReactionAction) (string, ReactionAction) {
	switch action {
	case ReactionAdded, ReactionRemoved:
		return reaction, action
	}
	if len(reaction) > 0 {
		switch reaction[0] {
		case '+':
			return reaction[1:], ReactionAdded
		case '-':
			return reaction[1:], ReactionRemoved
		}
	}
	return reaction, ReactionAdded
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
// `control` field; we emit `drop_down`, `toggle_switch`, and
// `condition` (ConditionallyRenderControls).
//
// Wire values for Control.Control match fastapi_poe.types literals
// exactly: "drop_down" (NOT "dropdown"), "toggle_switch", and
// "condition". Poe's validator runs Pydantic with extra="forbid" and
// silently drops the whole parameter_controls object on any mismatch.
//
// Field applicability by control kind (all extra fields are
// json:"omitempty" so a single Go struct covers every kind without
// emitting forbidden keys):
//
//	drop_down      : Label, ParameterName, Options, DefaultValue
//	toggle_switch  : Label, ParameterName, DefaultValue
//	condition      : Condition, Controls
type Control struct {
	Control       string          `json:"control"`
	Label         string          `json:"label,omitempty"`
	ParameterName string          `json:"parameter_name,omitempty"`
	Description   string          `json:"description,omitempty"`
	DefaultValue  any             `json:"default_value,omitempty"`
	Options       []ValueNamePair `json:"options,omitempty"` // drop_down only

	// condition (ConditionallyRenderControls) only.
	Condition *Condition `json:"condition,omitempty"`
	Controls  []Control  `json:"controls,omitempty"`
}

// Condition is the per-`condition`-control comparator block. Matches
// fastapi_poe.types.ComparatorCondition.
type Condition struct {
	Comparator string           `json:"comparator"` // "eq" | "ne" | "gt" | "ge" | "lt" | "le"
	Left       ConditionOperand `json:"left"`
	Right      ConditionOperand `json:"right"`
}

// ConditionOperand is the LiteralValue|ParameterValue anyOf. Exactly
// one of Literal or ParameterName must be set on the wire; both are
// omitempty so the chosen variant matches the upstream schema's
// additionalProperties:false discriminator.
type ConditionOperand struct {
	Literal       any    `json:"literal,omitempty"`
	ParameterName string `json:"parameter_name,omitempty"`
}

// LiteralOperand builds a LiteralValue operand.
func LiteralOperand(v any) ConditionOperand { return ConditionOperand{Literal: v} }

// ParamOperand builds a ParameterValue operand referencing another
// control's parameter_name.
func ParamOperand(name string) ConditionOperand { return ConditionOperand{ParameterName: name} }

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
	// Defeat response buffering in any intermediary proxy (Tailscale
	// Funnel, nginx, …). Without this a small first event can sit in the
	// proxy's buffer and never reach Poe, which then drops the bot
	// connection within ~15ms (the "fast client disconnect"). nginx and
	// several proxies honour X-Accel-Buffering: no by disabling buffering
	// for this response. See also Preamble.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	return &SSEWriter{w: w, f: f}, nil
}

// preamblePadding is the byte length of the padding written by Preamble.
// A single ~50-byte meta event can sit in an intermediary proxy's
// response buffer and never be forwarded to Poe until more bytes
// accumulate — so Poe sees no first byte and drops the connection
// ~11–18ms in (logged as "fast client disconnect"). 4KB exceeds the
// common proxy first-buffer threshold (nginx proxy_buffer_size defaults
// to 4–8KB), forcing an immediate flush to the client. SSE comment lines
// (": …") are ignored by all conformant clients, so the padding is
// invisible and cannot corrupt the stream.
const preamblePadding = 4096

// preambleFrame is the constant padded SSE comment frame, built once.
// ": " + padding spaces + "\n\n".
var preambleFrame = func() []byte {
	frame := make([]byte, 0, preamblePadding+4)
	frame = append(frame, ':', ' ')
	frame = append(frame, bytes.Repeat([]byte{' '}, preamblePadding)...)
	frame = append(frame, '\n', '\n')
	return frame
}()

// Preamble writes a padded SSE comment frame and flushes it immediately,
// forcing any buffering proxy between the relay and Poe to forward the
// first bytes right away — before the ~400ms session resume. Call once,
// before Meta. A comment frame changes nothing in the event sequence that
// follows.
func (s *SSEWriter) Preamble() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("sse: closed")
	}
	if _, err := s.w.Write(preambleFrame); err != nil {
		return err
	}
	s.f.Flush()
	return nil
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

// File advertises an output attachment to the client. The url is the
// Poe-served download URL returned by the attachment upload; name is
// the filename shown to the user; contentType is its MIME type. A
// non-empty inlineRef makes Poe render the attachment inline when the
// streamed markdown references it via ![title][inlineRef].
func (s *SSEWriter) File(url, contentType, name, inlineRef string) error {
	// inline_ref must be JSON null (not "") for a plain downloadable
	// attachment. An empty-but-present string makes Poe treat the file
	// as inline with an empty reference key, rendering a degenerate
	// "[]: <url>" markdown link-reference instead of an attachment chip.
	var ref any
	if inlineRef != "" {
		ref = inlineRef
	}
	return s.event("file", map[string]any{
		"url":          url,
		"content_type": contentType,
		"name":         name,
		"inline_ref":   ref,
	})
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

// capWriter accumulates up to max bytes for debug-logging the head of a
// streamed body. Once full, further writes are discarded and truncated
// is set. It implements io.Writer so it can sit on the receive side of
// an io.TeeReader without bounding what the decoder reads.
type capWriter struct {
	buf       []byte
	max       int
	truncated bool
}

func (c *capWriter) Write(p []byte) (int, error) {
	if room := c.max - len(c.buf); room > 0 {
		c.buf = append(c.buf, p[:min(len(p), room)]...)
	}
	if len(p) > 0 && len(c.buf) >= c.max {
		c.truncated = true
	}
	return len(p), nil
}

// Package router maps Poe conversation_ids to ACP sessions and owns their
// lifecycle (lazy create / resume, per-conv cwd, serial-per-conv prompting,
// idle GC).
package router

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/poe-acp-relay/internal/acpclient"
	"github.com/kfet/poe-acp-relay/internal/debuglog"
)

// ChunkSink is the interface the HTTP/SSE layer implements to receive
// assistant output chunks for a single Poe query.
type ChunkSink interface {
	Text(s string) error
	Replace(s string) error
	Error(text, errorType string) error
	Done() error
	// FirstChunk is called by the router once, the first time the sink
	// receives a non-empty update from the agent. Used by the HTTP layer
	// to stop the "still thinking…" heartbeat.
	FirstChunk()
}

// Turn is one message in a Poe query, decoupled from the wire type so
// the router doesn't depend on poeproto.
type Turn struct {
	Role    string // "user", "bot", or "system"
	Content string
	// MessageID is Poe's per-turn id (unique within a query). The router
	// uses the latest user turn's MessageID to scope downloaded attachments
	// into a per-message subdir under the conv cwd. Empty is tolerated.
	MessageID   string
	Attachments []Attachment
}

// Attachment is a file attached to a Poe message. The router forwards
// these to the agent as ACP content blocks alongside the text prompt.
// Poe URLs are public-signed so no fetch happens on the relay side.
//
// When ParsedContent is non-empty AND the agent advertises
// promptCapabilities.embeddedContext, the relay emits a
// ContentBlock::Resource (TextResourceContents) so the agent has the
// text inline without a fetch round-trip. Otherwise the relay falls
// back to a ResourceLink block (the mandatory ACP baseline that all
// agents support).
type Attachment struct {
	URL           string
	ContentType   string
	Name          string
	ParsedContent string
}

// Agent is the subset of acpclient.AgentProc the router needs. Exposed
// as an interface for testability.
type Agent interface {
	Caps() acpclient.Caps
	NewSession(ctx context.Context, cwd string, sink acpclient.SessionUpdateSink, systemPromptBlocks []acp.ContentBlock) (acp.SessionId, error)
	ListSessions(ctx context.Context, cwd string) ([]acpclient.SessionInfo, error)
	ResumeSession(ctx context.Context, cwd string, sid acp.SessionId, sink acpclient.SessionUpdateSink) error
	Prompt(ctx context.Context, sid acp.SessionId, prompt []acp.ContentBlock) (acp.StopReason, error)
	Cancel(ctx context.Context, sid acp.SessionId) error
	SetModel(ctx context.Context, sid acp.SessionId, modelID string) error
	SetConfigOption(ctx context.Context, sid acp.SessionId, configID, value string) error
}

// Options is the per-prompt set of user-selected parameter values
// extracted from Poe's `parameters` dict.
type Options struct {
	Model        string // "" = leave as-is
	Thinking     string // "" = leave as-is; one of "off","minimal","low","medium","high","xhigh","max"
	HideThinking bool   // suppress agent_thought_chunk in the SSE stream
}

// Config configures a Router.
type Config struct {
	// Agent drives sessions. *acpclient.AgentProc satisfies this.
	Agent Agent
	// StateDir is the root for per-conv working dirs. Each conv gets
	// StateDir/convs/<conv_id>/ as its cwd.
	StateDir string
	// SessionTTL: sessions idle longer than this are dropped from the map.
	SessionTTL time.Duration
	// Defaults are the values the Poe UI shows when the user hasn't
	// touched the Options panel. ParseOptions overlays user-supplied
	// keys on top of these so the agent always converges to the
	// UI-promised state, even on the first turn when Poe sends an
	// empty `parameters` dict.
	Defaults Options
	// SystemPrompt, if non-empty, is the durable system-prompt text the
	// router injects into every new session. When the agent advertises
	// the session.systemPrompt capability the text is sent via
	// session/new._meta as a single text ContentBlock; otherwise it is
	// prepended to the first session/prompt with a "preserve verbatim"
	// instruction. See acp-spec/rfd-system-prompt.md and
	// docs/skill-injection-plan.md.
	SystemPrompt string
	// Now overrides the clock for tests. Defaults to time.Now.
	Now func() time.Time
	// HTTPClient is used to fetch attachment bytes (download-to-disk
	// path, plus the inline ImageBlock for vision-capable models).
	// Defaults to http.DefaultClient.
	HTTPClient *http.Client
	// MaxInlineImageBytes caps the raw byte size of an image attachment
	// that the relay will base64-encode into an additive ImageBlock
	// alongside the file:// ResourceLink. Anything larger gets the
	// link-only treatment. Zero falls back to defaultMaxInlineImageBytes
	// (3 MiB — leaves headroom for base64 overhead under the tightest
	// provider cap, Bedrock-Anthropic at 3.75 MB).
	MaxInlineImageBytes int64
	// MaxAttachmentBytes caps how many bytes the relay will download to
	// disk per attachment. Files exceeding the cap are skipped (logged
	// and dropped from the prompt). Zero falls back to
	// defaultMaxAttachmentBytes (100 MiB).
	MaxAttachmentBytes int64
	// AttachmentTTL is how long downloaded attachment files persist on
	// disk before the GC sweep deletes them. Zero falls back to
	// defaultAttachmentTTL (30 days). The router clamps AttachmentTTL
	// to be no shorter than SessionTTL with a warn log so that a live
	// resumed session never points at a swept file.
	AttachmentTTL time.Duration
}

// Router is the conv_id → session map.
type Router struct {
	cfg Config

	mu       sync.Mutex
	sessions map[string]*sessionState
}

// sessionState tracks one conv_id.
type sessionState struct {
	convID    string
	userID    string
	sessionID acp.SessionId
	cwd       string

	// turnMu serialises prompts for this conv. Held for the whole turn.
	turnMu sync.Mutex
	// inUse is non-zero while a prompt is in flight; GC skips such
	// sessions so a long generation isn't evicted out from under itself.
	// Protected by Router.mu (read by gcOnce, written by Prompt).
	inUse int

	// sinkMu guards sink (written by Prompt, read by OnUpdate).
	sinkMu       sync.Mutex
	sink         ChunkSink
	first        bool
	hideThinking bool
	// chunkMode tracks the kind of stream currently being emitted,
	// so multi-chunk thoughts render as one continuous block.
	// Reset to chunkNone at the start of each turn.
	chunkMode chunkKind

	// applied tracks the last successfully-applied agent options, so
	// we only call set_model / set_config_option when values change.
	// Protected by turnMu.
	applied Options

	lastUsedNs int64 // protected by Router.mu

	// pendingSystemPromptInline, when true, causes the next Prompt to
	// prepend the router's SystemPrompt text to the first content
	// block. Used on the fallback path (agent didn't advertise the
	// session.systemPrompt cap) for fresh sessions and on resume.
	// Protected by turnMu.
	pendingSystemPromptInline bool
}

// chunkKind classifies the most recent stream chunk written to the sink.
type chunkKind int

const (
	chunkNone    chunkKind = iota // no chunks yet this turn
	chunkMessage                  // last chunk was an AgentMessageChunk
	chunkThought                  // last chunk was an AgentThoughtChunk
)

// New creates a router.
func New(cfg Config) (*Router, error) {
	if cfg.Agent == nil {
		return nil, fmt.Errorf("router: nil Agent")
	}
	if cfg.StateDir == "" {
		return nil, fmt.Errorf("router: empty StateDir")
	}
	if cfg.SessionTTL == 0 {
		cfg.SessionTTL = 2 * time.Hour
	}
	if cfg.AttachmentTTL == 0 {
		cfg.AttachmentTTL = defaultAttachmentTTL
	}
	if cfg.AttachmentTTL < cfg.SessionTTL {
		// A live resumed session must never reference a swept file.
		log.Printf("router: AttachmentTTL=%s < SessionTTL=%s; clamping AttachmentTTL up to SessionTTL",
			cfg.AttachmentTTL, cfg.SessionTTL)
		cfg.AttachmentTTL = cfg.SessionTTL
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if err := os.MkdirAll(filepath.Join(cfg.StateDir, "convs"), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir state: %w", err)
	}
	return &Router{cfg: cfg, sessions: make(map[string]*sessionState)}, nil
}

// OnUpdate implements acpclient.SessionUpdateSink; forwards to the current
// sink (if one is attached). Emits transition markers between thought and
// message streams so multi-chunk thoughts render as one Markdown block
// quote.
func (s *sessionState) OnUpdate(_ context.Context, n acp.SessionNotification) error {
	u := n.Update
	var (
		kind chunkKind
		text string
	)
	switch {
	case u.AgentMessageChunk != nil && u.AgentMessageChunk.Content.Text != nil:
		kind, text = chunkMessage, u.AgentMessageChunk.Content.Text.Text
	case u.AgentThoughtChunk != nil && u.AgentThoughtChunk.Content.Text != nil:
		kind, text = chunkThought, u.AgentThoughtChunk.Content.Text.Text
	default:
		// Tool-calls, plans, available_commands_update etc. are
		// suppressed in v1.
		return nil
	}
	if text == "" {
		return nil
	}

	s.sinkMu.Lock()
	sink := s.sink
	firstSent := s.first
	hideThinking := s.hideThinking
	prevMode := s.chunkMode
	if sink == nil {
		s.sinkMu.Unlock()
		return nil
	}
	if kind == chunkThought && hideThinking {
		s.sinkMu.Unlock()
		return nil
	}
	// Compute the transition prefix (if any) before unlocking.
	var prefix string
	switch {
	case prevMode == chunkNone && kind == chunkThought:
		prefix = "> _Thinking…_\n> "
	case prevMode == chunkMessage && kind == chunkThought:
		prefix = "\n\n> _Thinking…_\n> "
	case prevMode == chunkThought && kind == chunkMessage:
		prefix = "\n\n"
	}
	s.chunkMode = kind
	if !firstSent {
		s.first = true
	}
	s.sinkMu.Unlock()

	if !firstSent {
		sink.FirstChunk()
	}
	if kind == chunkThought {
		// Continue the blockquote across newlines inside the thought
		// chunk so Poe's Markdown renderer keeps it as one block.
		text = strings.ReplaceAll(text, "\n", "\n> ")
	}
	return sink.Text(prefix + text)
}

// Prompt handles one Poe query end-to-end. Serialises per-conv. The query
// slice is the full Poe-supplied conversation (including prior turns); the
// router uses only the latest user turn on the hot path, but seeds the
// agent with the full transcript on a cold path that can't resume.
func (r *Router) Prompt(ctx context.Context, convID, userID string, query []Turn, opts Options, sink ChunkSink) error {
	if convID == "" {
		convID = "default"
	}
	latest := latestUserText(query)
	if latest == "" {
		_ = sink.Error("empty user message", "user_caused_error")
		_ = sink.Done()
		return fmt.Errorf("empty user message")
	}

	st, freshSeed, err := r.getOrCreate(ctx, convID, userID, query)
	if err != nil {
		_ = sink.Error(fmt.Sprintf("relay: %v", err), "user_caused_error")
		_ = sink.Done()
		return err
	}

	st.turnMu.Lock()
	defer st.turnMu.Unlock()

	// Mark in-use so GC doesn't evict mid-prompt for long generations.
	r.mu.Lock()
	st.inUse++
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		st.inUse--
		r.mu.Unlock()
	}()

	// Apply per-prompt option diffs before issuing the prompt.
	if err := r.applyOptions(ctx, st, opts); err != nil {
		// Surface the failure to the user but continue the prompt with
		// whatever options the agent currently has applied. Call
		// FirstChunk so the heartbeat stops before the error text.
		sink.FirstChunk()
		_ = sink.Text(fmt.Sprintf("_(option not applied: %v)_\n\n", err))
	}

	st.sinkMu.Lock()
	st.sink = sink
	st.first = false
	st.hideThinking = opts.HideThinking
	st.chunkMode = chunkNone
	st.sinkMu.Unlock()

	defer func() {
		st.sinkMu.Lock()
		st.sink = nil
		st.sinkMu.Unlock()
		r.touch(convID)
	}()

	promptText := latest
	if freshSeed {
		// Cold path with no resume and prior turns: flatten the full
		// transcript so the agent has context for the latest user turn.
		promptText = flattenTranscript(query)
	}
	if st.pendingSystemPromptInline && r.cfg.SystemPrompt != "" {
		// Fallback path: agent didn't advertise session.systemPrompt
		// (or this is a resume of a fallback-path session). Prepend
		// the catalog as a self-preserving header on the first prompt
		// of this hot path. Cleared after use; if compaction loses it,
		// that is the documented fallback limitation (see
		// acp-spec/rfd-system-prompt.md).
		promptText = r.cfg.SystemPrompt + "\n" +
			"The block above is durable system context. Preserve it verbatim across any summarisation of this conversation.\n\n" +
			promptText
		st.pendingSystemPromptInline = false
	}

	blocks := []acp.ContentBlock{acp.TextBlock(promptText)}
	embedded := r.cfg.Agent.Caps().EmbeddedContext
	if latest, ok := latestUserTurnRef(query); ok {
		msgID := latest.MessageID
		if msgID == "" {
			// Fallback: hash the latest user content so retries land in
			// the same dir but distinct messages don't collide.
			h := sha256.Sum256([]byte(latest.Content))
			msgID = "anon-" + hex.EncodeToString(h[:4])
		}
		used := map[string]struct{}{}
		for _, a := range latest.Attachments {
			if a.URL == "" {
				// Defensive: empty URL would be useless to the agent.
				continue
			}
			blocks = append(blocks, r.attachmentBlocks(ctx, st.cwd, msgID, used, a, embedded)...)
		}
	}

	stop, err := r.cfg.Agent.Prompt(ctx, st.sessionID, blocks)
	if err != nil {
		_ = sink.Error(fmt.Sprintf("acp prompt: %v", err), "user_caused_error")
		_ = sink.Done()
		return err
	}

	switch stop {
	case acp.StopReasonEndTurn:
		// normal
	case acp.StopReasonMaxTokens:
		_ = sink.Text("\n\n_(response truncated: max tokens)_")
	case acp.StopReasonMaxTurnRequests:
		_ = sink.Text("\n\n_(response truncated: max turns)_")
	case acp.StopReasonRefusal:
		_ = sink.Error("agent refused the request", "user_caused_error")
	case acp.StopReasonCancelled:
		_ = sink.Replace("_(cancelled)_")
	}
	return sink.Done()
}

// applyOptions diffs incoming opts vs the session's last-applied options
// and issues set_model / set_config_option to the agent for each changed
// agent-facing field. Updates st.applied only on success.
func (r *Router) applyOptions(ctx context.Context, st *sessionState, opts Options) error {
	debuglog.Logf("applyOptions conv=%s sid=%s incoming={model=%q thinking=%q hide=%v} applied={model=%q thinking=%q}",
		st.convID, string(st.sessionID), opts.Model, opts.Thinking, opts.HideThinking,
		st.applied.Model, st.applied.Thinking)
	if opts.Model != "" && opts.Model != st.applied.Model {
		debuglog.Logf("  -> set_model %q (was %q)", opts.Model, st.applied.Model)
		if err := r.cfg.Agent.SetModel(ctx, st.sessionID, opts.Model); err != nil {
			return fmt.Errorf("set_model %s: %w", opts.Model, err)
		}
		st.applied.Model = opts.Model
	}
	if opts.Thinking != "" && opts.Thinking != st.applied.Thinking {
		debuglog.Logf("  -> set_config thinking_level=%q (was %q)", opts.Thinking, st.applied.Thinking)
		if err := r.cfg.Agent.SetConfigOption(ctx, st.sessionID, "thinking_level", opts.Thinking); err != nil {
			// Common case: the current model doesn't support the
			// requested thinking level (e.g. non-reasoning models
			// only accept "off"). The Poe dropdown advertises a
			// fixed set of levels, so this mismatch is expected.
			// Mark applied anyway to avoid re-attempting (and
			// re-nagging) on every subsequent prompt of this
			// session, and don't surface a user-visible notice.
			debuglog.Logf("  -> set_config thinking_level=%q rejected by agent: %v (suppressed)", opts.Thinking, err)
			st.applied.Thinking = opts.Thinking
		} else {
			st.applied.Thinking = opts.Thinking
		}
	}
	// HideThinking is applied via st.hideThinking under sinkMu in
	// Prompt; nothing to apply on the agent side.
	return nil
}

// ParseOptions extracts a strongly-typed Options struct from Poe's
// `parameters` dict, overlaying valid keys on top of defaults. Unknown
// keys and wrong-type values are silently dropped — Poe documents that
// other bots calling ours may inject arbitrary parameters, so this is
// untrusted input.
//
// Defaults matter because Poe materialises `default_value`s into the
// UI display only; an empty `parameters` dict on the first turn would
// otherwise leave the agent on its own internal default while the UI
// promises something else. Overlaying onto defaults keeps UI and agent
// in sync from turn 1.
func ParseOptions(params map[string]any, defaults Options) Options {
	o := defaults
	if v, ok := params["model"].(string); ok {
		o.Model = v
	}
	if v, ok := params["thinking"].(string); ok {
		switch v {
		case "off", "minimal", "low", "medium", "high", "xhigh", "max":
			o.Thinking = v
		}
	}
	if v, ok := params["hide_thinking"].(bool); ok {
		o.HideThinking = v
	}
	return o
}

// Defaults returns the per-conversation option defaults configured on
// this router. The HTTP layer uses this to seed ParseOptions.
func (r *Router) Defaults() Options { return r.cfg.Defaults }

// httpClient returns the configured HTTP client, defaulting to
// http.DefaultClient.
func (r *Router) httpClient() *http.Client {
	if r.cfg.HTTPClient != nil {
		return r.cfg.HTTPClient
	}
	return http.DefaultClient
}

// latestUserText returns the content of the last user message in the query.
func latestUserText(q []Turn) string {
	for i := len(q) - 1; i >= 0; i-- {
		if q[i].Role == "user" {
			return q[i].Content
		}
	}
	return ""
}

// Image inline policy and attachment-disk constants.
//
// The relay's universal path for any attachment is: download to disk
// under the conv's cwd, emit file:// ResourceLink. ACP agents handle
// file:// ResourceLink natively (fir, for example, converts it to an
// @<path> mention in ExtractPromptContent). Inline ImageBlock is an
// additive optimisation, not a replacement: when the format and size
// fit the universal vision-model envelope (PNG/JPEG/GIF/WebP, ≤ ~3 MB
// raw to leave headroom for base64 under Bedrock-Anthropic's 3.75 MB
// cap), the relay also emits an ImageBlock so the LLM sees the pixels
// directly without needing a tool round-trip.
//
// Anything outside the inline envelope (HEIC, BMP, PDF, video, octet
// stream, oversize images, …) "just works" because the agent falls
// back to its own tools (sips, pdftotext, ffprobe, Read) on the
// file:// path.
const (
	defaultMaxInlineImageBytes int64         = 3 * 1024 * 1024
	defaultMaxAttachmentBytes  int64         = 100 * 1024 * 1024
	defaultAttachmentTTL       time.Duration = 30 * 24 * time.Hour
)

// attachmentDirName is the per-conv subdir holding downloaded files.
const attachmentDirName = ".poe-attachments"

var imageInlineAllowedMimeTypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/gif":  true,
	"image/webp": true,
}

// attachmentBlocks turns one Poe attachment into one or more ACP content
// blocks for the latest user prompt.
//
// Universal path:
//
//  1. parsed_content + agent advertises embeddedContext → inline
//     ResourceBlock (TextResourceContents). Poe pre-parsed the file for
//     us; no fetch, no tool round-trip on the agent side.
//
//  2. Otherwise: download a.URL to <cwd>/.poe-attachments/<msgID>/<name>
//     (using os.Root to confine writes inside the conv cwd, even if
//     a.Name is hostile), emit a ResourceLink whose URI is the
//     properly-escaped file:// form of the absolute path, with the
//     MimeType set so the agent can route without sniffing.
//
//  3. Additive inline: when the mime type is in the inline allow-list
//     (PNG/JPEG/GIF/WebP) and the file is within MaxInlineImageBytes,
//     also emit an ImageBlock(base64) AFTER the link. Agent gets both:
//     file path for tool work, pixels for the LLM directly.
//
// Download failures degrade to a plain https ResourceLink so the agent
// at least learns the URL existed. Logged via debuglog. A prompt is
// never failed because of attachment IO.
//
// `used` is a per-prompt set tracking already-claimed filenames inside
// the message dir; on collision the helper appends "-2", "-3", ….
func (r *Router) attachmentBlocks(
	ctx context.Context,
	cwd, msgID string,
	used map[string]struct{},
	a Attachment,
	embedded bool,
) []acp.ContentBlock {
	// Path 1: pre-parsed text — fastest, agent gets the bytes directly.
	if embedded && a.ParsedContent != "" {
		return []acp.ContentBlock{textResourceBlock(a.URL, a.ParsedContent, a.ContentType)}
	}

	// Paths 2 + 3: download to disk, emit file:// ResourceLink, possibly
	// followed by an inline ImageBlock.
	absPath, name, size, err := r.downloadAttachment(ctx, cwd, msgID, used, a)
	if err != nil {
		debuglog.Logf("attachmentBlocks: download failed url=%s err=%v; emitting bare ResourceLink", a.URL, err)
		// Last-resort: tell the agent about the URL so a vision-capable
		// agent that can fetch directly still has a chance.
		return []acp.ContentBlock{resourceLinkBlockHTTPS(a)}
	}

	link := fileResourceLinkBlock(name, absPath, a.ContentType)
	out := []acp.ContentBlock{link}

	if imageInlineAllowedMimeTypes[a.ContentType] && size <= r.maxInlineImageBytes() {
		data, rerr := os.ReadFile(absPath)
		if rerr != nil {
			debuglog.Logf("attachmentBlocks: inline read failed path=%s err=%v; link-only", absPath, rerr)
		} else {
			out = append(out, acp.ImageBlock(base64.StdEncoding.EncodeToString(data), a.ContentType))
		}
	}
	return out
}

func (r *Router) maxInlineImageBytes() int64 {
	if r.cfg.MaxInlineImageBytes > 0 {
		return r.cfg.MaxInlineImageBytes
	}
	return defaultMaxInlineImageBytes
}

func (r *Router) maxAttachmentBytes() int64 {
	if r.cfg.MaxAttachmentBytes > 0 {
		return r.cfg.MaxAttachmentBytes
	}
	return defaultMaxAttachmentBytes
}

// downloadAttachment GETs a.URL and writes the body to
// <cwd>/.poe-attachments/<msgID>/<name>, using os.Root so a hostile
// a.Name (e.g. "../../etc/passwd") cannot escape the message dir. The
// helper retries with a hash-derived fallback name if the kernel/runtime
// rejects the supplied name. Returns the absolute path, the final name,
// and the byte count on success.
func (r *Router) downloadAttachment(
	ctx context.Context,
	cwd, msgID string,
	used map[string]struct{},
	a Attachment,
) (absPath, name string, size int64, err error) {
	root, err := openMessageDir(cwd, msgID)
	if err != nil {
		return "", "", 0, fmt.Errorf("open message dir: %w", err)
	}
	defer root.Close()

	hc := r.httpClient()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return "", "", 0, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", "", 0, fmt.Errorf("http %d", resp.StatusCode)
	}
	max := r.maxAttachmentBytes()
	if resp.ContentLength > 0 && resp.ContentLength > max {
		return "", "", 0, fmt.Errorf("declared content-length %d exceeds cap %d", resp.ContentLength, max)
	}

	preferred := preferredName(a)
	finalName := uniqueName(preferred, used)
	f, perr := root.OpenFile(finalName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if perr != nil {
		// os.Root rejects ".." components and absolute paths. Retry
		// once with a hash-derived fallback so a hostile a.Name can't
		// kill the attachment.
		debuglog.Logf("downloadAttachment: Root rejected name=%q err=%v; using fallback", finalName, perr)
		finalName = uniqueName(fallbackName(a), used)
		f, perr = root.OpenFile(finalName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if perr != nil {
			return "", "", 0, fmt.Errorf("create attachment file: %w", perr)
		}
	}
	used[finalName] = struct{}{}
	// LimitReader+1 so we can detect overflow.
	n, cerr := io.Copy(f, io.LimitReader(resp.Body, max+1))
	closeErr := f.Close()
	if cerr != nil {
		_ = root.Remove(finalName)
		return "", "", 0, cerr
	}
	if closeErr != nil {
		_ = root.Remove(finalName)
		return "", "", 0, closeErr
	}
	if n > max {
		_ = root.Remove(finalName)
		return "", "", 0, fmt.Errorf("attachment exceeds cap %d bytes", max)
	}
	abs := filepath.Join(cwd, attachmentDirName, msgID, finalName)
	return abs, finalName, n, nil
}

// openMessageDir opens (creating if needed) <cwd>/.poe-attachments/<msgID>
// as an os.Root. The caller must Close() the returned Root.
func openMessageDir(cwd, msgID string) (*os.Root, error) {
	attBase := filepath.Join(cwd, attachmentDirName)
	if err := os.MkdirAll(attBase, 0o755); err != nil {
		return nil, err
	}
	parent, err := os.OpenRoot(attBase)
	if err != nil {
		return nil, err
	}
	// Mkdir tolerates ErrExist via Stat-then-create dance.
	if err := parent.Mkdir(msgID, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		parent.Close()
		return nil, err
	}
	sub, err := parent.OpenRoot(msgID)
	parent.Close()
	if err != nil {
		return nil, err
	}
	return sub, nil
}

// preferredName picks the filename to write under .poe-attachments/<msgID>/.
// Empty / "." / ".." trigger fallback. Names longer than 200 bytes are
// truncated while preserving the extension.
func preferredName(a Attachment) string {
	name := a.Name
	switch name {
	case "", ".", "..":
		return fallbackName(a)
	}
	return capName(name, 200)
}

// fallbackName synthesises a stable, harmless filename from the URL hash
// plus an extension derived from the content type.
func fallbackName(a Attachment) string {
	h := sha256.Sum256([]byte(a.URL))
	stem := "attachment-" + hex.EncodeToString(h[:4])
	if exts, _ := mime.ExtensionsByType(a.ContentType); len(exts) > 0 {
		return stem + exts[0]
	}
	return stem + ".bin"
}

// capName truncates name to max bytes while preserving its extension.
func capName(name string, max int) string {
	if len(name) <= max {
		return name
	}
	ext := filepath.Ext(name)
	if len(ext) >= max {
		return name[:max]
	}
	return name[:max-len(ext)] + ext
}

// uniqueName returns name if unused, otherwise appends "-2", "-3", …
// before the extension until it finds an unclaimed slot. The caller is
// responsible for marking the result as used.
func uniqueName(name string, used map[string]struct{}) string {
	if _, taken := used[name]; !taken {
		return name
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d%s", stem, i, ext)
		if _, taken := used[candidate]; !taken {
			return candidate
		}
	}
}

// fileResourceLinkBlock builds a ResourceLink with a properly-escaped
// file:// URI for the absolute path. Using net/url ensures spaces and
// non-ASCII characters in absPath are encoded per RFC 3986 rather than
// emitted raw (which would yield a malformed URI for spec-conformant
// agents).
func fileResourceLinkBlock(name, absPath, contentType string) acp.ContentBlock {
	uri := (&url.URL{Scheme: "file", Path: absPath}).String()
	return resourceLinkBlock(name, uri, contentType)
}

// resourceLinkBlockHTTPS is the last-resort fallback when download
// fails: emit a ResourceLink to the original Poe URL.
func resourceLinkBlockHTTPS(a Attachment) acp.ContentBlock {
	name := a.Name
	if name == "" {
		name = a.URL
	}
	return resourceLinkBlock(name, a.URL, a.ContentType)
}

// resourceLinkBlock is the shared builder behind the file:// and https
// helpers. Sets MimeType on the returned ResourceLink when known so
// the agent can route without sniffing.
func resourceLinkBlock(name, uri, contentType string) acp.ContentBlock {
	block := acp.ResourceLinkBlock(name, uri)
	if contentType != "" && block.ResourceLink != nil {
		ct := contentType
		block.ResourceLink.MimeType = &ct
	}
	return block
}

// textResourceBlock builds an embedded text Resource block. Sets MimeType
// when known so the agent can route without sniffing.
func textResourceBlock(uri, text, mime string) acp.ContentBlock {
	trc := acp.TextResourceContents{Uri: uri, Text: text}
	if mime != "" {
		m := mime
		trc.MimeType = &m
	}
	return acp.ResourceBlock(acp.EmbeddedResourceResource{TextResourceContents: &trc})
}

// latestUserTurnRef returns the last user turn in the query, or zero
// + false if none exists. Only the latest turn's attachments are
// forwarded; prior turns' files are already part of the agent's
// session history.
func latestUserTurnRef(q []Turn) (Turn, bool) {
	for i := len(q) - 1; i >= 0; i-- {
		if q[i].Role == "user" {
			return q[i], true
		}
	}
	return Turn{}, false
}

// flattenTranscript turns a multi-turn Poe query into a single seed prompt
// for an agent that has no prior context. Format: each turn is prefixed
// with a role tag; the latest user turn is emitted last.
//
// Note: prior turns' attachments are intentionally not reconstructed
// here. Poe attachment URLs are signed and may have expired by the time
// we cold-resume; only the latest user turn's attachments are forwarded
// (as ResourceLink/Resource blocks alongside the seed text).
func flattenTranscript(q []Turn) string {
	var b strings.Builder
	b.WriteString("[Resuming a prior conversation. Transcript so far:]\n\n")
	for _, t := range q {
		var label string
		switch t.Role {
		case "user":
			label = "User"
		case "bot":
			label = "Assistant"
		case "system":
			label = "System"
		default:
			label = t.Role
		}
		b.WriteString(label)
		b.WriteString(": ")
		b.WriteString(t.Content)
		b.WriteString("\n\n")
	}
	b.WriteString("[End of prior transcript. Respond to the latest User message above.]")
	return b.String()
}

// Cancel asks the agent to cancel the current prompt for a conv.
func (r *Router) Cancel(ctx context.Context, convID string) error {
	r.mu.Lock()
	st, ok := r.sessions[convID]
	r.mu.Unlock()
	if !ok {
		return nil
	}
	return r.cfg.Agent.Cancel(ctx, st.sessionID)
}

// getOrCreate returns the sessionState for convID, creating or resuming an
// agent session if necessary. freshSeed is true iff the caller should seed
// the next prompt with the full transcript (cold path that did not resume,
// AND has prior turns to seed from).
func (r *Router) getOrCreate(ctx context.Context, convID, userID string, query []Turn) (st *sessionState, freshSeed bool, err error) {
	r.mu.Lock()
	if existing, ok := r.sessions[convID]; ok {
		r.mu.Unlock()
		debuglog.Logf("getOrCreate conv=%s -> hit (sid=%s)", convID, string(existing.sessionID))
		return existing, false, nil
	}
	r.mu.Unlock()
	debuglog.Logf("getOrCreate conv=%s user=%s -> miss, query_len=%d", convID, userID, len(query))

	cwd := filepath.Join(r.cfg.StateDir, "convs", convID)
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		return nil, false, fmt.Errorf("mkdir conv dir: %w", err)
	}

	st = &sessionState{
		convID:     convID,
		userID:     userID,
		cwd:        cwd,
		lastUsedNs: r.cfg.Now().UnixNano(),
	}

	// Tier 1: try resume if the agent supports list+resume.
	caps := r.cfg.Agent.Caps()
	if caps.ListSessions && caps.ResumeSession {
		sessions, lerr := r.cfg.Agent.ListSessions(ctx, cwd)
		if lerr == nil && len(sessions) > 0 {
			sid := acp.SessionId(sessions[0].SessionId)
			if rerr := r.cfg.Agent.ResumeSession(ctx, cwd, sid, st); rerr == nil {
				st.sessionID = sid
				// Resume: the previous session may have had the system
				// prompt installed, but we have no way to confirm. On
				// the cap-supported path the RFD says agents SHOULD
				// restore it on session/load — trust that. On the
				// fallback path, re-inject inline on the next prompt
				// (the RFD's stated mitigation).
				if r.cfg.SystemPrompt != "" && !caps.SystemPrompt {
					st.pendingSystemPromptInline = true
				}
				winner, _ := r.install(convID, st)
				debuglog.Logf("getOrCreate conv=%s -> resumed sid=%s", convID, string(sid))
				return winner, false, nil
			} else {
				debuglog.Logf("getOrCreate conv=%s resume failed: %v", convID, rerr)
			}
		} else if lerr != nil {
			debuglog.Logf("getOrCreate conv=%s list_sessions err: %v", convID, lerr)
		}
	}

	// Tier 2: new session. If we have prior turns, the caller will seed.
	var sysBlocks []acp.ContentBlock
	if r.cfg.SystemPrompt != "" && caps.SystemPrompt {
		sysBlocks = []acp.ContentBlock{acp.TextBlock(r.cfg.SystemPrompt)}
	}
	sid, nerr := r.cfg.Agent.NewSession(ctx, cwd, st, sysBlocks)
	if nerr != nil {
		return nil, false, fmt.Errorf("acp new session: %w", nerr)
	}
	st.sessionID = sid
	// Fallback path for new sessions: agent didn't advertise the cap,
	// so inline the system prompt on the first user prompt.
	if r.cfg.SystemPrompt != "" && !caps.SystemPrompt {
		st.pendingSystemPromptInline = true
	}
	freshSeed = len(query) > 1
	winner, won := r.install(convID, st)
	if !won {
		// Lost the race: the existing session is already hot and has
		// (or will have) its own history; do not double-seed it.
		freshSeed = false
	}
	debuglog.Logf("getOrCreate conv=%s -> new sid=%s won_race=%v fresh_seed=%v",
		convID, string(sid), won, freshSeed)
	return winner, freshSeed, nil
}

// install registers st under convID, returning the winning entry and
// whether st itself was the winner (true) or an existing entry beat us
// (false).
func (r *Router) install(convID string, st *sessionState) (*sessionState, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.sessions[convID]; ok {
		// Lost the race; the session we just created/resumed leaks on the
		// agent side but that is cheap and rare.
		return existing, false
	}
	r.sessions[convID] = st
	return st, true
}

func (r *Router) touch(convID string) {
	r.mu.Lock()
	if st, ok := r.sessions[convID]; ok {
		st.lastUsedNs = r.cfg.Now().UnixNano()
	}
	r.mu.Unlock()
}

// RunGC runs a background goroutine that drops idle sessions. Returns a
// stop function.
func (r *Router) RunGC(ctx context.Context, every time.Duration) (stop func()) {
	ctx2, cancel := context.WithCancel(ctx)
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx2.Done():
				return
			case <-t.C:
				r.gcOnce()
				r.sweepAttachmentsOnce()
			}
		}
	}()
	return cancel
}

func (r *Router) gcOnce() {
	cutoff := r.cfg.Now().Add(-r.cfg.SessionTTL).UnixNano()
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, st := range r.sessions {
		if st.inUse == 0 && st.lastUsedNs < cutoff {
			delete(r.sessions, id)
		}
	}
}

// sweepAttachmentsOnce walks <StateDir>/convs/*/.poe-attachments/ and
// removes files whose mtime is older than AttachmentTTL. Empty message
// dirs are removed afterwards so the directory tree doesn't drift.
//
// Decoupled from gcOnce: a hot session may keep its memory state but
// still have stale files from old turns that should be reaped.
func (r *Router) sweepAttachmentsOnce() {
	ttl := r.cfg.AttachmentTTL
	if ttl <= 0 {
		return
	}
	cutoff := r.cfg.Now().Add(-ttl)
	convsRoot := filepath.Join(r.cfg.StateDir, "convs")
	convDirs, err := os.ReadDir(convsRoot)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			debuglog.Logf("sweepAttachmentsOnce: read %s: %v", convsRoot, err)
		}
		return
	}
	var removedFiles, removedDirs int
	for _, cd := range convDirs {
		if !cd.IsDir() {
			continue
		}
		attRoot := filepath.Join(convsRoot, cd.Name(), attachmentDirName)
		msgDirs, err := os.ReadDir(attRoot)
		if err != nil {
			continue // no attachments for this conv
		}
		for _, md := range msgDirs {
			if !md.IsDir() {
				continue
			}
			msgPath := filepath.Join(attRoot, md.Name())
			files, err := os.ReadDir(msgPath)
			if err != nil {
				continue
			}
			liveCount := 0
			for _, fe := range files {
				p := filepath.Join(msgPath, fe.Name())
				info, err := fe.Info()
				if err != nil {
					liveCount++
					continue
				}
				if info.Mode().IsRegular() && info.ModTime().Before(cutoff) {
					if err := os.Remove(p); err == nil {
						removedFiles++
					} else {
						liveCount++
						debuglog.Logf("sweepAttachmentsOnce: remove %s: %v", p, err)
					}
				} else {
					liveCount++
				}
			}
			if liveCount == 0 {
				if err := os.Remove(msgPath); err == nil {
					removedDirs++
				}
			}
		}
	}
	if removedFiles > 0 || removedDirs > 0 {
		debuglog.Logf("sweepAttachmentsOnce: removed %d files, %d empty dirs", removedFiles, removedDirs)
	}
}

// Len returns the number of tracked sessions.
func (r *Router) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sessions)
}

// DebugInfo is a snapshot of session state for /debug/sessions.
type DebugInfo struct {
	ConvID    string    `json:"conv_id"`
	UserID    string    `json:"user_id"`
	SessionID string    `json:"session_id"`
	Cwd       string    `json:"cwd"`
	LastUsed  time.Time `json:"last_used"`
}

// Debug returns a snapshot of all tracked sessions, sorted by conv_id.
func (r *Router) Debug() []DebugInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]DebugInfo, 0, len(r.sessions))
	for _, st := range r.sessions {
		out = append(out, DebugInfo{
			ConvID:    st.convID,
			UserID:    st.userID,
			SessionID: string(st.sessionID),
			Cwd:       st.cwd,
			LastUsed:  time.Unix(0, st.lastUsedNs),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ConvID < out[j].ConvID })
	return out
}

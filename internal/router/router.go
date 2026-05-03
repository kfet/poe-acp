// Package router maps Poe conversation_ids to ACP sessions and owns their
// lifecycle (lazy create / resume, per-conv cwd, serial-per-conv prompting,
// idle GC).
package router

import (
	"context"
	"fmt"
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
	Role        string // "user", "bot", or "system"
	Content     string
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
	Thinking     string // "" = leave as-is; one of "off","minimal","low","medium","high"
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
	for _, a := range latestUserAttachments(query) {
		if a.URL == "" {
			// Defensive: an empty URL would produce a ResourceLink that
			// most agents reject. Skip silently.
			continue
		}
		blocks = append(blocks, attachmentBlock(a, embedded))
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
			return fmt.Errorf("set_config thinking_level=%s: %w", opts.Thinking, err)
		}
		st.applied.Thinking = opts.Thinking
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
		case "off", "minimal", "low", "medium", "high":
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

// latestUserText returns the content of the last user message in the query.
func latestUserText(q []Turn) string {
	for i := len(q) - 1; i >= 0; i-- {
		if q[i].Role == "user" {
			return q[i].Content
		}
	}
	return ""
}

// attachmentBlock turns one Poe attachment into an ACP content block.
// Prefers ContentBlock::Resource (TextResourceContents) when the agent
// advertises embeddedContext AND Poe has computed parsed_content for
// the file — that path delivers the text inline, avoiding a fetch.
// Falls back to ResourceLink (the mandatory ACP baseline) otherwise.
// Sets MimeType on either block when known so the agent can route the
// file correctly without sniffing.
func attachmentBlock(a Attachment, embedded bool) acp.ContentBlock {
	if embedded && a.ParsedContent != "" {
		trc := acp.TextResourceContents{
			Uri:  a.URL,
			Text: a.ParsedContent,
		}
		if a.ContentType != "" {
			ct := a.ContentType
			trc.MimeType = &ct
		}
		return acp.ResourceBlock(acp.EmbeddedResourceResource{TextResourceContents: &trc})
	}
	name := a.Name
	if name == "" {
		name = a.URL
	}
	block := acp.ResourceLinkBlock(name, a.URL)
	if a.ContentType != "" && block.ResourceLink != nil {
		ct := a.ContentType
		block.ResourceLink.MimeType = &ct
	}
	return block
}

// latestUserAttachments returns the attachments on the last user message.
// Only the latest turn's attachments are forwarded; prior turns' files
// are already part of the agent's session history.
func latestUserAttachments(q []Turn) []Attachment {
	for i := len(q) - 1; i >= 0; i-- {
		if q[i].Role == "user" {
			return q[i].Attachments
		}
	}
	return nil
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

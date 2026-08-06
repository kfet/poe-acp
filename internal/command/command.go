// Package command implements the relay's chat-command surface: it
// brokers interactive OAuth login (bridging Poe turns into fir's
// _meta.auth.interactive two-call protocol) and the session-control
// commands (!status, !model, !new, !help).
//
// The user-facing surface is deliberately small: !help, !status,
// !model [filter|id], !new, !login [provider|cancel]. Older spellings
// (!models, !relay, !bot, !cancel-login, !whoami, !reset) still work as
// undocumented aliases so nothing a user has learned ever breaks.
//
// Each Poe conversation can have at most one in-flight login. The first
// login command (e.g. "!login anthropic") calls fir's authenticate to
// produce a URL; the next user turn from the same conversation submits
// the pasted redirect URL. The broker holds no goroutines — the actual
// blocking on the user input happens inside fir, parked across turns.
// The relay only remembers which conversation has a pending login for
// which method.
//
// Commands accept the sigils "/", "!", and "." but user-facing prose
// suggests "!" (DisplaySigil): Poe's chat client intercepts "/"-prefixed
// messages as native slash commands and rejects unknown ones before they
// reach the bot, so "/login" is unreliable; "!"/"." pass straight through.
package command

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/kfet/acp-kit/client"
	"github.com/kfet/poe-acp/internal/router"
)

// Authenticator is the agent-side surface the broker depends on. The full
// *client.AgentProc satisfies it.
type Authenticator interface {
	AuthMethods() []client.AuthMethod
	Authenticate(ctx context.Context, methodID, id, redirect string, cancel bool) (client.AuthResult, error)
}

// Controller is the per-conversation session-control surface used by the
// non-auth commands (!status, !model, !new). *router.Router
// satisfies it. Optional: if nil, those commands report unavailable.
// (router never imports command — the dependency edge is one-way:
// command → router.)
type Controller interface {
	AvailableModels() (models []client.ModelInfo, currentID string)
	SetModelOverride(convID, modelID string) error
	ResetSession(convID string) error
	StatusFor(convID string) router.SessionStatus
	AgentCommands() []client.CommandInfo
	RelayInfo(convID string) router.RelayInfo
}

// passthroughAllow is the curated set of agent-advertised commands the
// relay is willing to forward as chat commands (`!reload` → `/reload`).
// Kept deliberately small and safe: read-only, non-destructive, or
// explicitly account-scoped (logout) or config-reload (mcp) operations the user invokes
// directly. Commands outside this set never reach the agent via the
// command surface (the user's literal text still does).
var passthroughAllow = map[string]bool{
	"reload":    true,
	"logout":    true,
	"compact":   true,
	"session":   true,
	"changelog": true,
	"mcp":       true,
	"skills":    true,
}

// Deliberately NOT allowlisted, and not an oversight:
//
//   - resume, continue — the relay owns the conversation→session mapping.
//     Letting the agent switch its own session underneath the relay would
//     desync that mapping (the relay would keep prompting into a session
//     the agent has moved on from). !new is the supported way to change
//     session state. Do not "fix" this by adding them here.
//   - name, share, export — side-effecting or account/artefact-scoped
//     operations that make no sense driven from a Poe chat turn.

// Broker tracks per-conversation pending logins.
type Broker struct {
	a    Authenticator
	ctrl Controller // optional; set via SetController for session commands

	mu      sync.Mutex
	pending map[string]pendingEntry // convID → in-flight login
}

// SetController wires the session-control surface (the router) used by
// !status/!model/!new. Call once at startup, after construction,
// to break the broker↔router construction cycle. Safe before any turns.
func (b *Broker) SetController(c Controller) { b.ctrl = c }

// Passthrough decides whether text is an allowlisted agent command and,
// if so, returns the prompt text to forward to the agent (e.g. "!reload"
// → "/reload"). ok=false means the turn is not a passthrough command and
// should be handled normally. The command must be both allowlisted and
// actually advertised by the agent. Requires a wired Controller.
func (b *Broker) Passthrough(text string) (rewritten string, ok bool) {
	if b.ctrl == nil {
		return "", false
	}
	body, has := stripSigil(strings.TrimSpace(text))
	if !has || body == "" {
		return "", false
	}
	name := body
	if i := strings.IndexByte(body, ' '); i >= 0 {
		name = body[:i]
	}
	if !passthroughAllow[name] || !b.agentHasCommand(name) {
		return "", false
	}
	return "/" + body, true
}

// agentHasCommand reports whether the agent currently advertises name.
func (b *Broker) agentHasCommand(name string) bool {
	for _, c := range b.ctrl.AgentCommands() {
		if c.Name == name {
			return true
		}
	}
	return false
}

// passthroughCommands returns the allowlisted commands the agent
// currently advertises, for display in !help.
func (b *Broker) passthroughCommands() []client.CommandInfo {
	if b.ctrl == nil {
		return nil
	}
	var out []client.CommandInfo
	for _, c := range b.ctrl.AgentCommands() {
		if passthroughAllow[c.Name] {
			out = append(out, c)
		}
	}
	return out
}

// pendingEntry is the per-conversation in-flight login state.
type pendingEntry struct {
	methodID string
	// authID is the opaque id returned by the agent on call 1; passed
	// back on call 2 / cancel so the agent can disambiguate concurrent
	// pending logins for the same methodID.
	authID string
}

// New constructs a Broker.
func New(a Authenticator) *Broker {
	return &Broker{a: a, pending: make(map[string]pendingEntry)}
}

// HasPending reports whether the conversation is waiting for a pasted
// redirect URL.
func (b *Broker) HasPending(convID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.pending[convID]
	return ok
}

// commandSigils are the leading characters that introduce a relay
// command. Poe's chat client intercepts "/"-prefixed messages as native
// slash commands and rejects unknown ones before they ever reach the
// bot, so a bare "/login" usually never arrives (and is flaky when it
// does — see docs). "!" and "." pass straight through untouched. We
// accept all three on input and echo DisplaySigil in user-facing prose.
const commandSigils = "/!."

// DisplaySigil is the command prefix shown in user-facing messages. It
// is deliberately not "/" so the suggested commands survive Poe's
// client-side slash-command interceptor.
const DisplaySigil = "!"

// stripSigil removes a single leading command sigil from t (which must
// already be TrimSpace'd) and reports whether one was present.
func stripSigil(t string) (body string, ok bool) {
	if t == "" {
		return t, false
	}
	if strings.IndexByte(commandSigils, t[0]) >= 0 {
		return t[1:], true
	}
	return t, false
}

// isLoginBody reports whether a sigil-stripped command body is one of the
// login-family commands.
func isLoginBody(body string) bool {
	return body == "login" || strings.HasPrefix(body, "login ") ||
		body == "logins" || // alias for "list"
		body == "cancel-login"
}

// isCancelLoginBody reports whether a sigil-stripped body aborts an
// in-flight login: the documented `login cancel`, or the undocumented
// `cancel-login` alias kept for backwards compatibility.
func isCancelLoginBody(body string) bool {
	if body == "cancel-login" {
		return true
	}
	rest, ok := strings.CutPrefix(body, "login ")
	return ok && strings.TrimSpace(rest) == "cancel"
}

// IsLoginCommand reports whether text is a login/logins/cancel-login
// command under any accepted sigil (/, !, .). Trims leading whitespace
// so users can paste the command after a thought.
func IsLoginCommand(text string) bool {
	body, ok := stripSigil(strings.TrimSpace(text))
	if !ok {
		return false
	}
	return isLoginBody(body)
}

// isSessionBody reports whether a sigil-stripped body is one of the
// session-control commands (handled only when a Controller is wired).
func isSessionBody(body string) bool {
	switch {
	case body == "status" || body == "whoami":
		return true
	case body == "relay" || body == "bot":
		return true
	case body == "models" || strings.HasPrefix(body, "models "):
		return true
	case body == "model" || strings.HasPrefix(body, "model "):
		return true
	case body == "new" || body == "reset":
		return true
	}
	return false
}

// IsCommand reports whether text is any relay command the broker handles
// (the login family, "help", or a session command), under any accepted
// sigil. The HTTP handler uses this to decide whether to route a turn to
// the broker instead of forwarding it to the agent.
func IsCommand(text string) bool {
	body, ok := stripSigil(strings.TrimSpace(text))
	if !ok {
		return false
	}
	return body == "help" || isLoginBody(body) || isSessionBody(body)
}

// Outcome is what the HTTP handler should render to the user. Exactly
// one of Text / URL / Error is set; Done is always implied (the auth
// turn never streams further chunks).
type Outcome struct {
	// Text is a plain message to stream as a Poe `text` event.
	Text string
	// URL, if non-empty, is the auth URL to surface to the user. Text
	// may also be set with prose preceding/following the URL.
	URL string
	// Instructions accompany URL.
	Instructions string
}

// Handle dispatches one user turn. Behaviour:
//
//  1. If the conversation has a pending login, treat any non-command
//     text as the pasted redirect URL.
//  2. Otherwise interpret !login [provider|cancel] (and the older
//     !logins / !cancel-login spellings).
//
// Returns (nil, nil) if the turn is not auth-related and should be
// forwarded to the normal prompt path.
func (b *Broker) Handle(ctx context.Context, convID, text string) (*Outcome, error) {
	t := strings.TrimSpace(text)
	body, hasSigil := stripSigil(t)

	// Help is stateless and never collides with a pasted redirect URL
	// (those never carry a sigil), so it wins even mid-login.
	if hasSigil && body == "help" {
		return b.help(), nil
	}

	// Pasted redirect URL for an in-flight login wins over command parsing.
	if entry, ok := b.peek(convID); ok {
		if hasSigil && isCancelLoginBody(body) {
			return b.cancel(ctx, convID, entry)
		}
		return b.complete(ctx, convID, entry, t)
	}

	if !hasSigil {
		return nil, nil
	}

	switch {
	case isCancelLoginBody(body):
		return &Outcome{Text: "No login in progress."}, nil
	case body == "login" || body == "logins":
		return b.list(), nil
	case strings.HasPrefix(body, "login "):
		// body has a provider arg: "login <provider>".
		rest := strings.TrimSpace(strings.TrimPrefix(body, "login"))
		return b.start(ctx, convID, rest)
	case body == "relay" || body == "bot":
		// Undocumented aliases: !relay folded into !status.
		return b.status(convID), nil
	case body == "status" || body == "whoami":
		return b.status(convID), nil
	case body == "models" || strings.HasPrefix(body, "models "):
		// Undocumented alias: !models folded into !model.
		return b.models(strings.TrimSpace(strings.TrimPrefix(body, "models"))), nil
	case body == "model" || strings.HasPrefix(body, "model "):
		return b.model(convID, strings.TrimSpace(strings.TrimPrefix(body, "model"))), nil
	case body == "new" || body == "reset":
		return b.reset(convID), nil
	default:
		// Sigil-prefixed but not a login command — not ours.
		return nil, nil
	}
}

// OfferLogin renders the onboarding message shown when the agent reports
// that no usable provider is connected (an "Authentication required"
// prompt error). It lists the loginable providers using the Poe-safe
// DisplaySigil. Safe for concurrent use. Returned text is empty-safe:
// it always yields actionable guidance, even with no OAuth methods.
func (b *Broker) OfferLogin() string {
	loginable := filterLoginable(b.a.AuthMethods())
	if len(loginable) == 0 {
		return "⚠️ This bot has no LLM provider connected, and the agent " +
			"advertises no interactive login methods. Set a provider API key " +
			"in the agent's environment (e.g. `ANTHROPIC_API_KEY`) and restart."
	}
	var sb strings.Builder
	sb.WriteString("⚠️ No LLM provider is connected yet, so I can't answer. " +
		"Connect one by sending one of these (the leading `" + DisplaySigil +
		"` matters — Poe swallows a leading `/`):\n\n")
	for _, m := range loginable {
		shortID := strings.TrimPrefix(m.ID, "oauth-")
		fmt.Fprintf(&sb, "- `%slogin %s` — %s\n", DisplaySigil, shortID, m.Name)
	}
	sb.WriteString("\nThen open the URL I reply with, authenticate, and paste " +
		"the page's URL back here to finish.")
	return sb.String()
}

// help lists the relay commands the broker understands.
func (b *Broker) help() *Outcome {
	s := DisplaySigil
	var sb strings.Builder
	sb.WriteString("Available commands:\n\n")
	sb.WriteString("- `" + s + "help` — show this message\n")
	if b.ctrl != nil {
		sb.WriteString("- `" + s + "status` — model, session and relay info\n")
		sb.WriteString("- `" + s + "model [filter|id]` — list/filter models, or switch\n")
		sb.WriteString("- `" + s + "new` — start a fresh session (clears context)\n")
	}
	sb.WriteString("- `" + s + "login [provider|cancel]` — connect a provider (e.g. `" + s +
		"login anthropic`), or abort a login in progress\n")
	if pt := b.passthroughCommands(); len(pt) > 0 {
		sb.WriteString("\nAgent commands:\n\n")
		for _, c := range pt {
			fmt.Fprintf(&sb, "- `%s%s`", s, c.Name)
			if c.Description != "" {
				fmt.Fprintf(&sb, " — %s", c.Description)
			}
			sb.WriteString("\n")
		}
	}
	return &Outcome{Text: sb.String()}
}

// status renders one compact snapshot of everything the user might ask
// about: the conversation's model / thinking / session (the old !status)
// plus the relay process's version / uptime / sessions (the old !relay).
// Rendered as a short bullet list — it has to read well on a phone, so
// no tables and no wide lines; conversation state first, relay after.
func (b *Broker) status(convID string) *Outcome {
	if b.ctrl == nil {
		return &Outcome{Text: "Session control is unavailable."}
	}
	st := b.ctrl.StatusFor(convID)
	ri := b.ctrl.RelayInfo(convID)
	var sb strings.Builder
	sb.WriteString("**Status**\n\n")
	fmt.Fprintf(&sb, "- model: `%s`", st.EffectiveModel)
	if st.OverrideModel != "" {
		fmt.Fprintf(&sb, " (set via %smodel)", DisplaySigil)
	}
	sb.WriteString("\n")
	if st.Thinking != "" {
		fmt.Fprintf(&sb, "- thinking: %s\n", st.Thinking)
	}
	sess := "none yet (fresh on next message)"
	if st.HasSession {
		sess = "active"
	}
	if ri.SessionID != "" {
		sess += " `" + ri.SessionID + "`"
	}
	fmt.Fprintf(&sb, "- session: %s\n", sess)
	fmt.Fprintf(&sb, "- models available: %d\n", st.ModelsAvailable)
	if ri.Version != "" {
		fmt.Fprintf(&sb, "- relay: `%s`\n", ri.Version)
	}
	if ri.Uptime != "" {
		fmt.Fprintf(&sb, "- uptime: %s\n", ri.Uptime)
	}
	if ri.AgentCmd != "" {
		fmt.Fprintf(&sb, "- agent: `%s`\n", ri.AgentCmd)
	}
	fmt.Fprintf(&sb, "- active conversations: %d\n", ri.ActiveSessions)
	return &Outcome{Text: sb.String()}
}

// modelsListCap bounds how many models a model listing prints in one
// message.
const modelsListCap = 40

// models lists available model ids, optionally filtered by substring.
// It backs bare `!model`, `!model <filter>` and the undocumented
// `!models [filter]` alias.
func (b *Broker) models(filter string) *Outcome {
	if b.ctrl == nil {
		return &Outcome{Text: "Session control is unavailable."}
	}
	all, current := b.ctrl.AvailableModels()
	if len(all) == 0 {
		return &Outcome{Text: fmt.Sprintf("No models available — connect a provider with `%slogin`.", DisplaySigil)}
	}
	f := strings.ToLower(filter)
	matched := make([]client.ModelInfo, 0, len(all))
	for _, m := range all {
		if f == "" || strings.Contains(strings.ToLower(m.ID), f) {
			matched = append(matched, m)
		}
	}
	var sb strings.Builder
	if filter == "" {
		fmt.Fprintf(&sb, "%d models available (current: `%s`). `%smodel <id>` to switch, `%smodel <filter>` to narrow:\n\n",
			len(all), current, DisplaySigil, DisplaySigil)
	} else {
		fmt.Fprintf(&sb, "%d model(s) match %q (current: `%s`):\n\n", len(matched), filter, current)
	}
	if len(matched) == 0 {
		fmt.Fprintf(&sb, "(none match %q — try `%smodel` for the full list)\n", filter, DisplaySigil)
		return &Outcome{Text: sb.String()}
	}
	for i, m := range matched {
		if i >= modelsListCap {
			fmt.Fprintf(&sb, "…and %d more (filter to narrow).\n", len(matched)-modelsListCap)
			break
		}
		marker := ""
		if m.ID == current {
			marker = " ←"
		}
		fmt.Fprintf(&sb, "- `%s`%s\n", m.ID, marker)
	}
	return &Outcome{Text: sb.String()}
}

// model implements `!model [filter|id]`:
//
//	no arg          → list every available model
//	arg == model id → switch this chat to it
//	anything else   → treat the arg as a list filter
//
// Exact-id match wins over the filter reading, so a model id that also
// happens to be a substring of other ids still switches. A filter that
// narrows to exactly one model deliberately does NOT auto-switch —
// silently changing model off an approximate match would be surprising.
func (b *Broker) model(convID, arg string) *Outcome {
	if b.ctrl == nil {
		return &Outcome{Text: "Session control is unavailable."}
	}
	if arg == "" {
		return b.models("")
	}
	all, _ := b.ctrl.AvailableModels()
	for _, m := range all {
		if m.ID == arg {
			return b.setModel(convID, arg)
		}
	}
	return b.models(arg)
}

// setModel switches the sticky model for the conversation.
func (b *Broker) setModel(convID, id string) *Outcome {
	if err := b.ctrl.SetModelOverride(convID, id); err != nil {
		return &Outcome{Text: fmt.Sprintf("❌ %v. Use `%smodel` to see available ids.", err, DisplaySigil)}
	}
	return &Outcome{Text: fmt.Sprintf("✅ Model set to `%s` for this chat — applies from your next message.", id)}
}

// reset drops the conversation's live session so the next turn is fresh.
func (b *Broker) reset(convID string) *Outcome {
	if b.ctrl == nil {
		return &Outcome{Text: "Session control is unavailable."}
	}
	if err := b.ctrl.ResetSession(convID); err != nil {
		return &Outcome{Text: fmt.Sprintf("Couldn't reset: %v.", err)}
	}
	return &Outcome{Text: "🧹 Fresh session — previous context cleared. Your model choice is kept."}
}

// list renders the available login methods.
func (b *Broker) list() *Outcome {
	methods := b.a.AuthMethods()
	loginable := filterLoginable(methods)
	if len(loginable) == 0 {
		return &Outcome{Text: "No OAuth login methods available."}
	}
	var sb strings.Builder
	sb.WriteString("Available login methods:\n\n")
	for _, m := range loginable {
		shortID := strings.TrimPrefix(m.ID, "oauth-")
		fmt.Fprintf(&sb, "- `%slogin %s` — %s", DisplaySigil, shortID, m.Name)
		if m.Description != "" {
			fmt.Fprintf(&sb, " (%s)", m.Description)
		}
		sb.WriteString("\n")
	}
	return &Outcome{Text: sb.String()}
}

// start initiates a login.
func (b *Broker) start(ctx context.Context, convID, provider string) (*Outcome, error) {
	methodID, err := b.resolveMethodID(provider)
	if err != nil {
		return &Outcome{Text: err.Error()}, nil
	}

	// Refuse a second concurrent login for this conv.
	b.mu.Lock()
	if existing, ok := b.pending[convID]; ok {
		b.mu.Unlock()
		return &Outcome{Text: fmt.Sprintf("A login is already in progress (%s). Paste the redirect URL or send `%slogin cancel`.", existing.methodID, DisplaySigil)}, nil
	}
	b.mu.Unlock()

	res, err := b.a.Authenticate(ctx, methodID, "", "", false)
	if err != nil {
		return nil, fmt.Errorf("authenticate %s: %w", methodID, err)
	}
	switch res.State {
	case "needs_redirect":
		if res.ID == "" {
			// Agent supports the protocol but didn't return an id —
			// fall back to single-pending-per-method semantics by
			// passing "" on call 2. This still works against older
			// fir builds.
		}
		b.mu.Lock()
		b.pending[convID] = pendingEntry{methodID: methodID, authID: res.ID}
		b.mu.Unlock()
		text := fmt.Sprintf("Open this URL to authenticate, then paste the URL of the page you land on (even if it fails to load):\n\n%s\n", res.URL)
		if res.Instructions != "" {
			text += "\n" + res.Instructions + "\n"
		}
		return &Outcome{Text: text, URL: res.URL, Instructions: res.Instructions}, nil
	case "ok", "":
		return &Outcome{Text: fmt.Sprintf("✅ Already authenticated (%s).", methodID)}, nil
	case "cancelled":
		return &Outcome{Text: "Login cancelled."}, nil
	default:
		return &Outcome{Text: fmt.Sprintf("Login returned unexpected state: %q.", res.State)}, nil
	}
}

// complete submits the pasted redirect URL.
func (b *Broker) complete(ctx context.Context, convID string, entry pendingEntry, redirect string) (*Outcome, error) {
	if redirect == "" {
		return &Outcome{Text: fmt.Sprintf("Empty paste — send the redirect URL or `%slogin cancel`.", DisplaySigil)}, nil
	}
	res, err := b.a.Authenticate(ctx, entry.methodID, entry.authID, redirect, false)
	// Always drop the pending entry — a failed paste means the user must
	// start over (matches the TUI: a bad paste aborts and prints the error).
	b.mu.Lock()
	delete(b.pending, convID)
	b.mu.Unlock()
	if err != nil {
		return &Outcome{Text: fmt.Sprintf("❌ Login failed: %v\n\nSend `%slogin %s` to try again.", err, DisplaySigil, strings.TrimPrefix(entry.methodID, "oauth-"))}, nil
	}
	switch res.State {
	case "ok", "":
		return &Outcome{Text: fmt.Sprintf("✅ Authenticated (%s).", entry.methodID)}, nil
	case "cancelled":
		return &Outcome{Text: "Login cancelled."}, nil
	default:
		return &Outcome{Text: fmt.Sprintf("Login returned unexpected state: %q.", res.State)}, nil
	}
}

// cancel cancels an in-flight login.
func (b *Broker) cancel(ctx context.Context, convID string, entry pendingEntry) (*Outcome, error) {
	b.mu.Lock()
	delete(b.pending, convID)
	b.mu.Unlock()
	if _, err := b.a.Authenticate(ctx, entry.methodID, entry.authID, "", true); err != nil {
		return &Outcome{Text: fmt.Sprintf("Login cancelled (agent reported: %v).", err)}, nil
	}
	return &Outcome{Text: "Login cancelled."}, nil
}

// peek returns the pending entry for convID.
func (b *Broker) peek(convID string) (pendingEntry, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.pending[convID]
	return e, ok
}

// resolveMethodID maps a user-typed provider name to the full method id
// advertised by the agent. Accepts both the short form ("anthropic") and
// the full form ("oauth-anthropic"). Case-insensitive on the short form.
func (b *Broker) resolveMethodID(provider string) (string, error) {
	methods := b.a.AuthMethods()
	loginable := filterLoginable(methods)
	if len(loginable) == 0 {
		return "", errors.New("the agent advertises no OAuth login methods")
	}
	want := strings.ToLower(strings.TrimSpace(provider))
	for _, m := range loginable {
		if m.ID == provider {
			return m.ID, nil
		}
		if strings.EqualFold(strings.TrimPrefix(m.ID, "oauth-"), want) {
			return m.ID, nil
		}
	}
	available := make([]string, 0, len(loginable))
	for _, m := range loginable {
		available = append(available, strings.TrimPrefix(m.ID, "oauth-"))
	}
	sort.Strings(available)
	return "", fmt.Errorf("unknown provider %q. Available: %s", provider, strings.Join(available, ", "))
}

// filterLoginable returns only OAuth/agent-typed methods we can actually
// drive over Poe (env_var and terminal methods aren't usable here).
func filterLoginable(methods []client.AuthMethod) []client.AuthMethod {
	out := methods[:0:0]
	for _, m := range methods {
		// type=="" defaults to "agent" per the RFD.
		if m.Type == "" || m.Type == "agent" {
			out = append(out, m)
		}
	}
	return out
}

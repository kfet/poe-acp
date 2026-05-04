// Package authbroker bridges Poe's turn-based chat into fir's
// _meta.auth.interactive two-call OAuth protocol.
//
// Each Poe conversation can have at most one in-flight login. The first
// /login turn calls fir's authenticate to produce a URL; the next user
// turn from the same conversation submits the pasted redirect URL. The
// broker holds no goroutines — the actual blocking on the user input
// happens inside fir, parked across turns. The relay only remembers
// which conversation has a pending login for which method.
package authbroker

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/kfet/poe-acp/internal/acpclient"
)

// Authenticator is the agent-side surface the broker depends on. The full
// *acpclient.AgentProc satisfies it.
type Authenticator interface {
	AuthMethods() []acpclient.AuthMethod
	Authenticate(ctx context.Context, methodID, id, redirect string, cancel bool) (acpclient.AuthResult, error)
}

// Broker tracks per-conversation pending logins.
type Broker struct {
	a Authenticator

	mu      sync.Mutex
	pending map[string]pendingEntry // convID → in-flight login
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

// IsLoginCommand reports whether text is a /login or /logout command.
// Trims leading whitespace so users can paste the command after a thought.
func IsLoginCommand(text string) bool {
	t := strings.TrimSpace(text)
	return t == "/login" || strings.HasPrefix(t, "/login ") ||
		t == "/logins" || // alias for "list"
		t == "/cancel-login"
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
//  2. Otherwise interpret /login [provider], /logins, /cancel-login.
//
// Returns (nil, nil) if the turn is not auth-related and should be
// forwarded to the normal prompt path.
func (b *Broker) Handle(ctx context.Context, convID, text string) (*Outcome, error) {
	t := strings.TrimSpace(text)

	// Pasted redirect URL for an in-flight login wins over command parsing.
	if entry, ok := b.peek(convID); ok {
		if t == "/cancel-login" {
			return b.cancel(ctx, convID, entry)
		}
		return b.complete(ctx, convID, entry, t)
	}

	if !IsLoginCommand(t) {
		return nil, nil
	}

	if t == "/cancel-login" {
		return &Outcome{Text: "No login in progress."}, nil
	}
	if t == "/login" || t == "/logins" {
		return b.list(), nil
	}
	rest := strings.TrimSpace(strings.TrimPrefix(t, "/login"))
	provider := strings.TrimSpace(rest)
	if provider == "" {
		return b.list(), nil
	}
	return b.start(ctx, convID, provider)
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
		fmt.Fprintf(&sb, "- `/login %s` — %s", shortID, m.Name)
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
		return &Outcome{Text: fmt.Sprintf("A login is already in progress (%s). Paste the redirect URL or send `/cancel-login`.", existing.methodID)}, nil
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
		return &Outcome{Text: "Empty paste — send the redirect URL or `/cancel-login`."}, nil
	}
	res, err := b.a.Authenticate(ctx, entry.methodID, entry.authID, redirect, false)
	// Always drop the pending entry — a failed paste means the user must
	// start over (matches the TUI: a bad paste aborts and prints the error).
	b.mu.Lock()
	delete(b.pending, convID)
	b.mu.Unlock()
	if err != nil {
		return &Outcome{Text: fmt.Sprintf("❌ Login failed: %v\n\nSend `/login %s` to try again.", err, strings.TrimPrefix(entry.methodID, "oauth-"))}, nil
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
func filterLoginable(methods []acpclient.AuthMethod) []acpclient.AuthMethod {
	out := methods[:0:0]
	for _, m := range methods {
		// type=="" defaults to "agent" per the RFD.
		if m.Type == "" || m.Type == "agent" {
			out = append(out, m)
		}
	}
	return out
}

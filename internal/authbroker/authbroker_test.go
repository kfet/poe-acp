package authbroker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kfet/acp-kit/client"
)

type fakeAuth struct {
	methods []client.AuthMethod
	calls   []call
	err     error
	res     client.AuthResult
}

type call struct {
	method, id, redirect string
	cancel               bool
}

func (f *fakeAuth) AuthMethods() []client.AuthMethod { return f.methods }
func (f *fakeAuth) Authenticate(_ context.Context, methodID, id, redirect string, cancel bool) (client.AuthResult, error) {
	f.calls = append(f.calls, call{methodID, id, redirect, cancel})
	return f.res, f.err
}

func newFake() *fakeAuth {
	return &fakeAuth{
		methods: []client.AuthMethod{
			{ID: "oauth-anthropic", Name: "Anthropic", Type: "agent"},
			{ID: "oauth-github-copilot", Name: "GitHub Copilot", Type: "agent"},
			{ID: "env-openai", Name: "OpenAI key", Type: "env_var"},
		},
	}
}

func TestList_HappyPath(t *testing.T) {
	f := newFake()
	b := New(f)
	out, err := b.Handle(context.Background(), "c1", "/login")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Text, "anthropic") || !strings.Contains(out.Text, "github-copilot") {
		t.Fatalf("listing missing methods: %s", out.Text)
	}
	if strings.Contains(out.Text, "env-openai") || strings.Contains(out.Text, "OPENAI") {
		t.Fatalf("env_var methods leaked into login list: %s", out.Text)
	}
}

func TestList_NoMethods(t *testing.T) {
	b := New(&fakeAuth{})
	out, _ := b.Handle(context.Background(), "c1", "/login")
	if !strings.Contains(out.Text, "No OAuth login") {
		t.Fatalf("expected no-methods text, got %q", out.Text)
	}
}

func TestStart_NeedsRedirectThenComplete(t *testing.T) {
	f := newFake()
	f.res = client.AuthResult{State: "needs_redirect", URL: "https://x/auth"}
	b := New(f)

	out, err := b.Handle(context.Background(), "c1", "/login anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Text, "https://x/auth") {
		t.Fatalf("URL missing from outcome: %q", out.Text)
	}
	if !b.HasPending("c1") {
		t.Fatal("expected pending login after needs_redirect")
	}

	// Now paste.
	f.res = client.AuthResult{State: "ok"}
	out, err = b.Handle(context.Background(), "c1", "https://localhost/cb?code=abc&state=xyz")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Text, "Authenticated") {
		t.Fatalf("expected auth success, got %q", out.Text)
	}
	if b.HasPending("c1") {
		t.Fatal("pending should be cleared after success")
	}
	if len(f.calls) != 2 {
		t.Fatalf("expected 2 Authenticate calls, got %d", len(f.calls))
	}
	if f.calls[1].redirect != "https://localhost/cb?code=abc&state=xyz" {
		t.Errorf("call 2 redirect = %q", f.calls[1].redirect)
	}
}

func TestStart_CachedCreds(t *testing.T) {
	f := newFake()
	f.res = client.AuthResult{State: "ok"}
	b := New(f)
	out, _ := b.Handle(context.Background(), "c1", "/login anthropic")
	if !strings.Contains(out.Text, "Already authenticated") {
		t.Fatalf("got %q", out.Text)
	}
	if b.HasPending("c1") {
		t.Fatal("no pending expected for cached creds")
	}
}

func TestStart_UnknownProvider(t *testing.T) {
	b := New(newFake())
	out, _ := b.Handle(context.Background(), "c1", "/login bogus")
	if !strings.Contains(out.Text, "unknown provider") {
		t.Fatalf("got %q", out.Text)
	}
}

func TestComplete_RedirectError(t *testing.T) {
	f := newFake()
	f.res = client.AuthResult{State: "needs_redirect", URL: "https://x"}
	b := New(f)
	if _, err := b.Handle(context.Background(), "c1", "/login anthropic"); err != nil {
		t.Fatal(err)
	}

	f.err = errors.New("oauth login failed")
	out, err := b.Handle(context.Background(), "c1", "https://localhost/cb")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Text, "Login failed") {
		t.Fatalf("got %q", out.Text)
	}
	if b.HasPending("c1") {
		t.Fatal("pending should be cleared after failure")
	}
}

func TestCancel_WithPending(t *testing.T) {
	f := newFake()
	f.res = client.AuthResult{State: "needs_redirect", URL: "https://x"}
	b := New(f)
	_, _ = b.Handle(context.Background(), "c1", "/login anthropic")

	f.res = client.AuthResult{State: "cancelled"}
	out, _ := b.Handle(context.Background(), "c1", "/cancel-login")
	if !strings.Contains(out.Text, "cancelled") {
		t.Fatalf("got %q", out.Text)
	}
	if b.HasPending("c1") {
		t.Fatal("pending should be cleared after cancel")
	}
	// Last call must have cancel=true.
	if last := f.calls[len(f.calls)-1]; !last.cancel {
		t.Error("expected cancel call")
	}
}

func TestCancel_WithoutPending(t *testing.T) {
	b := New(newFake())
	out, _ := b.Handle(context.Background(), "c1", "/cancel-login")
	if !strings.Contains(out.Text, "No login") {
		t.Fatalf("got %q", out.Text)
	}
}

func TestNonAuthTurn_PassesThrough(t *testing.T) {
	b := New(newFake())
	out, err := b.Handle(context.Background(), "c1", "hello world")
	if err != nil {
		t.Fatal(err)
	}
	if out != nil {
		t.Fatalf("non-auth turn should return (nil, nil), got %+v", out)
	}
}

func TestPendingCapturesNextTurnAsPaste(t *testing.T) {
	// Once pending, /login command on a *different* provider is treated
	// as the paste? No — only non-/cancel-login text is treated as paste.
	// Verify that a text that doesn't look like a command is the paste.
	f := newFake()
	f.res = client.AuthResult{State: "needs_redirect", URL: "https://x"}
	b := New(f)
	_, _ = b.Handle(context.Background(), "c1", "/login anthropic")

	f.res = client.AuthResult{State: "ok"}
	out, err := b.Handle(context.Background(), "c1", "anything-the-user-pastes")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Text, "Authenticated") {
		t.Fatalf("got %q", out.Text)
	}
	if got := f.calls[len(f.calls)-1].redirect; got != "anything-the-user-pastes" {
		t.Errorf("paste = %q", got)
	}
}

func TestConcurrentLoginAttemptIsRejected(t *testing.T) {
	f := newFake()
	f.res = client.AuthResult{State: "needs_redirect", URL: "https://x"}
	b := New(f)
	_, _ = b.Handle(context.Background(), "c1", "/login anthropic")

	// Different conv: this turn is a new /login command — but for c1 it's
	// pending. Simulate same conv issuing /login again. Per spec: pending
	// captures any text as paste, so /login text is handed through as the
	// paste string. (User can /cancel-login first if they want to switch.)
	got := f.calls
	if len(got) != 1 {
		t.Fatalf("setup: expected 1 call, got %d", len(got))
	}

	out, _ := b.Handle(context.Background(), "c1", "/login github-copilot")
	if out == nil {
		t.Fatal("expected outcome (paste flow), got passthrough")
	}
	// Must have submitted that text as the redirect.
	if last := f.calls[len(f.calls)-1]; last.redirect != "/login github-copilot" {
		t.Errorf("expected paste, got call=%+v", last)
	}
}

func TestMultiConvIsolation(t *testing.T) {
	// Conv A and conv B both /login anthropic. Each must get its own
	// authID; pasting on B must call authenticate with B's id, not A's.
	f := newFake()
	f.res = client.AuthResult{State: "needs_redirect", URL: "https://x", ID: "id-A"}
	b := New(f)
	if _, err := b.Handle(context.Background(), "convA", "/login anthropic"); err != nil {
		t.Fatal(err)
	}
	f.res = client.AuthResult{State: "needs_redirect", URL: "https://x", ID: "id-B"}
	if _, err := b.Handle(context.Background(), "convB", "/login anthropic"); err != nil {
		t.Fatal(err)
	}

	f.res = client.AuthResult{State: "ok"}
	if _, err := b.Handle(context.Background(), "convB", "paste-B"); err != nil {
		t.Fatal(err)
	}
	last := f.calls[len(f.calls)-1]
	if last.id != "id-B" || last.redirect != "paste-B" {
		t.Errorf("convB paste used id=%q redirect=%q, want id=id-B redirect=paste-B", last.id, last.redirect)
	}

	// convA still pending.
	if !b.HasPending("convA") {
		t.Error("convA pending should be intact")
	}
	if b.HasPending("convB") {
		t.Error("convB should be cleared")
	}

	if _, err := b.Handle(context.Background(), "convA", "paste-A"); err != nil {
		t.Fatal(err)
	}
	last = f.calls[len(f.calls)-1]
	if last.id != "id-A" || last.redirect != "paste-A" {
		t.Errorf("convA paste used id=%q redirect=%q, want id=id-A redirect=paste-A", last.id, last.redirect)
	}
}

func TestPendingPropagatesAuthID(t *testing.T) {
	f := newFake()
	f.res = client.AuthResult{State: "needs_redirect", URL: "https://x", ID: "the-id"}
	b := New(f)
	if _, err := b.Handle(context.Background(), "c1", "/login anthropic"); err != nil {
		t.Fatal(err)
	}
	f.res = client.AuthResult{State: "ok"}
	if _, err := b.Handle(context.Background(), "c1", "paste"); err != nil {
		t.Fatal(err)
	}
	if got := f.calls[1].id; got != "the-id" {
		t.Errorf("call 2 id = %q, want the-id", got)
	}
}

func TestResolveMethodID_FullID(t *testing.T) {
	b := New(newFake())
	id, err := b.resolveMethodID("oauth-anthropic")
	if err != nil || id != "oauth-anthropic" {
		t.Fatalf("got id=%q err=%v", id, err)
	}
}

// TestBangSigil_EndToEnd verifies the "!" sigil (which survives Poe's
// slash-command interceptor) drives the same flow as "/", and that
// user-facing suggestions use the bang form.
func TestBangSigil_EndToEnd(t *testing.T) {
	f := newFake()
	f.res = client.AuthResult{State: "needs_redirect", URL: "https://x/auth"}
	b := New(f)

	// List via "." sigil must enumerate methods using the "!" display sigil.
	list, _ := b.Handle(context.Background(), "c1", ".login")
	if !strings.Contains(list.Text, "!login anthropic") {
		t.Fatalf("list should suggest the bang sigil, got: %q", list.Text)
	}
	if strings.Contains(list.Text, "/login ") {
		t.Fatalf("list should not suggest the slash sigil, got: %q", list.Text)
	}

	// Start via "!" sigil.
	out, err := b.Handle(context.Background(), "c1", "!login anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Text, "https://x/auth") || !b.HasPending("c1") {
		t.Fatalf("bang-sigil login did not start: %q pending=%v", out.Text, b.HasPending("c1"))
	}

	// Cancel via "!" sigil.
	cancel, _ := b.Handle(context.Background(), "c1", "!cancel-login")
	if !strings.Contains(cancel.Text, "cancelled") && !strings.Contains(cancel.Text, "Login cancelled") {
		t.Fatalf("bang-sigil cancel failed: %q", cancel.Text)
	}
	if b.HasPending("c1") {
		t.Fatal("expected no pending login after cancel")
	}
}

// TestHandle_SigilNonLogin covers the defensive path: a sigil-prefixed
// message that isn't a login command, with no pending login, is not ours
// (returns nil, nil) so it falls through to the normal prompt path.
func TestHandle_SigilNonLogin(t *testing.T) {
	b := New(newFake())
	out, err := b.Handle(context.Background(), "c1", "!foo bar")
	if err != nil || out != nil {
		t.Fatalf("expected (nil, nil) for non-login sigil command, got out=%v err=%v", out, err)
	}
}

// TestOfferLogin lists loginable providers with the Poe-safe sigil and
// degrades gracefully when the agent advertises none.
func TestOfferLogin(t *testing.T) {
	got := New(newFake()).OfferLogin()
	if !strings.Contains(got, "!login anthropic") || !strings.Contains(got, "!login github-copilot") {
		t.Fatalf("offer missing loginable providers: %q", got)
	}
	if strings.Contains(got, "env-openai") || strings.Contains(got, "/login ") {
		t.Fatalf("offer leaked env method or slash sigil: %q", got)
	}

	none := New(&fakeAuth{}).OfferLogin()
	if !strings.Contains(none, "API key") {
		t.Fatalf("no-methods offer should suggest an env API key, got %q", none)
	}
}

func TestIsLoginCommand(t *testing.T) {
	cases := map[string]bool{
		"/login":              true,
		"  /login  ":          true,
		"/login anthropic":    true,
		"/logins":             true,
		"/cancel-login":       true,
		"!login":              true,
		"!login anthropic":    true,
		".login":              true,
		".logins":             true,
		"!cancel-login":       true,
		"  !login anthropic ": true,
		"login":               false,
		"hello":               false,
		"/loginanthropic":     false,
		"!loginanthropic":     false,
		"please /login later": false,
		"!logout":             false,
	}
	for in, want := range cases {
		if got := IsLoginCommand(in); got != want {
			t.Errorf("IsLoginCommand(%q) = %v, want %v", in, got, want)
		}
	}
}

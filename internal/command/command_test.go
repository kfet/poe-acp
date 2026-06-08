package command

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kfet/acp-kit/client"
	"github.com/kfet/poe-acp/internal/router"
)

// fakeCtrl is a stub command.Controller for command tests.
type fakeCtrl struct {
	models    []client.ModelInfo
	current   string
	setErr    error
	resetErr  error
	status    router.SessionStatus
	lastSet   [2]string // {convID, modelID}
	resetConv string
	agentCmds []client.CommandInfo
	relayInfo router.RelayInfo
}

func (c *fakeCtrl) AvailableModels() ([]client.ModelInfo, string) { return c.models, c.current }
func (c *fakeCtrl) SetModelOverride(conv, id string) error {
	c.lastSet = [2]string{conv, id}
	return c.setErr
}
func (c *fakeCtrl) ResetSession(conv string) error        { c.resetConv = conv; return c.resetErr }
func (c *fakeCtrl) StatusFor(string) router.SessionStatus { return c.status }
func (c *fakeCtrl) AgentCommands() []client.CommandInfo   { return c.agentCmds }
func (c *fakeCtrl) RelayInfo(string) router.RelayInfo     { return c.relayInfo }

func withCtrl(c *fakeCtrl) *Broker {
	b := New(newFake())
	b.SetController(c)
	return b
}

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

// TestHelp_ListsCommands: !help enumerates the relay commands.
func TestHelp_ListsCommands(t *testing.T) {
	out, err := New(newFake()).Handle(context.Background(), "c1", "!help")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"!help", "!login", "!login <provider>", "!cancel-login"} {
		if !strings.Contains(out.Text, want) {
			t.Fatalf("help output missing %q: %s", want, out.Text)
		}
	}
}

// TestHelp_DuringPendingLogin: !help is stateless — it shows help without
// consuming the pending login as a pasted redirect or cancelling it.
func TestHelp_DuringPendingLogin(t *testing.T) {
	f := newFake()
	f.res = client.AuthResult{State: "needs_redirect", URL: "https://x/auth"}
	b := New(f)
	if _, err := b.Handle(context.Background(), "c1", "!login anthropic"); err != nil {
		t.Fatal(err)
	}
	if !b.HasPending("c1") {
		t.Fatal("precondition: expected pending login")
	}
	out, _ := b.Handle(context.Background(), "c1", "!help")
	if !strings.Contains(out.Text, "Available commands") {
		t.Fatalf("expected help during pending, got %q", out.Text)
	}
	if !b.HasPending("c1") {
		t.Fatal("!help must not clear a pending login")
	}
}

func TestIsCommand(t *testing.T) {
	cases := map[string]bool{
		"!help":            true,
		"/help":            true,
		".help":            true,
		"  !help ":         true,
		"!login":           true,
		"!login anthropic": true,
		"!cancel-login":    true,
		"help":             false, // no sigil
		"!helpme":          false,
		"!foo":             false,
		"hello":            false,
	}
	for in, want := range cases {
		if got := IsCommand(in); got != want {
			t.Errorf("IsCommand(%q) = %v, want %v", in, got, want)
		}
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

func hb(b *Broker, text string) string {
	out, _ := b.Handle(context.Background(), "c1", text)
	if out == nil {
		return ""
	}
	return out.Text
}

func TestStatus(t *testing.T) {
	b := withCtrl(&fakeCtrl{status: router.SessionStatus{
		EffectiveModel: "anthropic/x", OverrideModel: "anthropic/x",
		Thinking: "low", HasSession: true, ModelsAvailable: 7,
	}})
	got := hb(b, "!status")
	for _, w := range []string{"anthropic/x", "low", "7", "active", "!model"} {
		if !strings.Contains(got, w) {
			t.Fatalf("status missing %q: %s", w, got)
		}
	}
	// whoami alias + no-session wording.
	b2 := withCtrl(&fakeCtrl{status: router.SessionStatus{EffectiveModel: "m", ModelsAvailable: 1}})
	if g := hb(b2, "!whoami"); !strings.Contains(g, "fresh on next message") {
		t.Fatalf("whoami no-session: %s", g)
	}
}

func TestModelsCommand(t *testing.T) {
	models := []client.ModelInfo{{ID: "anthropic/opus"}, {ID: "openai/gpt"}, {ID: "anthropic/haiku"}}
	b := withCtrl(&fakeCtrl{models: models, current: "anthropic/opus"})
	all := hb(b, "!models")
	if !strings.Contains(all, "3 models") || !strings.Contains(all, "anthropic/opus") || !strings.Contains(all, "←") {
		t.Fatalf("models list: %s", all)
	}
	filt := hb(b, "!models anthropic")
	if !strings.Contains(filt, "anthropic/opus") || strings.Contains(filt, "openai/gpt") {
		t.Fatalf("models filter: %s", filt)
	}
	if g := hb(b, "!models zzz"); !strings.Contains(g, "none match") {
		t.Fatalf("models no-match: %s", g)
	}
	if g := hb(withCtrl(&fakeCtrl{}), "!models"); !strings.Contains(g, "No models") {
		t.Fatalf("models empty: %s", g)
	}
}

func TestModelsCap(t *testing.T) {
	many := make([]client.ModelInfo, modelsListCap+5)
	for i := range many {
		many[i] = client.ModelInfo{ID: "p/m" + string(rune('a'+i%26)) + string(rune('0'+i/26))}
	}
	g := hb(withCtrl(&fakeCtrl{models: many}), "!models")
	if !strings.Contains(g, "and 5 more") {
		t.Fatalf("expected cap overflow note: %s", g)
	}
}

func TestModelCommand(t *testing.T) {
	c := &fakeCtrl{models: []client.ModelInfo{{ID: "p/m"}}, current: "p/m", status: router.SessionStatus{EffectiveModel: "p/m"}}
	b := withCtrl(c)
	// no arg → show current
	if g := hb(b, "!model"); !strings.Contains(g, "p/m") {
		t.Fatalf("model no-arg: %s", g)
	}
	// valid set
	if g := hb(b, "!model p/m"); !strings.Contains(g, "✅") || c.lastSet != [2]string{"c1", "p/m"} {
		t.Fatalf("model set: %s last=%v", g, c.lastSet)
	}
	// error from controller
	c.setErr = errors.New("unknown model \"bad\"")
	if g := hb(b, "!model bad"); !strings.Contains(g, "❌") || !strings.Contains(g, "unknown model") {
		t.Fatalf("model err: %s", g)
	}
}

func TestResetCommand(t *testing.T) {
	c := &fakeCtrl{}
	if g := hb(withCtrl(c), "!new"); !strings.Contains(g, "Fresh session") || c.resetConv != "c1" {
		t.Fatalf("new: %s conv=%s", g, c.resetConv)
	}
	c2 := &fakeCtrl{resetErr: errors.New("busy")}
	if g := hb(withCtrl(c2), "!reset"); !strings.Contains(g, "busy") {
		t.Fatalf("reset err: %s", g)
	}
}

func TestSessionCommands_NoController(t *testing.T) {
	b := New(newFake()) // no SetController
	for _, cmd := range []string{"!status", "!models", "!model x", "!new"} {
		if g := hb(b, cmd); !strings.Contains(g, "unavailable") {
			t.Fatalf("%s without controller: %s", cmd, g)
		}
	}
}

func TestHelp_DynamicWithController(t *testing.T) {
	withCmds := hb(withCtrl(&fakeCtrl{}), "!help")
	for _, w := range []string{"!status", "!models", "!model", "!new", "!login"} {
		if !strings.Contains(withCmds, w) {
			t.Fatalf("help (ctrl) missing %q: %s", w, withCmds)
		}
	}
	noCmds := hb(New(newFake()), "!help")
	if strings.Contains(noCmds, "!status") || strings.Contains(noCmds, "!models") {
		t.Fatalf("help (no ctrl) should omit session cmds: %s", noCmds)
	}
	if !strings.Contains(noCmds, "!login") {
		t.Fatalf("help (no ctrl) should still list login: %s", noCmds)
	}
}

func TestIsCommand_SessionVerbs(t *testing.T) {
	for _, c := range []string{"!status", "!whoami", "!models", "!models foo", "!model", "!model x", "!new", "!reset", ".status"} {
		if !IsCommand(c) {
			t.Errorf("IsCommand(%q) = false, want true", c)
		}
	}
	for _, c := range []string{"!statusx", "!modelss", "status", "!newish"} {
		if IsCommand(c) {
			t.Errorf("IsCommand(%q) = true, want false", c)
		}
	}
}

func TestPassthrough(t *testing.T) {
	c := &fakeCtrl{agentCmds: []client.CommandInfo{{Name: "reload", Description: "Reload"}, {Name: "share"}}}
	b := withCtrl(c)

	if r, ok := b.Passthrough("!reload"); !ok || r != "/reload" {
		t.Fatalf("reload: %q %v", r, ok)
	}
	// logout: allowlisted + advertised → forwarded to fir's /logout
	c.agentCmds = []client.CommandInfo{{Name: "logout", Description: "Log out"}}
	if r, ok := b.Passthrough("!logout"); !ok || r != "/logout" {
		t.Fatalf("logout: %q %v", r, ok)
	}
	if r, ok := b.Passthrough("!logout all"); !ok || r != "/logout all" {
		t.Fatalf("logout args: %q %v", r, ok)
	}
	c.agentCmds = []client.CommandInfo{{Name: "reload", Description: "Reload"}, {Name: "share"}}
	// allowlisted but agent does NOT advertise it
	if _, ok := b.Passthrough("!compact"); ok {
		t.Fatal("compact not advertised → false")
	}
	// advertised but not allowlisted
	if _, ok := b.Passthrough("!share"); ok {
		t.Fatal("share not allowlisted → false")
	}
	// no sigil
	if _, ok := b.Passthrough("reload"); ok {
		t.Fatal("no sigil → false")
	}
	// empty body
	if _, ok := b.Passthrough("!"); ok {
		t.Fatal("empty body → false")
	}
	// args preserved
	c.agentCmds = []client.CommandInfo{{Name: "session"}}
	if r, ok := b.Passthrough("!session foo"); !ok || r != "/session foo" {
		t.Fatalf("session args: %q %v", r, ok)
	}
	// nil controller
	if _, ok := New(newFake()).Passthrough("!reload"); ok {
		t.Fatal("nil ctrl → false")
	}
}

func TestHelp_ListsAgentCommands(t *testing.T) {
	b := withCtrl(&fakeCtrl{agentCmds: []client.CommandInfo{{Name: "reload", Description: "Reload exts"}, {Name: "share"}}})
	g := hb(b, "!help")
	if !strings.Contains(g, "Agent commands") || !strings.Contains(g, "!reload") || !strings.Contains(g, "Reload exts") {
		t.Fatalf("help should list allowlisted agent cmds: %s", g)
	}
	if strings.Contains(g, "!share") {
		t.Fatalf("non-allowlisted agent cmd leaked: %s", g)
	}
}

func TestRelayCommand(t *testing.T) {
	// No controller wired -> unavailable.
	bNo := New(newFake())
	if out, err := bNo.Handle(context.Background(), "c", "!relay"); err != nil || out == nil ||
		!strings.Contains(out.Text, "unavailable") {
		t.Fatalf("nil-ctrl relay: out=%+v err=%v", out, err)
	}

	// Fully populated info, including a live session id and override model.
	b := withCtrl(&fakeCtrl{relayInfo: router.RelayInfo{
		Version:         "9.9.9",
		Uptime:          "3h2m1s",
		AgentCmd:        "fir --mode acp",
		ModelsAvailable: 7,
		ActiveSessions:  4,
		SessionID:       "sess-abc",
		EffectiveModel:  "opus",
	}})
	out, err := b.Handle(context.Background(), "c", "!relay")
	if err != nil || out == nil {
		t.Fatalf("relay: out=%+v err=%v", out, err)
	}
	for _, want := range []string{"**Relay**", "9.9.9", "3h2m1s", "fir --mode acp",
		"opus", "models available: 7", "active conversations: 4", "sess-abc"} {
		if !strings.Contains(out.Text, want) {
			t.Fatalf("relay output missing %q:\n%s", want, out.Text)
		}
	}

	// !bot alias + empty optional fields + no session -> fresh-session line.
	b2 := withCtrl(&fakeCtrl{relayInfo: router.RelayInfo{ModelsAvailable: 1}})
	out2, err := b2.Handle(context.Background(), "c", "!bot")
	if err != nil || out2 == nil {
		t.Fatalf("bot: out=%+v err=%v", out2, err)
	}
	if !strings.Contains(out2.Text, "none yet") {
		t.Fatalf("bot output missing fresh-session line:\n%s", out2.Text)
	}
	for _, bad := range []string{"version:", "uptime:", "agent:", "model:"} {
		if strings.Contains(out2.Text, bad) {
			t.Fatalf("bot output should omit empty field %q:\n%s", bad, out2.Text)
		}
	}

	// IsCommand recognises both sigil spellings.
	for _, c := range []string{"!relay", "/relay", ".bot", "!bot"} {
		if !IsCommand(c) {
			t.Fatalf("IsCommand(%q) = false, want true", c)
		}
	}

	// help lists !relay when a controller is wired.
	if h := b.help(); !strings.Contains(h.Text, "relay` — relay version") {
		t.Fatalf("help missing relay line:\n%s", h.Text)
	}
}

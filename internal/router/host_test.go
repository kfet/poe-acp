package router

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// hostRouter builds a router whose agent echoes "ok", with the given
// curated host allowlist and default host.
func hostRouter(t *testing.T, hosts []string, defaultHost string) (*Router, *fakeAgent) {
	t.Helper()
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	r, err := New(Config{
		Agent:      agent,
		StateDir:   t.TempDir(),
		SessionTTL: time.Hour,
		Hosts:      hosts,
		Defaults:   Options{Host: defaultHost},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r, agent
}

func (f *fakeAgent) metaHost() (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.lastMeta["host"]
	s, _ := v.(string)
	return s, ok
}

// No hosts configured: session/new carries no _meta at all — the
// pre-feature wire shape, byte for byte.
func TestHost_UnconfiguredSendsNoMeta(t *testing.T) {
	r, agent := hostRouter(t, nil, "")
	// Even a caller injecting a host parameter must not move the wire.
	opts := ParseOptions(map[string]any{"host": "evil-box"}, r.Defaults(), nil)
	if opts.Host != "evil-box" {
		t.Fatalf("ParseOptions dropped host: %#v", opts)
	}
	if err := r.Prompt(context.Background(), "c1", "u", []Turn{{Role: "user", Content: "hi"}}, opts, &captureSink{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if _, ok := agent.metaHost(); ok {
		t.Fatal("_meta.host must be absent when no hosts are configured")
	}
	agent.mu.Lock()
	meta := agent.lastMeta
	agent.mu.Unlock()
	if meta != nil {
		t.Fatalf("extraMeta must be nil, got %#v", meta)
	}
}

// A configured, selected host lands in _meta.host at session create.
func TestHost_SelectedHostSentAtCreate(t *testing.T) {
	r, agent := hostRouter(t, []string{"zboxserver", "boxy"}, "zboxserver")
	opts := ParseOptions(map[string]any{"host": "boxy"}, r.Defaults(), nil)
	if err := r.Prompt(context.Background(), "c1", "u", []Turn{{Role: "user", Content: "hi"}}, opts, &captureSink{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if h, ok := agent.metaHost(); !ok || h != "boxy" {
		t.Fatalf("_meta.host = %q ok=%v, want boxy", h, ok)
	}
}

// No `host` parameter: the configured default is used.
func TestHost_DefaultUsedWhenParameterAbsent(t *testing.T) {
	r, agent := hostRouter(t, []string{"zboxserver", "boxy"}, "zboxserver")
	opts := ParseOptions(map[string]any{}, r.Defaults(), nil)
	if err := r.Prompt(context.Background(), "c1", "u", []Turn{{Role: "user", Content: "hi"}}, opts, &captureSink{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if h, _ := agent.metaHost(); h != "zboxserver" {
		t.Fatalf("_meta.host = %q, want zboxserver", h)
	}
}

// An unlisted host (untrusted parameter injection) is dropped and the
// configured default wins — the relay never hands an arbitrary ssh
// destination to the agent.
func TestHost_UnlistedHostRejected(t *testing.T) {
	r, agent := hostRouter(t, []string{"zboxserver"}, "zboxserver")
	opts := ParseOptions(map[string]any{"host": "attacker@evil"}, r.Defaults(), nil)
	if err := r.Prompt(context.Background(), "c1", "u", []Turn{{Role: "user", Content: "hi"}}, opts, &captureSink{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if h, _ := agent.metaHost(); h != "zboxserver" {
		t.Fatalf("_meta.host = %q, want zboxserver", h)
	}
}

// defaults.host without any curated list pins every conversation to one
// host and still sends _meta.host.
func TestHost_DefaultWithoutListStillSent(t *testing.T) {
	r, agent := hostRouter(t, nil, "zboxserver")
	if err := r.Prompt(context.Background(), "c1", "u", []Turn{{Role: "user", Content: "hi"}}, r.Defaults(), &captureSink{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if h, _ := agent.metaHost(); h != "zboxserver" {
		t.Fatalf("_meta.host = %q, want zboxserver", h)
	}
}

// THE important path: changing the Host dropdown on a LIVE conversation
// must keep the session (no new session, no torn-down pane) and say so
// exactly once, not on every subsequent turn.
func TestHost_LiveChangeKeepsSessionAndNotifiesOnce(t *testing.T) {
	r, agent := hostRouter(t, []string{"zboxserver", "boxy"}, "zboxserver")

	first := &captureSink{}
	if err := r.Prompt(context.Background(), "c1", "u", []Turn{{Role: "user", Content: "one"}}, r.Defaults(), first); err != nil {
		t.Fatalf("Prompt 1: %v", err)
	}
	if strings.Contains(first.text.String(), "host") {
		t.Fatalf("turn 1 must not mention host: %q", first.text.String())
	}
	sidBefore := sessionIDOf(t, r, "c1")
	created := atomic.LoadInt32(&agent.newSessCalls)

	// Turn 2 flips the dropdown to the other host.
	opts := ParseOptions(map[string]any{"host": "boxy"}, r.Defaults(), nil)
	second := &captureSink{}
	if err := r.Prompt(context.Background(), "c1", "u",
		[]Turn{{Role: "user", Content: "one", MessageID: "m1"}, {Role: "user", Content: "two", MessageID: "m2"}}, opts, second); err != nil {
		t.Fatalf("Prompt 2: %v", err)
	}
	if got := atomic.LoadInt32(&agent.newSessCalls); got != created {
		t.Fatalf("host change created a new session: NewSession calls %d -> %d", created, got)
	}
	if sid := sessionIDOf(t, r, "c1"); sid != sidBefore {
		t.Fatalf("session id changed on host twiddle: %q -> %q", sidBefore, sid)
	}
	if h, _ := agent.metaHost(); h != "zboxserver" {
		t.Fatalf("_meta.host must not be re-sent/changed, got %q", h)
	}
	txt := second.text.String()
	if !strings.Contains(txt, "host takes effect on the next conversation") ||
		!strings.Contains(txt, "zboxserver") {
		t.Fatalf("missing host notice, got %q", txt)
	}
	if !strings.Contains(txt, "ok") {
		t.Fatalf("turn output lost: %q", txt)
	}

	// Turn 3 with the SAME (changed) selection must not nag again.
	third := &captureSink{}
	if err := r.Prompt(context.Background(), "c1", "u",
		[]Turn{{Role: "user", Content: "one", MessageID: "m1"}, {Role: "user", Content: "two", MessageID: "m2"}, {Role: "user", Content: "three", MessageID: "m3"}}, opts, third); err != nil {
		t.Fatalf("Prompt 3: %v", err)
	}
	if strings.Contains(third.text.String(), "host takes effect") {
		t.Fatalf("host notice repeated on a later turn: %q", third.text.String())
	}

	// Flipping BACK to the session's real host is silent too.
	back := ParseOptions(map[string]any{"host": "zboxserver"}, r.Defaults(), nil)
	fourth := &captureSink{}
	if err := r.Prompt(context.Background(), "c1", "u",
		[]Turn{{Role: "user", Content: "one", MessageID: "m1"}, {Role: "user", Content: "two", MessageID: "m2"}, {Role: "user", Content: "three", MessageID: "m3"}, {Role: "user", Content: "four", MessageID: "m4"}}, back, fourth); err != nil {
		t.Fatalf("Prompt 4: %v", err)
	}
	if strings.Contains(fourth.text.String(), "host takes effect") {
		t.Fatalf("returning to the session's own host must be silent: %q", fourth.text.String())
	}
}

// A session created before any host was configured has host "", so the
// notice names the agent's own host rather than an empty backtick pair.
func TestHost_NoticeLabelsHostlessSession(t *testing.T) {
	r, _ := hostRouter(t, []string{"boxy"}, "")
	if err := r.Prompt(context.Background(), "c1", "u", []Turn{{Role: "user", Content: "one"}}, Options{}, &captureSink{}); err != nil {
		t.Fatalf("Prompt 1: %v", err)
	}
	sink := &captureSink{}
	opts := ParseOptions(map[string]any{"host": "boxy"}, r.Defaults(), nil)
	if err := r.Prompt(context.Background(), "c1", "u",
		[]Turn{{Role: "user", Content: "one", MessageID: "m1"}, {Role: "user", Content: "two", MessageID: "m2"}}, opts, sink); err != nil {
		t.Fatalf("Prompt 2: %v", err)
	}
	if !strings.Contains(sink.text.String(), "the agent's own host") {
		t.Fatalf("notice = %q", sink.text.String())
	}
}

// ParseOptions treats `host` as untrusted: wrong type and empty string
// leave the resolved default in place.
func TestHost_ParseOptionsJunk(t *testing.T) {
	defs := Options{Host: "zboxserver"}
	for name, params := range map[string]map[string]any{
		"wrong type":   {"host": 42},
		"empty string": {"host": ""},
		"absent":       {},
	} {
		if got := ParseOptions(params, defs, nil).Host; got != "zboxserver" {
			t.Fatalf("%s: host = %q, want zboxserver", name, got)
		}
	}
}

// A reaction never carries parameters; it must still resolve to the
// configured default host when it has to create the session.
func TestHost_ReactionCreatesSessionOnDefaultHost(t *testing.T) {
	r, agent := hostRouter(t, []string{"zboxserver"}, "zboxserver")
	if err := r.ReportReaction(context.Background(), "c1", "u", "m1", "like", "add"); err != nil {
		t.Fatalf("ReportReaction: %v", err)
	}
	if h, _ := agent.metaHost(); h != "zboxserver" {
		t.Fatalf("_meta.host = %q, want zboxserver", h)
	}
}

func sessionIDOf(t *testing.T, r *Router, convID string) acp.SessionId {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.sessions[convID]
	if !ok {
		t.Fatalf("no session for conv %q", convID)
	}
	return st.sessionID
}

// A bot that declares the Host dropdown must also defuse "--host" in
// agent output, or Poe's client rejects the whole message. A bot with no
// host list must leave it alone (see poeproto reserved flags).
func TestHost_ReservedFlagEscaping(t *testing.T) {
	for name, hosts := range map[string][]string{"configured": {"boxy"}, "unconfigured": nil} {
		agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
			a.emit(sid, "try --host boxy ")
			return acp.StopReasonEndTurn, nil
		})
		r, err := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour, Hosts: hosts})
		if err != nil {
			t.Fatalf("%s: New: %v", name, err)
		}
		sink := &captureSink{}
		if err := r.Prompt(context.Background(), "c1", "u", []Turn{{Role: "user", Content: "hi"}}, Options{}, sink); err != nil {
			t.Fatalf("%s: Prompt: %v", name, err)
		}
		escaped := strings.Contains(sink.text.String(), "--\u200bhost")
		if want := hosts != nil; escaped != want {
			t.Fatalf("%s: escaped=%v want %v (text %q)", name, escaped, want, sink.text.String())
		}
	}
}

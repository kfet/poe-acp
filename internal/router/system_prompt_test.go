package router

import (
	"context"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/acp-kit/client"
)

// TestSystemPrompt_CapPath: agent advertises session.systemPrompt cap,
// router must pass the prompt as session/new._meta blocks (visible here
// via fakeAgent.lastSysBlocks) and MUST NOT inline it on the first
// session/prompt.
func TestSystemPrompt_CapPath(t *testing.T) {
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	agent.caps = client.Caps{SystemPrompt: true}

	rtr := mustRouterWithSP(t, agent, "DURABLE-CATALOG-XYZ")
	sink := newCollector()
	if err := rtr.Prompt(context.Background(), "c-cap", "u",
		[]Turn{{Role: "user", Content: "hello"}}, Options{}, sink); err != nil {
		t.Fatal(err)
	}

	if len(agent.lastSysBlocks) != 1 || agent.lastSysBlocks[0].Text == nil ||
		!strings.Contains(agent.lastSysBlocks[0].Text.Text, "DURABLE-CATALOG-XYZ") {
		t.Fatalf("system prompt not delivered via _meta: %+v", agent.lastSysBlocks)
	}
	if !strings.Contains(agent.lastSysBlocks[0].Text.Text, "Relay & Transport Contract") {
		t.Fatalf("static system prompt missing relay/transport contract clause: %+v", agent.lastSysBlocks)
	}
	if strings.Contains(agent.lastPromptTxt, "DURABLE-CATALOG-XYZ") {
		t.Fatalf("cap path must NOT inline catalog on prompt; got %q", agent.lastPromptTxt)
	}
	if agent.lastPromptTxt != "hello" {
		t.Fatalf("user prompt mangled: %q", agent.lastPromptTxt)
	}
}

// TestSystemPrompt_FallbackPath: agent doesn't advertise the cap, router
// must NOT pass _meta blocks and MUST inline the catalog on the first
// session/prompt only.
func TestSystemPrompt_FallbackPath(t *testing.T) {
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	// caps zero — no SystemPrompt support.

	rtr := mustRouterWithSP(t, agent, "DURABLE-CATALOG-XYZ")
	sink := newCollector()
	if err := rtr.Prompt(context.Background(), "c-fb", "u",
		[]Turn{{Role: "user", Content: "hello"}}, Options{}, sink); err != nil {
		t.Fatal(err)
	}

	if agent.lastSysBlocks != nil {
		t.Fatalf("fallback path must not send _meta blocks; got %+v", agent.lastSysBlocks)
	}
	if !strings.Contains(agent.lastPromptTxt, "DURABLE-CATALOG-XYZ") {
		t.Fatalf("fallback path must inline catalog; got %q", agent.lastPromptTxt)
	}
	if !strings.Contains(agent.lastPromptTxt, "hello") {
		t.Fatalf("user message lost in inline fallback: %q", agent.lastPromptTxt)
	}

	// Second turn on the same conv must NOT re-inline (one-shot only).
	sink2 := newCollector()
	if err := rtr.Prompt(context.Background(), "c-fb", "u",
		[]Turn{{Role: "user", Content: "again"}}, Options{}, sink2); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(agent.lastPromptTxt, "DURABLE-CATALOG-XYZ") {
		t.Fatalf("catalog re-injected on later turn: %q", agent.lastPromptTxt)
	}
	if agent.lastPromptTxt != "again" {
		t.Fatalf("turn-2 prompt wrong: %q", agent.lastPromptTxt)
	}
}

// TestSystemPrompt_ResumeReinjects: on the fallback path, a resumed
// session re-injects the catalog on its first prompt (since intra-
// session compaction may have lost it).
func TestSystemPrompt_ResumeReinjects(t *testing.T) {
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	agent.caps = client.Caps{ListSessions: true, ResumeSession: true}
	agent.listResult = []client.SessionInfo{{SessionId: "prev-sess"}}

	rtr := mustRouterWithSP(t, agent, "DURABLE-CATALOG-XYZ")
	sink := newCollector()
	if err := rtr.Prompt(context.Background(), "c-res", "u",
		[]Turn{{Role: "user", Content: "hi"}}, Options{}, sink); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(agent.lastPromptTxt, "DURABLE-CATALOG-XYZ") {
		t.Fatalf("resume fallback didn't re-inject: %q", agent.lastPromptTxt)
	}
}

// TestSystemPrompt_ResumeCapPathTrustsAgent: when the agent advertises
// the cap, the resume path trusts the agent to restore system prompt on
// session/load (per RFD); router must NOT inline on next prompt.
func TestSystemPrompt_ResumeCapPathTrustsAgent(t *testing.T) {
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	agent.caps = client.Caps{ListSessions: true, ResumeSession: true, SystemPrompt: true}
	agent.listResult = []client.SessionInfo{{SessionId: "prev-sess"}}

	rtr := mustRouterWithSP(t, agent, "DURABLE-CATALOG-XYZ")
	sink := newCollector()
	if err := rtr.Prompt(context.Background(), "c-rescap", "u",
		[]Turn{{Role: "user", Content: "hi"}}, Options{}, sink); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(agent.lastPromptTxt, "DURABLE-CATALOG-XYZ") {
		t.Fatalf("cap-path resume must trust agent; inlined anyway: %q", agent.lastPromptTxt)
	}
}

func TestSystemPromptProvider_CalledPerNewSession(t *testing.T) {
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	agent.caps = client.Caps{SystemPrompt: true}

	catalogs := []string{"CATALOG-ONE", "CATALOG-TWO"}
	calls := 0
	rtr := mustRouterWithSPProvider(t, agent, func() string {
		if calls >= len(catalogs) {
			t.Fatalf("system prompt provider called too many times: %d", calls+1)
		}
		out := catalogs[calls]
		calls++
		return out
	})

	if err := rtr.Prompt(context.Background(), "c-one", "u",
		[]Turn{{Role: "user", Content: "first"}}, Options{}, newCollector()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("provider calls after first session=%d want 1", calls)
	}
	if len(agent.lastSysBlocks) != 1 || agent.lastSysBlocks[0].Text == nil ||
		!strings.Contains(agent.lastSysBlocks[0].Text.Text, "CATALOG-ONE") {
		t.Fatalf("first session got wrong system prompt: %+v", agent.lastSysBlocks)
	}

	// Existing hot session keeps its original injected prompt; provider is
	// not polled on every turn.
	if err := rtr.Prompt(context.Background(), "c-one", "u",
		[]Turn{{Role: "user", Content: "same session"}}, Options{}, newCollector()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("provider called for hot session turn: calls=%d", calls)
	}

	if err := rtr.Prompt(context.Background(), "c-two", "u",
		[]Turn{{Role: "user", Content: "second"}}, Options{}, newCollector()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("provider calls after second session=%d want 2", calls)
	}
	if len(agent.lastSysBlocks) != 1 || agent.lastSysBlocks[0].Text == nil ||
		!strings.Contains(agent.lastSysBlocks[0].Text.Text, "CATALOG-TWO") {
		t.Fatalf("second session got wrong system prompt: %+v", agent.lastSysBlocks)
	}
}

func TestSystemPromptProvider_CalledAgainAfterGCEviction(t *testing.T) {
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	agent.caps = client.Caps{SystemPrompt: true}

	clock := time.Unix(0, 0)
	catalogs := []string{"CATALOG-BEFORE-GC", "CATALOG-AFTER-GC"}
	calls := 0
	rtr := mustRouterWithConfig(t, agent, Config{
		SessionTTL: time.Second,
		Now:        func() time.Time { return clock },
		SystemPromptProvider: func() string {
			if calls >= len(catalogs) {
				t.Fatalf("system prompt provider called too many times: %d", calls+1)
			}
			out := catalogs[calls]
			calls++
			return out
		},
	})

	if err := rtr.Prompt(context.Background(), "c-gc", "u",
		[]Turn{{Role: "user", Content: "before"}}, Options{}, newCollector()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("provider calls before gc=%d want 1", calls)
	}

	clock = clock.Add(2 * time.Second)
	rtr.gcOnce()
	if rtr.Len() != 0 {
		t.Fatalf("session was not evicted")
	}

	if err := rtr.Prompt(context.Background(), "c-gc", "u",
		[]Turn{{Role: "user", Content: "after"}}, Options{}, newCollector()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("provider calls after gc=%d want 2", calls)
	}
	if len(agent.lastSysBlocks) != 1 || agent.lastSysBlocks[0].Text == nil ||
		!strings.Contains(agent.lastSysBlocks[0].Text.Text, "CATALOG-AFTER-GC") {
		t.Fatalf("post-gc session got wrong system prompt: %+v", agent.lastSysBlocks)
	}
}

func TestSystemPromptProvider_EmptyDisablesInjection(t *testing.T) {
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	agent.caps = client.Caps{SystemPrompt: true}

	rtr := mustRouterWithSPProvider(t, agent, func() string { return "" })
	if err := rtr.Prompt(context.Background(), "c-empty", "u",
		[]Turn{{Role: "user", Content: "hi"}}, Options{}, newCollector()); err != nil {
		t.Fatal(err)
	}
	if agent.lastSysBlocks != nil {
		t.Fatalf("empty provider must not deliver _meta blocks; got %+v", agent.lastSysBlocks)
	}
	if strings.Contains(agent.lastPromptTxt, "Relay & Transport Contract") {
		t.Fatalf("empty provider must not prepend transport contract; got %q", agent.lastPromptTxt)
	}
	if agent.lastPromptTxt != "hi" {
		t.Fatalf("user prompt mangled: %q", agent.lastPromptTxt)
	}
}

func newCollector() *captureSink { return &captureSink{} }

func mustRouterWithSP(t *testing.T, a Agent, sp string) *Router {
	t.Helper()
	return mustRouterWithSPProvider(t, a, func() string { return sp })
}

func mustRouterWithSPProvider(t *testing.T, a Agent, provider func() string) *Router {
	t.Helper()
	return mustRouterWithConfig(t, a, Config{SystemPromptProvider: provider})
}

func mustRouterWithConfig(t *testing.T, a Agent, cfg Config) *Router {
	t.Helper()
	cfg.Agent = a
	if cfg.StateDir == "" {
		cfg.StateDir = t.TempDir()
	}
	if cfg.SessionTTL == 0 {
		cfg.SessionTTL = time.Hour
	}
	rtr, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return rtr
}

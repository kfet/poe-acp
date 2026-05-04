package router

import (
	"context"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/poe-acp/internal/acpclient"
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
	agent.caps = acpclient.Caps{SystemPrompt: true}

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
	agent.caps = acpclient.Caps{ListSessions: true, ResumeSession: true}
	agent.listResult = []acpclient.SessionInfo{{SessionId: "prev-sess"}}

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
	agent.caps = acpclient.Caps{ListSessions: true, ResumeSession: true, SystemPrompt: true}
	agent.listResult = []acpclient.SessionInfo{{SessionId: "prev-sess"}}

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

func newCollector() *captureSink { return &captureSink{} }

func mustRouterWithSP(t *testing.T, a Agent, sp string) *Router {
	t.Helper()
	rtr, err := New(Config{
		Agent:        a,
		StateDir:     t.TempDir(),
		SessionTTL:   time.Hour,
		SystemPrompt: sp,
	})
	if err != nil {
		t.Fatal(err)
	}
	return rtr
}

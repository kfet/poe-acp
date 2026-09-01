package router

import (
	"context"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
	"github.com/kfet/poe-acp/internal/config"
)

func newCmdRouter(t *testing.T, agent Agent, defModel string) *Router {
	t.Helper()
	r, err := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour,
		Defaults: Options{Model: defModel}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func TestAvailableModels(t *testing.T) {
	a := newFakeAgent(nil)
	a.models = []client.ModelInfo{{ID: "p/a"}, {ID: "p/b"}}
	a.currentModelID = "p/a"
	r := newCmdRouter(t, a, "p/a")
	m, cur := r.AvailableModels()
	if len(m) != 2 || cur != "p/a" {
		t.Fatalf("got %v cur=%q", m, cur)
	}
}

func TestStatusFor(t *testing.T) {
	a := newFakeAgent(nil)
	a.models = []client.ModelInfo{{ID: "p/a"}, {ID: "p/b"}}
	r := newCmdRouter(t, a, "p/a")

	s := r.StatusFor("c1")
	if s.EffectiveModel != "p/a" || s.OverrideModel != "" || s.HasSession || s.ModelsAvailable != 2 {
		t.Fatalf("default status wrong: %+v", s)
	}
	if err := r.SetModelOverride("c1", "p/b"); err != nil {
		t.Fatal(err)
	}
	s = r.StatusFor("c1")
	if s.EffectiveModel != "p/b" || s.OverrideModel != "p/b" {
		t.Fatalf("override status wrong: %+v", s)
	}
	// empty convID resolves to "default"
	if r.StatusFor("").EffectiveModel != "p/a" {
		t.Fatalf("default conv status")
	}
}

func TestSetModelOverride(t *testing.T) {
	a := newFakeAgent(nil)
	a.models = []client.ModelInfo{{ID: "p/a"}}
	r := newCmdRouter(t, a, "p/a")

	if err := r.SetModelOverride("c1", "nope"); err == nil {
		t.Fatal("expected error for unknown model")
	}
	if err := r.SetModelOverride("c1", "p/a"); err != nil {
		t.Fatalf("valid model: %v", err)
	}
	// No model list yet → validation skipped (override still stored).
	b := newFakeAgent(nil)
	rb := newCmdRouter(t, b, "")
	if err := rb.SetModelOverride("c1", "anything"); err != nil {
		t.Fatalf("empty-models should skip validation: %v", err)
	}
	// empty convID resolves to "default".
	if err := rb.SetModelOverride("", "anything"); err != nil {
		t.Fatalf("empty conv override: %v", err)
	}
}

func TestPromptAppliesOverride(t *testing.T) {
	a := newFakeAgent(func(_ context.Context, ag *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		ag.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	a.models = []client.ModelInfo{{ID: "p/over"}}
	r := newCmdRouter(t, a, "") // no default model
	if err := r.SetModelOverride("c1", "p/over"); err != nil {
		t.Fatal(err)
	}
	sink := &captureSink{}
	if err := r.Prompt(context.Background(), "c1", "u", []Turn{{Role: "user", Content: "hi"}}, Options{}, sink); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	a.mu.Lock()
	got := a.lastSetModel
	a.mu.Unlock()
	if got != "p/over" {
		t.Fatalf("override not applied to agent: lastSetModel=%q", got)
	}
}

func TestResetSession(t *testing.T) {
	r := newCmdRouter(t, newFakeAgent(nil), "p/a")

	// No live session → nil.
	if err := r.ResetSession("c1"); err != nil {
		t.Fatalf("reset absent: %v", err)
	}
	// empty convID resolves to "default" (also absent → nil).
	if err := r.ResetSession(""); err != nil {
		t.Fatalf("reset empty conv: %v", err)
	}

	// Idle session → evicted.
	idle := &sessionState{convID: "c1", queue: newSessionQueue(),
		drainStop: make(chan struct{}), runStop: make(chan struct{})}
	r.mu.Lock()
	r.sessions["c1"] = idle
	r.mu.Unlock()
	if err := r.ResetSession("c1"); err != nil {
		t.Fatalf("reset idle: %v", err)
	}
	r.mu.Lock()
	_, still := r.sessions["c1"]
	r.mu.Unlock()
	if still {
		t.Fatal("idle session not evicted")
	}

	// Busy session (queued turn) → ErrSessionBusy.
	busy := &sessionState{convID: "c2", queue: newSessionQueue(),
		drainStop: make(chan struct{}), runStop: make(chan struct{})}
	busy.queue.push(&turnReq{kind: turnUser, done: make(chan struct{})})
	r.mu.Lock()
	r.sessions["c2"] = busy
	r.mu.Unlock()
	if err := r.ResetSession("c2"); err != ErrSessionBusy {
		t.Fatalf("expected ErrSessionBusy, got %v", err)
	}
}

func TestAgentCommands(t *testing.T) {
	a := newFakeAgent(nil)
	a.agentCmds = []client.CommandInfo{{Name: "reload"}, {Name: "compact"}}
	r := newCmdRouter(t, a, "")
	cs := r.AgentCommands()
	if len(cs) != 2 || cs[0].Name != "reload" {
		t.Fatalf("AgentCommands: %v", cs)
	}
}

// TestAvailableModels_ModelOrder pins the single-source invariant for
// operator model ordering: AvailableModels applies Config.ModelOrder,
// so httpsrv's ParseOptions list matches the one paramctl.Build used
// for the schema's default_value. Guards against the pinned_models
// regression where the runtime path saw raw agent probe order.
func TestAvailableModels_ModelOrder(t *testing.T) {
	a := newFakeAgent(nil)
	a.models = []client.ModelInfo{
		{ID: "openrouter/aion-labs/aion-2.0"},
		{ID: "anthropic/claude-opus-5"},
		{ID: "openrouter/stealth/ox-alpha"},
	}
	a.currentModelID = "anthropic/claude-opus-5"
	pin := func(models []client.ModelInfo) []client.ModelInfo {
		return config.OrderPinned(models, []string{"openrouter/stealth/ox-alpha"})
	}
	r := newCmdRouter(t, a, "")
	r.cfg.ModelOrder = pin

	m, cur := r.AvailableModels()
	if len(m) != 3 || m[0].ID != "openrouter/stealth/ox-alpha" || cur != "anthropic/claude-opus-5" {
		t.Fatalf("pin not applied at source: %v cur=%q", m, cur)
	}

	// The exact production shape: ParseOptions over the router's list
	// must land on the pinned model for a provider-only parameter,
	// matching what the schema built from the same list advertises.
	defs := Options{Model: "anthropic/claude-opus-5"}
	opts := ParseOptions(map[string]any{"provider": "openrouter"}, defs, m)
	if opts.Model != "openrouter/stealth/ox-alpha" {
		t.Fatalf("provider-only param resolved %q, want pinned ox-alpha", opts.Model)
	}
	if DefaultModelForProvider(m, "openrouter", defs.Model) != opts.Model {
		t.Fatalf("schema default and runtime fallback disagree")
	}
}

// TestOnTurnEndRunsAfterTheSessionIsIdle pins the ordering the
// agent→relay loopback depends on: a `new_session` the agent asked for
// mid-turn is applied by the end-of-turn hook, and ResetSession refuses
// a busy session — so the hook must not run until the turn has left the
// queue's in-flight state.
func TestOnTurnEndRunsAfterTheSessionIsIdle(t *testing.T) {
	var (
		mu    sync.Mutex
		convs []string
		errs  []error
	)
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "hi")
		return acp.StopReasonEndTurn, nil
	})
	r := newCmdRouter(t, agent, "p/a")
	r.cfg.OnTurnEnd = func(convID string) {
		mu.Lock()
		convs = append(convs, convID)
		// The whole point: a reset issued from here must succeed.
		errs = append(errs, r.ResetSession(convID))
		mu.Unlock()
	}
	if err := r.Prompt(context.Background(), "c-hook", "u",
		[]Turn{{Role: "user", Content: "hello"}}, Options{}, &captureSink{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(convs) != 1 || convs[0] != "c-hook" {
		t.Fatalf("OnTurnEnd calls = %v, want one for c-hook", convs)
	}
	if errs[0] != nil {
		t.Fatalf("reset from the end-of-turn hook failed: %v — the hook ran too early", errs[0])
	}
}

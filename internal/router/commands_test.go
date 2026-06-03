package router

import (
	"context"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
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

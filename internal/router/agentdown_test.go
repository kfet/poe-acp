package router

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
)

// userTurns is the minimal query shape for a one-shot user turn.
func userTurns(text string) []Turn {
	return []Turn{{Role: "user", Content: text}}
}

// The 2026-08-28 incident: the agent's ssh pipe dropped, so session/new
// failed with a JSON-RPC internal error carrying "write |1: broken pipe".
// The relay emitted only a Poe `error` event — which Poe does not render —
// so every turn looked like an empty reply. The user must now get visible
// text saying the agent is down.
func TestPromptOnDeadAgentTellsTheUser(t *testing.T) {
	t.Parallel()
	agent := newFakeAgent(nil)
	agent.newSessErr = errors.New(`{"code":-32603,"message":"Internal error","data":{"error":"write |1: broken pipe"}}`)
	agent.die(&exec.ExitError{})
	r := mustRouter(t, agent)

	sink := &captureSink{}
	err := r.Prompt(context.Background(), "conv-dead", "u1", userTurns("hi"), Options{}, sink)
	if err == nil {
		t.Fatal("Prompt: want error")
	}
	if got := sink.text.String(); !strings.Contains(got, "backing agent is not running") {
		t.Fatalf("user text = %q, want the agent-down message", got)
	}
	if sink.errText != "" {
		t.Fatalf("error event = %q, want none (Poe does not render it)", sink.errText)
	}
	if !sink.done {
		t.Fatal("stream not finalised")
	}
}

// A live agent that simply fails the call must keep the old behaviour:
// diagnostic error event, no misleading outage message.
func TestPromptOnLiveAgentKeepsErrorEvent(t *testing.T) {
	t.Parallel()
	agent := newFakeAgent(nil)
	agent.newSessErr = errors.New("boom")
	r := mustRouterWithConfig(t, agent, Config{AgentDeathGrace: time.Millisecond})

	sink := &captureSink{}
	if err := r.Prompt(context.Background(), "conv-live", "u1", userTurns("hi"), Options{}, sink); err == nil {
		t.Fatal("Prompt: want error")
	}
	if !strings.Contains(sink.errText, "relay: acp new session: boom") {
		t.Fatalf("error event = %q, want the relay diagnostic", sink.errText)
	}
	if got := sink.text.String(); strings.Contains(got, "backing agent") {
		t.Fatalf("user text = %q, want no outage claim", got)
	}
}

// The agent can also die mid-conversation, with the session already
// established: session/prompt is then the call that fails.
func TestPromptRPCFailureOnDeadAgentTellsTheUser(t *testing.T) {
	t.Parallel()
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, _ acp.SessionId, _ string) (acp.StopReason, error) {
		a.die(&exec.ExitError{})
		return "", errors.New(`{"code":-32603,"data":{"error":"write |1: broken pipe"}}`)
	})
	r := mustRouter(t, agent)

	sink := &captureSink{}
	if err := r.Prompt(context.Background(), "conv-mid", "u1", userTurns("hi"), Options{}, sink); err == nil {
		t.Fatal("Prompt: want error")
	}
	if got := sink.text.String(); !strings.Contains(got, "backing agent is not running") {
		t.Fatalf("user text = %q, want the agent-down message", got)
	}
	if sink.errText != "" {
		t.Fatalf("error event = %q, want none", sink.errText)
	}
}

// The write to a dying agent can fail a beat BEFORE the reaper observes
// the exit. agentGone must wait out that window rather than misreport a
// dead agent as a generic relay error.
func TestAgentGoneWaitsOutTheExitRace(t *testing.T) {
	t.Parallel()
	agent := newFakeAgent(nil)
	r := mustRouterWithConfig(t, agent, Config{AgentDeathGrace: 5 * time.Second})

	// Err() is still nil; the exit lands while agentGone is waiting.
	go agent.die(&exec.ExitError{})
	if !r.agentGone() {
		t.Fatal("agentGone = false, want true once the exit lands")
	}
}

// A deliberate Close is still "gone" as far as a turn is concerned — the
// process cannot serve it either way — but a live agent is never gone.
func TestAgentGoneLiveAgent(t *testing.T) {
	t.Parallel()
	agent := newFakeAgent(nil)
	r := mustRouterWithConfig(t, agent, Config{AgentDeathGrace: time.Millisecond})
	if r.agentGone() {
		t.Fatal("agentGone = true for a live agent")
	}
	agent.die(client.ErrAgentClosed)
	if !r.agentGone() {
		t.Fatal("agentGone = false after the agent exited")
	}
}

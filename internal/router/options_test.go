package router

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

func mustRouter(t *testing.T, agent Agent) *Router {
	t.Helper()
	r, err := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	return r
}

func TestParseOptions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   map[string]any
		want Options
	}{
		{"nil", nil, Options{}},
		{"empty", map[string]any{}, Options{}},
		{
			"all valid",
			map[string]any{"model": "anthropic/claude-sonnet-4-5", "thinking": "high", "hide_thinking": true},
			Options{Model: "anthropic/claude-sonnet-4-5", Thinking: "high", HideThinking: true},
		},
		{
			"thinking off accepted",
			map[string]any{"thinking": "off"},
			Options{Thinking: "off"},
		},
		{
			"thinking none rejected (fir uses off)",
			map[string]any{"thinking": "none"},
			Options{},
		},
		{
			"unknown key dropped",
			map[string]any{"model": "x", "permission": "deny-all"},
			Options{Model: "x"},
		},
		{
			"invalid thinking dropped",
			map[string]any{"thinking": "bogus"},
			Options{},
		},
		{
			"wrong types dropped",
			map[string]any{"model": 42, "thinking": true, "hide_thinking": "yes"},
			Options{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseOptions(tc.in)
			if got != tc.want {
				t.Fatalf("got %#v want %#v", got, tc.want)
			}
		})
	}
}

// optsAgent is a fakeAgent with set-tracking.
type optsAgent struct {
	*fakeAgent

	mu             sync.Mutex
	setModelCalls  []string
	setConfigCalls [][2]string
	setModelErr    error
	setConfigErr   error
}

func newOptsAgent() *optsAgent {
	return &optsAgent{
		fakeAgent: newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
			a.emit(sid, "ok")
			return acp.StopReasonEndTurn, nil
		}),
	}
}

func (a *optsAgent) SetModel(_ context.Context, _ acp.SessionId, modelID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.setModelCalls = append(a.setModelCalls, modelID)
	return a.setModelErr
}

func (a *optsAgent) SetConfigOption(_ context.Context, _ acp.SessionId, configID, value string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.setConfigCalls = append(a.setConfigCalls, [2]string{configID, value})
	return a.setConfigErr
}

func TestRouter_AppliesOptionsAndDiffs(t *testing.T) {
	t.Parallel()
	agent := newOptsAgent()
	r := mustRouter(t, agent)
	turns := []Turn{{Role: "user", Content: "hi"}}

	// First prompt: model + thinking should both apply.
	if err := r.Prompt(context.Background(), "c1", "u", turns, Options{Model: "anth/claude", Thinking: "high"}, &captureSink{}); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	agent.mu.Lock()
	if len(agent.setModelCalls) != 1 || agent.setModelCalls[0] != "anth/claude" {
		t.Fatalf("set_model calls = %v", agent.setModelCalls)
	}
	if len(agent.setConfigCalls) != 1 || agent.setConfigCalls[0] != [2]string{"thinking_level", "high"} {
		t.Fatalf("set_config calls = %v", agent.setConfigCalls)
	}
	agent.mu.Unlock()

	// Second prompt with same options: no new calls.
	if err := r.Prompt(context.Background(), "c1", "u", turns, Options{Model: "anth/claude", Thinking: "high"}, &captureSink{}); err != nil {
		t.Fatalf("prompt2: %v", err)
	}
	agent.mu.Lock()
	if len(agent.setModelCalls) != 1 || len(agent.setConfigCalls) != 1 {
		t.Fatalf("expected no new option calls; model=%v config=%v", agent.setModelCalls, agent.setConfigCalls)
	}
	agent.mu.Unlock()

	// Third prompt: change thinking only.
	if err := r.Prompt(context.Background(), "c1", "u", turns, Options{Model: "anth/claude", Thinking: "low"}, &captureSink{}); err != nil {
		t.Fatalf("prompt3: %v", err)
	}
	agent.mu.Lock()
	if len(agent.setModelCalls) != 1 {
		t.Fatalf("set_model unexpectedly called again: %v", agent.setModelCalls)
	}
	if len(agent.setConfigCalls) != 2 || agent.setConfigCalls[1] != [2]string{"thinking_level", "low"} {
		t.Fatalf("set_config calls = %v", agent.setConfigCalls)
	}
	agent.mu.Unlock()
}

func TestRouter_HideThinkingSuppressesThoughtChunks(t *testing.T) {
	t.Parallel()
	agent := newOptsAgent()
	// Override prompt to emit a thought chunk + a message chunk.
	agent.fakeAgent.onPrompt = func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		// thought
		a.emitUpdate(sid, acp.SessionUpdate{
			AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{
				Content: acp.TextBlock("deep thoughts"),
			},
		})
		a.emit(sid, "answer")
		return acp.StopReasonEndTurn, nil
	}
	r := mustRouter(t, agent)
	turns := []Turn{{Role: "user", Content: "hi"}}

	// hide_thinking=true → only "answer" should reach the sink.
	sink := &captureSink{}
	if err := r.Prompt(context.Background(), "c1", "u", turns, Options{HideThinking: true}, sink); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if got := sink.text.String(); got != "answer" {
		t.Fatalf("hide_thinking=true: got %q want %q", got, "answer")
	}

	// hide_thinking=false → both should reach the sink.
	sink2 := &captureSink{}
	if err := r.Prompt(context.Background(), "c2", "u", turns, Options{HideThinking: false}, sink2); err != nil {
		t.Fatalf("prompt2: %v", err)
	}
	got := sink2.text.String()
	if !strings.Contains(got, "deep thoughts") || !strings.Contains(got, "answer") {
		t.Fatalf("hide_thinking=false: got %q want both", got)
	}
}

func TestRouter_MultiChunkThoughtsRenderAsOneBlockquote(t *testing.T) {
	t.Parallel()
	agent := newOptsAgent()
	// Stream a thought as 3 chunks, then a message chunk, then another
	// thought chunk — exercise both "first thought" and "back to thought
	// after a message" transitions.
	agent.fakeAgent.onPrompt = func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		emit := func(text string) {
			a.emitUpdate(sid, acp.SessionUpdate{
				AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{
					Content: acp.TextBlock(text),
				},
			})
		}
		emit("thinking ")
		emit("about ")
		emit("things")
		a.emit(sid, "the answer")
		emit("more thoughts")
		return acp.StopReasonEndTurn, nil
	}
	r := mustRouter(t, agent)
	sink := &captureSink{}
	if err := r.Prompt(context.Background(), "c1", "u", []Turn{{Role: "user", Content: "hi"}}, Options{}, sink); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	got := sink.text.String()
	want := "> _Thinking…_\n> thinking about things\n\nthe answer\n\n> _Thinking…_\n> more thoughts"
	if got != want {
		t.Fatalf("blockquote rendering wrong:\n got: %q\nwant: %q", got, want)
	}
}

func TestRouter_ThoughtChunkPreservesNewlinesAsBlockquote(t *testing.T) {
	t.Parallel()
	agent := newOptsAgent()
	agent.fakeAgent.onPrompt = func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emitUpdate(sid, acp.SessionUpdate{
			AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{
				Content: acp.TextBlock("line1\nline2"),
			},
		})
		return acp.StopReasonEndTurn, nil
	}
	r := mustRouter(t, agent)
	sink := &captureSink{}
	if err := r.Prompt(context.Background(), "c1", "u", []Turn{{Role: "user", Content: "hi"}}, Options{}, sink); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if got, want := sink.text.String(), "> _Thinking…_\n> line1\n> line2"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestRouter_ApplyOptionsFailureSurfacesText(t *testing.T) {
	t.Parallel()
	agent := newOptsAgent()
	agent.setModelErr = stringErr("provider down")
	r := mustRouter(t, agent)

	sink := &captureSink{}
	if err := r.Prompt(context.Background(), "c1", "u",
		[]Turn{{Role: "user", Content: "hi"}},
		Options{Model: "anth/x"},
		sink,
	); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	got := sink.text.String()
	if !strings.Contains(got, "option not applied") || !strings.Contains(got, "ok") {
		t.Fatalf("expected error notice + agent text, got %q", got)
	}
	// applied.Model should remain empty so a retry is attempted next turn.
	r.mu.Lock()
	st := r.sessions["c1"]
	r.mu.Unlock()
	if st.applied.Model != "" {
		t.Fatalf("applied.Model should be empty after failed SetModel, got %q", st.applied.Model)
	}
}

type stringErr string

func (e stringErr) Error() string { return string(e) }

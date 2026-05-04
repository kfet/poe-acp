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
		name     string
		in       map[string]any
		defaults Options
		want     Options
	}{
		{"nil/no defaults", nil, Options{}, Options{}},
		{"empty/no defaults", map[string]any{}, Options{}, Options{}},
		{
			"empty params overlays nothing — defaults survive",
			map[string]any{},
			Options{Model: "anth/sonnet", Thinking: "medium"},
			Options{Model: "anth/sonnet", Thinking: "medium"},
		},
		{
			"nil params with defaults",
			nil,
			Options{Model: "anth/sonnet", Thinking: "medium", HideThinking: false},
			Options{Model: "anth/sonnet", Thinking: "medium", HideThinking: false},
		},
		{
			"all valid overrides defaults",
			map[string]any{"model": "anthropic/claude-sonnet-4-5", "thinking": "high", "hide_thinking": true},
			Options{Model: "anth/sonnet", Thinking: "medium"},
			Options{Model: "anthropic/claude-sonnet-4-5", Thinking: "high", HideThinking: true},
		},
		{
			"thinking off accepted",
			map[string]any{"thinking": "off"},
			Options{},
			Options{Thinking: "off"},
		},
		{
			"thinking none rejected — default survives",
			map[string]any{"thinking": "none"},
			Options{Thinking: "medium"},
			Options{Thinking: "medium"},
		},
		{
			"unknown key dropped, default survives for untouched fields",
			map[string]any{"model": "x", "permission": "deny-all"},
			Options{Thinking: "medium"},
			Options{Model: "x", Thinking: "medium"},
		},
		{
			"invalid thinking dropped — default survives",
			map[string]any{"thinking": "bogus"},
			Options{Thinking: "medium"},
			Options{Thinking: "medium"},
		},
		{
			"wrong types dropped — defaults survive",
			map[string]any{"model": 42, "thinking": true, "hide_thinking": "yes"},
			Options{Model: "anth/sonnet", Thinking: "medium"},
			Options{Model: "anth/sonnet", Thinking: "medium"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseOptions(tc.in, tc.defaults)
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

// TestRouter_ThinkingRejectionIsSilent pins the fix for non-reasoning
// models (e.g. kimi-k2.6) that reject any thinking_level other than
// "off". The relay should:
//  1. NOT surface a user-visible "option not applied" notice for
//     thinking_level rejection (it's expected, not an error).
//  2. Mark applied.Thinking anyway so it doesn't retry every turn.
//  3. Still proceed with the prompt normally.
func TestRouter_ThinkingRejectionIsSilent(t *testing.T) {
	t.Parallel()
	agent := newOptsAgent()
	agent.setConfigErr = stringErr(`set_config thinking_level=high: {"code":-32603,"message":"Internal error","data":{"error":"invalid thinking level: high"}}`)
	r := mustRouter(t, agent)

	sink := &captureSink{}
	if err := r.Prompt(context.Background(), "c1", "u",
		[]Turn{{Role: "user", Content: "hi"}},
		Options{Thinking: "high"},
		sink,
	); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	got := sink.text.String()
	if strings.Contains(got, "option not applied") {
		t.Fatalf("thinking rejection should be silent, got %q", got)
	}
	// applied.Thinking should be set so we don't retry next turn.
	r.mu.Lock()
	st := r.sessions["c1"]
	r.mu.Unlock()
	if st.applied.Thinking != "high" {
		t.Fatalf("applied.Thinking should be %q after rejection (to suppress retry), got %q", "high", st.applied.Thinking)
	}

	// Second prompt with same value: no further SetConfigOption call.
	calls := len(agent.setConfigCalls)
	if err := r.Prompt(context.Background(), "c1", "u",
		[]Turn{{Role: "user", Content: "hi again"}},
		Options{Thinking: "high"},
		&captureSink{},
	); err != nil {
		t.Fatalf("prompt2: %v", err)
	}
	if len(agent.setConfigCalls) != calls {
		t.Fatalf("expected no retry of SetConfigOption; calls went %d -> %d", calls, len(agent.setConfigCalls))
	}
}

type stringErr string

func (e stringErr) Error() string { return string(e) }

// TestRouter_AppliesDefaultsOnEmptyParams pins the bug fix: when Poe
// sends a query with empty `parameters` (which it does on the first
// turn — `default_value`s are UI-only), the router must still apply
// the configured defaults so the agent matches what the UI promised.
func TestRouter_AppliesDefaultsOnEmptyParams(t *testing.T) {
	t.Parallel()
	agent := newOptsAgent()
	r, err := New(Config{
		Agent:      agent,
		StateDir:   t.TempDir(),
		SessionTTL: time.Hour,
		Defaults:   Options{Model: "anth/sonnet", Thinking: "medium"},
	})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}

	// Caller hands ParseOptions empty params + the router's defaults.
	opts := ParseOptions(nil, r.Defaults())
	if err := r.Prompt(context.Background(), "c1", "u",
		[]Turn{{Role: "user", Content: "hi"}}, opts, &captureSink{}); err != nil {
		t.Fatalf("prompt: %v", err)
	}

	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.setModelCalls) != 1 || agent.setModelCalls[0] != "anth/sonnet" {
		t.Fatalf("expected SetModel(anth/sonnet) once; got %v", agent.setModelCalls)
	}
	if len(agent.setConfigCalls) != 1 || agent.setConfigCalls[0] != [2]string{"thinking_level", "medium"} {
		t.Fatalf("expected SetConfigOption(thinking_level=medium) once; got %v", agent.setConfigCalls)
	}
}

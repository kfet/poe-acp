package router

import (
	"context"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/poe-acp/internal/statusline"
)

// promptWith runs a single turn against a fake agent that emits the
// given session updates, and returns the capturing sink.
func promptWith(t *testing.T, convID string, opts Options, emit func(a *fakeAgent, sid acp.SessionId)) *captureSink {
	t.Helper()
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		emit(a, sid)
		return acp.StopReasonEndTurn, nil
	})
	r, err := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	sink := &captureSink{}
	if err := r.Prompt(context.Background(), convID, "u", []Turn{{Role: "user", Content: "hi"}}, opts, sink); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	return sink
}

// body returns the sink's accumulated answer text under its lock.
func body(s *captureSink) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.text.String()
}

func toolCall(id, title string, kind acp.ToolKind) acp.SessionUpdate {
	return acp.SessionUpdate{ToolCall: &acp.SessionUpdateToolCall{ToolCallId: acp.ToolCallId(id), Title: title, Kind: kind}}
}

// TestToolLines_DurableAndNewlineDelimited covers the core rendering
// contract: one durable blockquote line per tool_call, consecutive lines
// joined by a single newline (one quote block, one line each), a blank
// line before following message text so it is not sucked into the quote,
// and tool_call_update contributing NOTHING durable.
func TestToolLines_DurableAndNewlineDelimited(t *testing.T) {
	sink := promptWith(t, "c-tools", Options{ShowTools: true}, func(a *fakeAgent, sid acp.SessionId) {
		a.emitUpdate(sid, toolCall("t1", "go test ./...", acp.ToolKindExecute))
		a.emitUpdate(sid, toolCall("t2", "Read router.go", acp.ToolKindRead))
		k := acp.ToolKindRead
		a.emitUpdate(sid, acp.SessionUpdate{ToolCallUpdate: &acp.SessionToolCallUpdate{
			ToolCallId: "t2", Kind: &k,
		}})
		a.emit(sid, "all green")
	})
	want := "> `🔧 go test ./...`\n> `📖 Read router.go`\n\nall green"
	if got := body(sink); got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	sink.mu.Lock()
	firstCalled, labels := sink.firstCalled, sink.toolLabels
	sink.mu.Unlock()
	if !firstCalled {
		t.Fatal("a tool line is user-visible output: FirstChunk must fire")
	}
	// The spinner label path is untouched by show_tools.
	if len(labels) != 3 {
		t.Fatalf("toolLabels = %v, want 3 (2 calls + 1 update)", labels)
	}
}

// TestToolLines_SpacingAroundMessagesAndThoughts pins the chunkTool
// transitions in both directions: message → tool, tool → thought, and
// thought → tool.
func TestToolLines_SpacingAroundMessagesAndThoughts(t *testing.T) {
	sink := promptWith(t, "c-mix", Options{ShowTools: true}, func(a *fakeAgent, sid acp.SessionId) {
		a.emit(sid, "before ")
		a.emitUpdate(sid, toolCall("t1", "", acp.ToolKindSearch))
		a.emitUpdate(sid, acp.SessionUpdate{AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{
			Content: acp.TextBlock("pondering"),
		}})
		a.emitUpdate(sid, toolCall("t2", "", ""))
		a.emit(sid, "after")
	})
	want := "before \n\n> `🔍 search`" +
		"\n\n> _Thinking…_\n> pondering" +
		"\n\n> `🛠️ tool`" +
		"\n\nafter"
	if got := body(sink); got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

// TestToolLines_HostileTitleCannotBreakTheFrame feeds a title carrying
// newlines, backticks and a reserved --flag token. The rendered line
// must stay a single blockquote line, keep its code span balanced, and
// carry a defused flag token.
func TestToolLines_HostileTitleCannotBreakTheFrame(t *testing.T) {
	hostile := "  evil\n\n`` ` ``\n> **pwned**\n--model x  "
	sink := promptWith(t, "c-hostile", Options{ShowTools: true}, func(a *fakeAgent, sid acp.SessionId) {
		a.emitUpdate(sid, toolCall("t1", hostile, acp.ToolKindExecute))
	})
	got := body(sink)
	if strings.Count(got, "\n") != 0 {
		t.Fatalf("tool line must be single-line, got %q", got)
	}
	if strings.Count(got, "`")%2 != 0 {
		t.Fatalf("unbalanced code span: %q", got)
	}
	if !strings.HasPrefix(got, "> `🔧 ") || !strings.HasSuffix(got, "`") {
		t.Fatalf("frame broken: %q", got)
	}
	// Interior backticks are replaced, so the span cannot be escaped.
	if strings.Contains(strings.TrimSuffix(strings.TrimPrefix(got, "> `"), "`"), "`") {
		t.Fatalf("interior backtick survived: %q", got)
	}
	if !strings.Contains(got, "--\u200bmodel") {
		t.Fatalf("reserved flag token not defused: %q", got)
	}
}

// TestToolLines_LongTitleTruncated proves the rune-aware cap applies to
// the durable line (and does not split a multi-byte rune).
func TestToolLines_LongTitleTruncated(t *testing.T) {
	long := strings.Repeat("🌲", maxProgressRunes+20)
	sink := promptWith(t, "c-long", Options{ShowTools: true}, func(a *fakeAgent, sid acp.SessionId) {
		a.emitUpdate(sid, toolCall("t1", long, acp.ToolKindOther))
	})
	want := "> `🛠️ " + strings.Repeat("🌲", maxProgressRunes) + "`"
	if got := body(sink); got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

// TestToolLines_DisabledIsSilent is the show_tools=false contract: the
// spinner still learns the label, the body stays exactly the agent's text.
func TestToolLines_DisabledIsSilent(t *testing.T) {
	sink := promptWith(t, "c-off", Options{}, func(a *fakeAgent, sid acp.SessionId) {
		a.emitUpdate(sid, toolCall("t1", "go test ./...", acp.ToolKindExecute))
		a.emit(sid, "done")
	})
	if got := body(sink); got != "done" {
		t.Fatalf("body = %q, want %q", got, "done")
	}
	sink.mu.Lock()
	labels := sink.toolLabels
	sink.mu.Unlock()
	if len(labels) != 1 || labels[0] != "go test ./.." {
		t.Fatalf("toolLabels = %v, want the capped spinner label", labels)
	}
}

// TestToolLine_FlushesHeldEscaperText proves a tool line never reorders
// message text: the flag escaper holds a trailing partial token, and it
// must be released BEFORE the tool line lands.
func TestToolLine_FlushesHeldEscaperText(t *testing.T) {
	sink := promptWith(t, "c-flush", Options{ShowTools: true}, func(a *fakeAgent, sid acp.SessionId) {
		a.emit(sid, "tail") // no trailing whitespace → held by the escaper
		a.emitUpdate(sid, toolCall("t1", "Grep", acp.ToolKindSearch))
	})
	want := "tail\n\n> `🔍 Grep`"
	if got := body(sink); got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func TestToolKindEmoji(t *testing.T) {
	cases := map[acp.ToolKind]string{
		acp.ToolKindRead:       "📖",
		acp.ToolKindEdit:       "✏️",
		acp.ToolKindDelete:     "🗑️",
		acp.ToolKindMove:       "📦",
		acp.ToolKindSearch:     "🔍",
		acp.ToolKindExecute:    "🔧",
		acp.ToolKindFetch:      "🌐",
		acp.ToolKindThink:      "💭",
		acp.ToolKindSwitchMode: "🔀",
		acp.ToolKindOther:      "🛠️",
		acp.ToolKind("nope"):   "🛠️",
	}
	for kind, want := range cases {
		if got := toolKindEmoji(kind); got != want {
			t.Errorf("toolKindEmoji(%q) = %q, want %q", kind, got, want)
		}
	}
}

// TestPlan_ForwardedAndNormalised proves plan updates reach the sink as
// normalised entries — single-line, rune-capped, flag-defused — and never
// as body text.
func TestPlan_ForwardedAndNormalised(t *testing.T) {
	sink := promptWith(t, "c-plan", Options{ShowPlans: true}, func(a *fakeAgent, sid acp.SessionId) {
		a.emitUpdate(sid, acp.UpdatePlan(
			acp.PlanEntry{Content: "wire\n\tconfig  knobs", Status: acp.PlanEntryStatusCompleted},
			acp.PlanEntry{Content: "run --model probe", Status: acp.PlanEntryStatusInProgress},
			acp.PlanEntry{Content: strings.Repeat("x", maxProgressRunes+5), Status: acp.PlanEntryStatusPending},
		))
		a.emit(sid, "ok")
	})
	if got := body(sink); got != "ok" {
		t.Fatalf("plan must not reach the body: %q", got)
	}
	sink.mu.Lock()
	plans := sink.plans
	sink.mu.Unlock()
	if len(plans) != 1 {
		t.Fatalf("SetPlan calls = %d, want 1", len(sink.plans))
	}
	want := []statusline.PlanEntry{
		{Content: "wire config knobs", Status: "completed"},
		{Content: "run --\u200bmodel probe", Status: "in_progress"},
		{Content: strings.Repeat("x", maxProgressRunes), Status: "pending"},
	}
	got := plans[0]
	if len(got) != len(want) {
		t.Fatalf("entries = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry[%d] = %#v, want %#v", i, got[i], want[i])
		}
	}
}

// TestPlan_ReplacesPrevious pins ACP semantics: each plan update is the
// COMPLETE list, so the sink sees each revision whole.
func TestPlan_ReplacesPrevious(t *testing.T) {
	sink := promptWith(t, "c-plan2", Options{ShowPlans: true}, func(a *fakeAgent, sid acp.SessionId) {
		a.emitUpdate(sid, acp.UpdatePlan(acp.PlanEntry{Content: "a", Status: acp.PlanEntryStatusPending}))
		a.emitUpdate(sid, acp.UpdatePlan(
			acp.PlanEntry{Content: "a", Status: acp.PlanEntryStatusCompleted},
			acp.PlanEntry{Content: "b", Status: acp.PlanEntryStatusInProgress},
		))
	})
	sink.mu.Lock()
	plans := sink.plans
	sink.mu.Unlock()
	if len(plans) != 2 || len(plans[0]) != 1 || len(plans[1]) != 2 {
		t.Fatalf("plans = %#v, want revisions of 1 then 2 entries", plans)
	}
}

// TestPlan_DisabledIsSilent is the show_plans=false contract.
func TestPlan_DisabledIsSilent(t *testing.T) {
	sink := promptWith(t, "c-plan-off", Options{}, func(a *fakeAgent, sid acp.SessionId) {
		a.emitUpdate(sid, acp.UpdatePlan(acp.PlanEntry{Content: "a", Status: acp.PlanEntryStatusPending}))
		a.emit(sid, "done")
	})
	sink.mu.Lock()
	plans := sink.plans
	sink.mu.Unlock()
	if len(plans) != 0 {
		t.Fatalf("SetPlan must not be called when show_plans=false: %#v", plans)
	}
	if got := body(sink); got != "done" {
		t.Fatalf("body = %q, want %q", got, "done")
	}
}

func TestParseOptions_ProgressKnobs(t *testing.T) {
	defaults := Options{ShowPlans: true, ShowTools: true}
	if got := ParseOptions(nil, defaults); got != defaults {
		t.Fatalf("empty params must keep defaults: %#v", got)
	}
	got := ParseOptions(map[string]any{"show_plans": false, "show_tools": false}, defaults)
	if got.ShowPlans || got.ShowTools {
		t.Fatalf("user opt-out ignored: %#v", got)
	}
	// Wrong-typed values are untrusted input: dropped, defaults survive.
	got = ParseOptions(map[string]any{"show_plans": "yes", "show_tools": 1}, defaults)
	if !got.ShowPlans || !got.ShowTools {
		t.Fatalf("wrong-typed params must be ignored: %#v", got)
	}
	// And the opt-in direction from an off default.
	got = ParseOptions(map[string]any{"show_plans": true, "show_tools": true}, Options{})
	if !got.ShowPlans || !got.ShowTools {
		t.Fatalf("opt-in ignored: %#v", got)
	}
}

// TestDiscardSink_SetPlan pins the reaction-turn sink's no-op contract.
// A reaction's output is discarded, so its plan updates must be too.
func TestDiscardSink_SetPlan(t *testing.T) {
	discardSink{convID: "c"}.SetPlan([]statusline.PlanEntry{{Content: "step", Status: "pending"}})
}

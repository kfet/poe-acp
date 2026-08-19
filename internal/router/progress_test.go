package router

import (
	"context"
	"fmt"
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
	if got := ParseOptions(nil, defaults, nil); got != defaults {
		t.Fatalf("empty params must keep defaults: %#v", got)
	}
	got := ParseOptions(map[string]any{"show_plans": false, "show_tools": false}, defaults, nil)
	if got.ShowPlans || got.ShowTools {
		t.Fatalf("user opt-out ignored: %#v", got)
	}
	// Wrong-typed values are untrusted input: dropped, defaults survive.
	got = ParseOptions(map[string]any{"show_plans": "yes", "show_tools": 1}, defaults, nil)
	if !got.ShowPlans || !got.ShowTools {
		t.Fatalf("wrong-typed params must be ignored: %#v", got)
	}
	// And the opt-in direction from an off default.
	got = ParseOptions(map[string]any{"show_plans": true, "show_tools": true}, Options{}, nil)
	if !got.ShowPlans || !got.ShowTools {
		t.Fatalf("opt-in ignored: %#v", got)
	}
}

// TestDiscardSink_SetPlan pins the reaction-turn sink's no-op contract.
// A reaction's output is discarded, so its plan updates must be too.
func TestDiscardSink_SetPlan(t *testing.T) {
	discardSink{convID: "c"}.SetPlan([]statusline.PlanEntry{{Content: "step", Status: "pending"}})
}

// ---- show_tool_details -----------------------------------------------

// detailOpts is the both-toggles-on configuration; detail rendering is
// deliberately gated on show_tools as well.
var detailOpts = Options{ShowTools: true, ShowToolDetails: true}

func toolCallWith(id, title string, kind acp.ToolKind, texts ...string) acp.SessionUpdate {
	c := make([]acp.ToolCallContent, 0, len(texts))
	for _, t := range texts {
		c = append(c, acp.ToolContent(acp.TextBlock(t)))
	}
	return acp.SessionUpdate{ToolCall: &acp.SessionUpdateToolCall{
		ToolCallId: acp.ToolCallId(id), Title: title, Kind: kind, Content: c,
	}}
}

func toolUpdate(id string, status acp.ToolCallStatus, texts ...string) acp.SessionUpdate {
	c := make([]acp.ToolCallContent, 0, len(texts))
	for _, t := range texts {
		c = append(c, acp.ToolContent(acp.TextBlock(t)))
	}
	return acp.SessionUpdate{ToolCallUpdate: &acp.SessionToolCallUpdate{
		ToolCallId: acp.ToolCallId(id), Status: &status, Content: c,
	}}
}

// TestToolDetails_StartBlockAndTerminalResult is the headline case: a
// rexec-shaped tool call renders its start content under the title
// line, and its completed update renders a ✓ group with the output —
// all inside one blockquote, with the following prose separated. The
// terminal marker carries NO label: it sits directly under the start
// line of the same call.
func TestToolDetails_StartBlockAndTerminalResult(t *testing.T) {
	sink := promptWith(t, "c-detail", detailOpts, func(a *fakeAgent, sid acp.SessionId) {
		a.emitUpdate(sid, toolCallWith("t1", "rexec zbox", acp.ToolKindExecute,
			"host: zbox\n```\n$ uname -sr\n```"))
		a.emitUpdate(sid, toolUpdate("t1", acp.ToolCallStatusInProgress, "ignored"))
		a.emitUpdate(sid, toolUpdate("t1", acp.ToolCallStatusCompleted, "```\nLinux 6.8.0\n```\nexit 0"))
		a.emit(sid, "done")
	})
	want := "> `🔧 rexec zbox`\n" +
		"> host: zbox\n> ```\n> $ uname -sr\n> ```\n" +
		"> `✓`\n" +
		"> ```\n> Linux 6.8.0\n> ```\n> exit 0" +
		"\n\ndone"
	if got := body(sink); got != want {
		t.Fatalf("body =\n%q\nwant\n%q", got, want)
	}
}

// TestToolDetails_FailedStatusMarker pins the ✗ marker and the error
// content. The label is dropped because the group is adjacent to its
// own start line (see TestToolDetails_LabelKeptWhenInterleaved for the
// remembered-title path).
func TestToolDetails_FailedStatusMarker(t *testing.T) {
	sink := promptWith(t, "c-detail-fail", detailOpts, func(a *fakeAgent, sid acp.SessionId) {
		a.emitUpdate(sid, toolCall("t1", "Bash", acp.ToolKindExecute))
		a.emitUpdate(sid, toolUpdate("t1", acp.ToolCallStatusFailed, "boom"))
	})
	want := "> `🔧 Bash`\n> `✗`\n> boom"
	if got := body(sink); got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

// TestToolDetails_TerminalEmittedOnce guards against a duplicate group
// when the agent repeats a terminal status for the same call.
func TestToolDetails_TerminalEmittedOnce(t *testing.T) {
	sink := promptWith(t, "c-detail-dup", detailOpts, func(a *fakeAgent, sid acp.SessionId) {
		a.emitUpdate(sid, toolCall("t1", "Bash", acp.ToolKindExecute))
		a.emitUpdate(sid, toolUpdate("t1", acp.ToolCallStatusCompleted, "one"))
		a.emitUpdate(sid, toolUpdate("t1", acp.ToolCallStatusCompleted, "two"))
	})
	if got, want := body(sink), "> `🔧 Bash`\n> `✓`\n> one"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

// TestToolDetails_DisabledReproducesOldOutput is the byte-for-byte
// opt-out contract: with show_tool_details off, content and terminal
// updates contribute nothing, exactly as before the option existed.
func TestToolDetails_DisabledReproducesOldOutput(t *testing.T) {
	emit := func(a *fakeAgent, sid acp.SessionId) {
		a.emitUpdate(sid, toolCallWith("t1", "rexec zbox", acp.ToolKindExecute, "host: zbox"))
		a.emitUpdate(sid, toolUpdate("t1", acp.ToolCallStatusCompleted, "out"))
		a.emit(sid, "done")
	}
	want := "> `🔧 rexec zbox`\n\ndone"
	if got := body(promptWith(t, "c-detail-off", Options{ShowTools: true}, emit)); got != want {
		t.Fatalf("show_tool_details=false body = %q, want %q", got, want)
	}
	// And the gate on show_tools: details alone render nothing.
	if got := body(promptWith(t, "c-detail-nogate", Options{ShowToolDetails: true}, emit)); got != "done" {
		t.Fatalf("show_tools=false body = %q, want %q", got, "done")
	}
}

// TestToolDetails_LineElision proves the middle of an over-long block is
// replaced by a marker, head and tail survive, and the rendered block
// stays within the line budget.
func TestToolDetails_LineElision(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&b, "line%d\n", i)
	}
	sink := promptWith(t, "c-detail-elide", detailOpts, func(a *fakeAgent, sid acp.SessionId) {
		a.emitUpdate(sid, toolCallWith("t1", "Bash", acp.ToolKindExecute, b.String()))
	})
	got := body(sink)
	lines := strings.Split(got, "\n")
	// 1 title line + at most maxDetailLines rendered content lines.
	if len(lines) > maxDetailLines+1 {
		t.Fatalf("rendered %d lines, want <= %d:\n%s", len(lines), maxDetailLines+1, got)
	}
	if !strings.Contains(got, "> … 89 lines elided …") {
		t.Fatalf("elision marker missing/miscounted:\n%s", got)
	}
	if !strings.Contains(got, "> line0\n") || !strings.Contains(got, "> line99") {
		t.Fatalf("head and tail must survive:\n%s", got)
	}
	if strings.Contains(got, "line50") {
		t.Fatalf("middle must be elided:\n%s", got)
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, ">") {
			t.Fatalf("line escaped the blockquote: %q", l)
		}
	}
}

// TestToolDetails_CharBudget covers the size bound independently of the
// line bound: few lines, each enormous.
func TestToolDetails_CharBudget(t *testing.T) {
	huge := strings.Repeat("x", 5000) + "\n" + strings.Repeat("y", 5000) + "\n" + strings.Repeat("z", 5000)
	sink := promptWith(t, "c-detail-chars", detailOpts, func(a *fakeAgent, sid acp.SessionId) {
		a.emitUpdate(sid, toolCallWith("t1", "Bash", acp.ToolKindExecute, huge))
	})
	got := body(sink)
	if n := len([]rune(got)); n > maxDetailChars+200 {
		t.Fatalf("rendered %d runes, want ~<= %d", n, maxDetailChars)
	}
	if !strings.Contains(got, "…") {
		t.Fatalf("truncation must be marked: %q", got)
	}
	if strings.Contains(got, strings.Repeat("y", 100)) {
		t.Fatalf("middle line must be elided: %q", got)
	}
}

// TestToolDetails_HostileContentCannotBreakTheFrame is the framing
// contract for detail text: every line stays quoted, an unbalanced
// fence is closed, stray backticks are neutralised, and reserved --flag
// tokens are defused.
func TestToolDetails_HostileContentCannotBreakTheFrame(t *testing.T) {
	hostile := "```sh\nrm -rf /\n" + // fence opened, never closed
		"> **pwned** `dangling\n" +
		"--show_tools off\n"
	sink := promptWith(t, "c-detail-hostile", detailOpts, func(a *fakeAgent, sid acp.SessionId) {
		a.emitUpdate(sid, toolCallWith("t1", "Bash", acp.ToolKindExecute, hostile))
		a.emit(sid, "after")
	})
	got := body(sink)
	quoted, after, found := strings.Cut(got, "\n\nafter")
	if !found || after != "" {
		t.Fatalf("prose must follow the quote block: %q", got)
	}
	for _, l := range strings.Split(quoted, "\n") {
		if !strings.HasPrefix(l, ">") {
			t.Fatalf("line escaped the blockquote: %q in %q", l, got)
		}
	}
	if n := strings.Count(quoted, "```"); n%2 != 0 {
		t.Fatalf("unbalanced code fence (%d) would swallow the answer:\n%s", n, quoted)
	}
	if !strings.Contains(quoted, "'dangling") {
		t.Fatalf("stray backtick not neutralised:\n%s", quoted)
	}
	if !strings.Contains(quoted, "--\u200bshow_tools") {
		t.Fatalf("reserved flag token not defused:\n%s", quoted)
	}
}

// TestToolDetails_NonTextBlocksSkipped: a diff block has no bounded
// one-block rendering, so it contributes nothing (the title line still
// stands alone).
func TestToolDetails_NonTextBlocksSkipped(t *testing.T) {
	sink := promptWith(t, "c-detail-diff", detailOpts, func(a *fakeAgent, sid acp.SessionId) {
		a.emitUpdate(sid, acp.SessionUpdate{ToolCall: &acp.SessionUpdateToolCall{
			ToolCallId: "t1", Title: "Edit", Kind: acp.ToolKindEdit,
			Content: []acp.ToolCallContent{acp.ToolDiffContent("a.go", "new", "old")},
		}})
	})
	if got, want := body(sink), "> `✏️ Edit`"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

// TestToolDetails_UnknownCallIdStillRenders: a terminal update for a
// call the relay never saw (e.g. emitted before the tool_call) still
// produces a group, falling back to the update's own title/kind.
func TestToolDetails_UnknownCallIdStillRenders(t *testing.T) {
	title, kind := "Grep", acp.ToolKindSearch
	sink := promptWith(t, "c-detail-orphan", detailOpts, func(a *fakeAgent, sid acp.SessionId) {
		st := acp.ToolCallStatusCompleted
		a.emitUpdate(sid, acp.SessionUpdate{ToolCallUpdate: &acp.SessionToolCallUpdate{
			ToolCallId: "zzz", Status: &st, Title: &title, Kind: &kind,
			Content: []acp.ToolCallContent{acp.ToolContent(acp.TextBlock("3 matches"))},
		}})
	})
	if got, want := body(sink), "> `✓ Grep`\n> 3 matches"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func TestParseOptions_ShowToolDetails(t *testing.T) {
	defaults := Options{ShowTools: true, ShowToolDetails: true}
	if got := ParseOptions(nil, defaults, nil); got != defaults {
		t.Fatalf("empty params must keep defaults: %#v", got)
	}
	if got := ParseOptions(map[string]any{"show_tool_details": false}, defaults, nil); got.ShowToolDetails {
		t.Fatalf("user opt-out ignored: %#v", got)
	}
	if got := ParseOptions(map[string]any{"show_tool_details": "yes"}, defaults, nil); !got.ShowToolDetails {
		t.Fatalf("wrong-typed param must be ignored: %#v", got)
	}
	if got := ParseOptions(map[string]any{"show_tool_details": true}, Options{}, nil); !got.ShowToolDetails {
		t.Fatalf("opt-in ignored: %#v", got)
	}
}

// TestToolDetails_StatuslessAndEmptyContent covers the quiet paths: an
// update with no status at all stays spinner-only, and content blocks
// that carry no usable text add no lines.
func TestToolDetails_StatuslessAndEmptyContent(t *testing.T) {
	sink := promptWith(t, "c-detail-quiet", detailOpts, func(a *fakeAgent, sid acp.SessionId) {
		a.emitUpdate(sid, toolCallWith("t1", "Bash", acp.ToolKindExecute, "", "  \n\n  "))
		a.emitUpdate(sid, acp.SessionUpdate{ToolCallUpdate: &acp.SessionToolCallUpdate{
			ToolCallId: "t1", Content: []acp.ToolCallContent{acp.ToolContent(acp.TextBlock("no status"))},
		}})
	})
	if got, want := body(sink), "> `🔧 Bash`"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

// TestToolDetails_BlankInteriorLinesStayQuoted: surrounding blank lines
// are trimmed, interior ones survive as bare ">" so the quote block is
// never broken by an empty line.
func TestToolDetails_BlankInteriorLinesStayQuoted(t *testing.T) {
	sink := promptWith(t, "c-detail-blank", detailOpts, func(a *fakeAgent, sid acp.SessionId) {
		a.emitUpdate(sid, toolCallWith("t1", "Bash", acp.ToolKindExecute, "\n\nhead\n\ntail\n\n"))
	})
	if got, want := body(sink), "> `🔧 Bash`\n> head\n>\n> tail"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

// TestToolDetails_CharBudgetShrinksElision pins the second bounding
// pass: few enough lines to pass the line cap, too many characters, so
// the kept head/tail shrinks until the size budget holds.
func TestToolDetails_CharBudgetShrinksElision(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 6; i++ {
		fmt.Fprintf(&b, "%d%s\n", i, strings.Repeat("x", 199))
	}
	sink := promptWith(t, "c-detail-shrink", detailOpts, func(a *fakeAgent, sid acp.SessionId) {
		a.emitUpdate(sid, toolCallWith("t1", "Bash", acp.ToolKindExecute, b.String()))
	})
	got := body(sink)
	// 4 of 6 lines survive (2 head + 2 tail = 800 runes), 2 elided.
	if !strings.Contains(got, "> … 2 lines elided …") {
		t.Fatalf("expected a 2-line elision:\n%s", got)
	}
	for _, want := range []string{"> 0x", "> 1x", "> 4x", "> 5x"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing kept line %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "> 2x") || strings.Contains(got, "> 3x") {
		t.Fatalf("middle lines must be elided:\n%s", got)
	}
}

// ---- redundancy removal ----------------------------------------------
//
// Three unconditional reductions: the terminal label is dropped when the
// group is adjacent to its own start line, content already rendered at
// start time is not repeated, and a completed call with nothing left to
// say emits nothing at all.

// TestToolDetails_LabelKeptWhenInterleaved covers parallel tool calls —
// the routine case. Two calls start back to back, then both complete:
// neither terminal group is adjacent to its own start line, so both
// carry the remembered label, and the second's label is required to
// tell the two groups apart.
func TestToolDetails_LabelKeptWhenInterleaved(t *testing.T) {
	sink := promptWith(t, "c-parallel", detailOpts, func(a *fakeAgent, sid acp.SessionId) {
		a.emitUpdate(sid, toolCall("t1", "Read a.go", acp.ToolKindRead))
		a.emitUpdate(sid, toolCall("t2", "Read b.go", acp.ToolKindRead))
		a.emitUpdate(sid, toolUpdate("t1", acp.ToolCallStatusCompleted, "aaa"))
		a.emitUpdate(sid, toolUpdate("t2", acp.ToolCallStatusCompleted, "bbb"))
	})
	want := "> `📖 Read a.go`\n> `📖 Read b.go`\n" +
		"> `✓ Read a.go`\n> aaa\n" +
		"> `✓ Read b.go`\n> bbb"
	if got := body(sink); got != want {
		t.Fatalf("body =\n%q\nwant\n%q", got, want)
	}
}

// TestToolDetails_LabelKeptAfterMessageText: prose (or a thought)
// between the start line and the terminal update also breaks adjacency
// — the marker is no longer under the line it completes, so it must
// name its call. It also opens a fresh blockquote block.
func TestToolDetails_LabelKeptAfterMessageText(t *testing.T) {
	sink := promptWith(t, "c-adj-text", detailOpts, func(a *fakeAgent, sid acp.SessionId) {
		a.emitUpdate(sid, toolCall("t1", "Bash", acp.ToolKindExecute))
		a.emit(sid, "meanwhile ")
		a.emitUpdate(sid, toolUpdate("t1", acp.ToolCallStatusCompleted, "out"))
	})
	want := "> `🔧 Bash`\n\nmeanwhile \n\n> `✓ Bash`\n> out"
	if got := body(sink); got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

// TestToolDetails_LabelDroppedAfterOwnSilentSibling: a suppressed
// (silent-success) group writes nothing, so it does NOT break the
// adjacency of a later call whose start line is still the last write.
func TestToolDetails_LabelDroppedAfterOwnSilentSibling(t *testing.T) {
	sink := promptWith(t, "c-adj-silent", detailOpts, func(a *fakeAgent, sid acp.SessionId) {
		a.emitUpdate(sid, toolCall("t1", "Read a.go", acp.ToolKindRead))
		a.emitUpdate(sid, toolCall("t2", "Read b.go", acp.ToolKindRead))
		a.emitUpdate(sid, toolUpdate("t1", acp.ToolCallStatusCompleted)) // silent
		a.emitUpdate(sid, toolUpdate("t2", acp.ToolCallStatusCompleted, "bbb"))
	})
	want := "> `📖 Read a.go`\n> `📖 Read b.go`\n> `✓`\n> bbb"
	if got := body(sink); got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

// TestToolDetails_ContentDedupe: the block the agent already showed at
// start time is not repeated in the terminal group; genuinely new
// blocks still render.
func TestToolDetails_ContentDedupe(t *testing.T) {
	sink := promptWith(t, "c-dedupe", detailOpts, func(a *fakeAgent, sid acp.SessionId) {
		a.emitUpdate(sid, toolCallWith("t1", "rexec rn", acp.ToolKindExecute, "$ uname -sr"))
		a.emitUpdate(sid, toolUpdate("t1", acp.ToolCallStatusCompleted, "$ uname -sr", "Linux 6.8.0"))
	})
	want := "> `🔧 rexec rn`\n> $ uname -sr\n> `✓`\n> Linux 6.8.0"
	if got := body(sink); got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

// TestToolDetails_DedupeIsPerCall: two calls whose content renders
// identically must not silence each other. Dedupe is keyed by
// ToolCallId, never global.
func TestToolDetails_DedupeIsPerCall(t *testing.T) {
	sink := promptWith(t, "c-dedupe-percall", detailOpts, func(a *fakeAgent, sid acp.SessionId) {
		a.emitUpdate(sid, toolCallWith("t1", "Bash a", acp.ToolKindExecute, "$ date"))
		a.emitUpdate(sid, toolCall("t2", "Bash b", acp.ToolKindExecute))
		a.emitUpdate(sid, toolUpdate("t2", acp.ToolCallStatusCompleted, "$ date"))
	})
	want := "> `🔧 Bash a`\n> $ date\n> `🔧 Bash b`\n> `✓`\n> $ date"
	if got := body(sink); got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

// TestToolDetails_SilentSuccess: a completed update with nothing new to
// say emits NOTHING — no marker line, no blank line, and the blockquote
// framing survives (the next tool line joins the same quote block, and
// following prose is still separated by one blank line).
func TestToolDetails_SilentSuccess(t *testing.T) {
	sink := promptWith(t, "c-silent", detailOpts, func(a *fakeAgent, sid acp.SessionId) {
		a.emitUpdate(sid, toolCallWith("t1", "Read a.go", acp.ToolKindRead, "package main"))
		a.emitUpdate(sid, toolUpdate("t1", acp.ToolCallStatusCompleted, "package main"))
		a.emitUpdate(sid, toolCall("t2", "Grep", acp.ToolKindSearch))
		a.emitUpdate(sid, toolUpdate("t2", acp.ToolCallStatusCompleted))
		a.emit(sid, "done")
	})
	want := "> `📖 Read a.go`\n> package main\n> `🔍 Grep`\n\ndone"
	if got := body(sink); got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

// TestToolDetails_SilentSuccessThenTextIsNotSeparatedTwice guards the
// framing edge: suppression must leave chunkMode alone, so the prose
// that follows gets exactly one blank-line separator.
func TestToolDetails_SilentSuccessOpensNoBlock(t *testing.T) {
	sink := promptWith(t, "c-silent-first", detailOpts, func(a *fakeAgent, sid acp.SessionId) {
		// Terminal update with no preceding tool line at all: nothing to
		// render, so the turn must not open a blockquote out of nowhere.
		a.emitUpdate(sid, toolUpdate("zzz", acp.ToolCallStatusCompleted))
		a.emit(sid, "just prose")
	})
	if got, want := body(sink), "just prose"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	sink.mu.Lock()
	first := sink.firstCalled
	sink.mu.Unlock()
	if !first {
		t.Fatal("FirstChunk must still fire for the prose")
	}
}

// TestToolDetails_FailedAlwaysRenders is the deliberate asymmetry: a
// failure renders even when dedupe leaves nothing to show. The marker
// alone is the signal, and it is never suppressed.
func TestToolDetails_FailedAlwaysRenders(t *testing.T) {
	sink := promptWith(t, "c-fail-loud", detailOpts, func(a *fakeAgent, sid acp.SessionId) {
		a.emitUpdate(sid, toolCallWith("t1", "Bash", acp.ToolKindExecute, "$ false"))
		a.emitUpdate(sid, toolUpdate("t1", acp.ToolCallStatusFailed, "$ false"))
		a.emitUpdate(sid, toolCall("t2", "Bash", acp.ToolKindExecute))
		a.emitUpdate(sid, toolUpdate("t2", acp.ToolCallStatusFailed))
	})
	want := "> `🔧 Bash`\n> $ false\n> `✗`\n> `🔧 Bash`\n> `✗`"
	if got := body(sink); got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

// TestToolDetails_DisabledUnaffectedByDedupe re-pins the opt-out path
// byte for byte against a turn that the reductions WOULD have changed:
// with show_tool_details off the body is the tool lines and the prose,
// exactly as before this change.
func TestToolDetails_DisabledUnaffectedByDedupe(t *testing.T) {
	emit := func(a *fakeAgent, sid acp.SessionId) {
		a.emitUpdate(sid, toolCallWith("t1", "rexec rn", acp.ToolKindExecute, "$ uname -sr"))
		a.emitUpdate(sid, toolCallWith("t2", "Read a.go", acp.ToolKindRead, "package main"))
		a.emitUpdate(sid, toolUpdate("t1", acp.ToolCallStatusCompleted, "$ uname -sr", "Linux"))
		a.emitUpdate(sid, toolUpdate("t2", acp.ToolCallStatusFailed, "boom"))
		a.emit(sid, "done")
	}
	want := "> `🔧 rexec rn`\n> `📖 Read a.go`\n\ndone"
	if got := body(promptWith(t, "c-dedupe-off", Options{ShowTools: true}, emit)); got != want {
		t.Fatalf("show_tool_details=false body = %q, want %q", got, want)
	}
}

// TestToolDetails_OrphanTerminalRendersOnce: a terminal update for a
// call whose tool_call never arrived is still gated once-only. Without
// the gate the echo would render a SECOND group directly under the
// first — and being "adjacent" to itself, as a bare unlabelled marker.
func TestToolDetails_OrphanTerminalRendersOnce(t *testing.T) {
	sink := promptWith(t, "c-orphan-echo", detailOpts, func(a *fakeAgent, sid acp.SessionId) {
		a.emitUpdate(sid, toolUpdate("zzz", acp.ToolCallStatusCompleted, "3 matches"))
		a.emitUpdate(sid, toolUpdate("zzz", acp.ToolCallStatusCompleted, "3 matches"))
	})
	if got, want := body(sink), "> `✓ tool`\n> 3 matches"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

// TestToolDetails_FailedAfterSilentSuccess: suppression records nothing
// — not even "terminal seen" — so a failure that follows a silent
// success for the same call is still loud. Suppressing output must
// never be able to swallow a failure.
func TestToolDetails_FailedAfterSilentSuccess(t *testing.T) {
	sink := promptWith(t, "c-silent-then-fail", detailOpts, func(a *fakeAgent, sid acp.SessionId) {
		a.emitUpdate(sid, toolCall("t1", "Bash", acp.ToolKindExecute))
		a.emitUpdate(sid, toolUpdate("t1", acp.ToolCallStatusCompleted))
		a.emitUpdate(sid, toolUpdate("t1", acp.ToolCallStatusFailed, "boom"))
	})
	if got, want := body(sink), "> `🔧 Bash`\n> `✗`\n> boom"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

// TestToolDetails_ResultOnLaterUpdateStillLands: same reason — a
// completed update carrying no content does not consume the call's one
// terminal group, so the result arriving on a second update renders.
func TestToolDetails_ResultOnLaterUpdateStillLands(t *testing.T) {
	sink := promptWith(t, "c-late-result", detailOpts, func(a *fakeAgent, sid acp.SessionId) {
		a.emitUpdate(sid, toolCall("t1", "Bash", acp.ToolKindExecute))
		a.emitUpdate(sid, toolUpdate("t1", acp.ToolCallStatusCompleted))
		a.emitUpdate(sid, toolUpdate("t1", acp.ToolCallStatusCompleted, "out"))
		a.emitUpdate(sid, toolUpdate("t1", acp.ToolCallStatusCompleted, "out"))
	})
	if got, want := body(sink), "> `🔧 Bash`\n> `✓`\n> out"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

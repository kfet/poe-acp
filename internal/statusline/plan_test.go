package statusline

import (
	"strings"
	"testing"
)

func TestPlanChecklist_StatusGlyphs(t *testing.T) {
	got := PlanChecklist([]PlanEntry{
		{Content: "wire config knobs", Status: "completed"},
		{Content: "render tool lines", Status: "in_progress"},
		{Content: "render plan checklist", Status: "pending"},
		{Content: "unknown status", Status: "weird"},
	})
	want := "> ✅ wire config knobs\n" +
		"> ⏳ render tool lines\n" +
		"> ▫️ render plan checklist\n" +
		"> ▫️ unknown status"
	if got != want {
		t.Fatalf("PlanChecklist =\n%q\nwant\n%q", got, want)
	}
}

func TestPlanChecklist_EmptyInputs(t *testing.T) {
	if got := PlanChecklist(nil); got != "" {
		t.Fatalf("nil plan = %q, want empty", got)
	}
	if got := PlanChecklist([]PlanEntry{{Content: "", Status: "pending"}}); got != "" {
		t.Fatalf("content-less plan = %q, want empty", got)
	}
}

// TestPlanChecklist_CapsEntries proves a long plan cannot blow up every
// keepalive frame: at most MaxPlanEntries lines plus a "+N more" tail.
func TestPlanChecklist_CapsEntries(t *testing.T) {
	var entries []PlanEntry
	for i := 0; i < MaxPlanEntries+5; i++ {
		entries = append(entries, PlanEntry{Content: "step", Status: "pending"})
	}
	// An empty entry in the overflow region must be skipped, not counted.
	entries = append(entries, PlanEntry{Content: "", Status: "pending"})
	got := PlanChecklist(entries)
	lines := strings.Split(got, "\n")
	if len(lines) != MaxPlanEntries+1 {
		t.Fatalf("lines = %d, want %d:\n%s", len(lines), MaxPlanEntries+1, got)
	}
	if lines[len(lines)-1] != "> _… +5 more_" {
		t.Fatalf("tail = %q, want the overflow summary", lines[len(lines)-1])
	}
}

// TestPlanChecklist_ExactlyAtCap has no overflow tail.
func TestPlanChecklist_ExactlyAtCap(t *testing.T) {
	var entries []PlanEntry
	for i := 0; i < MaxPlanEntries; i++ {
		entries = append(entries, PlanEntry{Content: "step", Status: "completed"})
	}
	got := PlanChecklist(entries)
	if n := len(strings.Split(got, "\n")); n != MaxPlanEntries {
		t.Fatalf("lines = %d, want %d", n, MaxPlanEntries)
	}
	if strings.Contains(got, "more") {
		t.Fatalf("unexpected overflow tail: %q", got)
	}
}

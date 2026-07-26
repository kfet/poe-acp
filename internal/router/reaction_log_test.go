package router

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// fixedNow returns a clock stuck at a non-UTC instant, so tests also
// pin down that the ledger timestamp is normalised to UTC.
func fixedNow() func() time.Time {
	ts := time.Date(2026, 3, 4, 5, 6, 7, 0, time.FixedZone("CET", 3600))
	return func() time.Time { return ts }
}

// readLedger returns the lines of dir/reactions.jsonl.
func readLedger(t *testing.T, dir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, reactionsLogName))
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	text := string(b)
	if !strings.HasSuffix(text, "\n") {
		t.Fatalf("ledger not newline-terminated: %q", text)
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

// TestReportReaction_PersistsLedger: each reaction event appends one
// JSON line to StateDir/reactions.jsonl, in event order, with the
// timestamp taken from the injected clock and normalised to UTC.
func TestReportReaction_PersistsLedger(t *testing.T) {
	agent := newFakeAgent(func(_ context.Context, _ *fakeAgent, _ acp.SessionId, _ string) (acp.StopReason, error) {
		return acp.StopReasonEndTurn, nil
	})
	dir := t.TempDir()
	r, _ := New(Config{Agent: agent, StateDir: dir, SessionTTL: time.Hour, Now: fixedNow()})

	if err := r.ReportReaction(context.Background(), "conv-1", "user-1", "msg-1", "👍", "added"); err != nil {
		t.Fatalf("ReportReaction added: %v", err)
	}
	if err := r.ReportReaction(context.Background(), "conv-1", "user-1", "msg-1", "👎", "removed"); err != nil {
		t.Fatalf("ReportReaction removed: %v", err)
	}

	got := readLedger(t, dir)
	want := []string{
		`{"ts":"2026-03-04T04:06:07Z","conv_id":"conv-1","user_id":"user-1","message_id":"msg-1","reaction":"👍","action":"added"}`,
		`{"ts":"2026-03-04T04:06:07Z","conv_id":"conv-1","user_id":"user-1","message_id":"msg-1","reaction":"👎","action":"removed"}`,
	}
	if len(got) != len(want) {
		t.Fatalf("ledger lines=%d want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d:\n got %s\nwant %s", i, got[i], want[i])
		}
	}
}

// TestReportReaction_PersistsDefaultedFields: an empty conv_id becomes
// "default" and an empty action becomes "added" in the ledger, matching
// what the agent is told.
func TestReportReaction_PersistsDefaultedFields(t *testing.T) {
	agent := newFakeAgent(func(_ context.Context, _ *fakeAgent, _ acp.SessionId, _ string) (acp.StopReason, error) {
		return acp.StopReasonEndTurn, nil
	})
	dir := t.TempDir()
	r, _ := New(Config{Agent: agent, StateDir: dir, SessionTTL: time.Hour, Now: fixedNow()})

	if err := r.ReportReaction(context.Background(), "", "u", "m", "👍", ""); err != nil {
		t.Fatalf("ReportReaction: %v", err)
	}
	want := `{"ts":"2026-03-04T04:06:07Z","conv_id":"default","user_id":"u","message_id":"m","reaction":"👍","action":"added"}`
	if got := readLedger(t, dir); len(got) != 1 || got[0] != want {
		t.Fatalf("ledger=%v want [%s]", got, want)
	}
}

// TestReportReaction_PersistsWhenSessionCreateFails: the label is
// written even though getOrCreate fails and the caller gets an error —
// the reaction is the only human quality signal we ever get.
func TestReportReaction_PersistsWhenSessionCreateFails(t *testing.T) {
	agent := newFakeAgent(func(context.Context, *fakeAgent, acp.SessionId, string) (acp.StopReason, error) {
		return acp.StopReasonEndTurn, nil
	})
	agent.newSessErr = errFakePromptFail
	dir := t.TempDir()
	r, _ := New(Config{Agent: agent, StateDir: dir, SessionTTL: time.Hour, Now: fixedNow()})

	if err := r.ReportReaction(context.Background(), "c", "u", "m", "👍", "added"); err == nil {
		t.Fatal("want getOrCreate error")
	}
	if got := readLedger(t, dir); len(got) != 1 {
		t.Fatalf("ledger=%v want 1 line", got)
	}
}

// TestReportReaction_PersistsWhenQueueSheds: the label is written even
// when the session queue refuses the turn (stopped/full).
func TestReportReaction_PersistsWhenQueueSheds(t *testing.T) {
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	dir := t.TempDir()
	r, _ := New(Config{Agent: agent, StateDir: dir, SessionTTL: time.Hour, Now: fixedNow()})

	// Bootstrap the session with a completed prompt, then stop its queue
	// so the reaction push is refused deterministically.
	if err := r.Prompt(context.Background(), "c", "u",
		[]Turn{{Role: "user", Content: "hi"}}, Options{}, &captureSink{}); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	r.sessions["c"].queue.stop()

	if err := r.ReportReaction(context.Background(), "c", "u", "m", "👍", "added"); err != nil {
		t.Fatalf("ReportReaction: %v", err)
	}
	if got := readLedger(t, dir); len(got) != 1 {
		t.Fatalf("ledger=%v want 1 line", got)
	}
}

// TestReportReaction_LedgerUnwritable: an unopenable ledger path is
// logged and swallowed — the reaction turn still goes through.
func TestReportReaction_LedgerUnwritable(t *testing.T) {
	got := make(chan string, 1)
	agent := newFakeAgent(func(_ context.Context, _ *fakeAgent, _ acp.SessionId, text string) (acp.StopReason, error) {
		got <- text
		return acp.StopReasonEndTurn, nil
	})
	dir := t.TempDir()
	// A directory where the ledger file belongs: OpenFile always fails.
	if err := os.Mkdir(filepath.Join(dir, reactionsLogName), 0o755); err != nil {
		t.Fatal(err)
	}
	r, _ := New(Config{Agent: agent, StateDir: dir, SessionTTL: time.Hour, Now: fixedNow()})

	if err := r.ReportReaction(context.Background(), "c", "u", "m", "👍", "added"); err != nil {
		t.Fatalf("ReportReaction: %v", err)
	}
	select {
	case text := <-got:
		if !strings.Contains(text, "out-of-band reaction") {
			t.Fatalf("unexpected prompt: %q", text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reaction turn never reached the agent")
	}
}

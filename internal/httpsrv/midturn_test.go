package httpsrv

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/poe-acp/internal/poeproto"
	"github.com/kfet/poe-acp/internal/router"
)

// TestSink_MidTurnSpinnerToggleSSE is the key SSE-level proof that the
// keepalive spinner re-arms MULTIPLE times mid-turn and that the
// text↔replace toggles render text → spinner → text correctly, never
// discarding the accumulated answer. It drives emitSpinnerFrame manually
// (heartbeat disabled, stall=0 so every idle tick re-arms) so the whole
// sequence is deterministic with no wall-clock waits.
func TestSink_MidTurnSpinnerToggleSSE(t *testing.T) {
	rec := httptest.NewRecorder()
	w, _ := poeproto.NewSSEWriter(rec)
	_ = w.Meta()
	// hb=0: no goroutine, we tick by hand. stall=0: any content-idle
	// interval counts as a stall, so a manual tick after a write re-arms.
	s := newSink(w, 0, 0)

	var spinTick int
	// Cold-start spinner (no output yet → stalled).
	if keep := s.emitSpinnerFrame(&spinTick); !keep {
		t.Fatal("cold-start tick must keep going")
	}
	// First real text: strips the cold-start spinner, appends.
	if err := s.Text("Hello"); err != nil {
		t.Fatal(err)
	}
	// Mid-turn stall → spinner re-arms, carrying "Hello".
	s.emitSpinnerFrame(&spinTick)
	// Output resumes: spinner stripped, " world" appended.
	if err := s.Text(" world"); err != nil {
		t.Fatal(err)
	}
	// Second mid-turn stall → spinner re-arms, carrying "Hello world".
	s.emitSpinnerFrame(&spinTick)
	// Final resume + seal.
	if err := s.Text("!"); err != nil {
		t.Fatal(err)
	}
	if err := s.Done(); err != nil {
		t.Fatal(err)
	}

	events := parseSSE(t, rec.Body.String())
	// Filter to content-bearing events (skip meta).
	var seq []sseEventRec
	for _, e := range events {
		if e.event == "replace_response" || e.event == "text" || e.event == "done" {
			seq = append(seq, e)
		}
	}
	// Expected ordered sequence:
	//  0 replace "> _Thinking._"                     (cold start)
	//  1 replace ""                                  (strip before "Hello")
	//  2 text    "Hello"
	//  3 replace "Hello\n\n> _Thinking.._"           (re-arm #1, carries acc)
	//  4 replace "Hello"                             (strip before " world")
	//  5 text    " world"
	//  6 replace "Hello world\n\n> _Thinking..._"    (re-arm #2, carries acc)
	//  7 replace "Hello world"                       (strip before "!")
	//  8 text    "!"
	//  9 done
	want := []sseEventRec{
		{"replace_response", "> _Thinking._"},
		{"replace_response", ""},
		{"text", "Hello"},
		{"replace_response", "Hello\n\n> _Thinking.._"},
		{"replace_response", "Hello"},
		{"text", " world"},
		{"replace_response", "Hello world\n\n> _Thinking..._"},
		{"replace_response", "Hello world"},
		{"text", "!"},
		{"done", ""},
	}
	if len(seq) != len(want) {
		t.Fatalf("event count = %d, want %d:\n%#v", len(seq), len(want), seq)
	}
	for i, wnt := range want {
		if seq[i].event != wnt.event || seq[i].text != wnt.text {
			t.Fatalf("event[%d] = {%s %q}, want {%s %q}\nfull:\n%#v",
				i, seq[i].event, seq[i].text, wnt.event, wnt.text, seq)
		}
	}
	// Every re-arm frame must contain the running accumulator — the
	// keepalive never drops the answer.
	for i, e := range seq {
		if e.event == "replace_response" && strings.Contains(e.text, "Thinking") {
			if i >= 3 && !strings.Contains(e.text, "Hello") {
				t.Fatalf("mid-turn re-arm frame dropped accumulated content: %q", e.text)
			}
		}
	}
}

// TestSink_SpinnerFrameDoesNotResetWedgeClock proves the invariant that
// a keepalive spinner frame updates NEITHER the wedge clock (lastWrite)
// NOR the content-stall clock (lastContent) — only real writes do — so a
// genuinely hung agent still trips IdleWriteTimeout while its spinner
// ticks.
func TestSink_SpinnerFrameDoesNotResetWedgeClock(t *testing.T) {
	rec := httptest.NewRecorder()
	w, _ := poeproto.NewSSEWriter(rec)
	_ = w.Meta()
	s := newSink(w, 0, 0)

	if err := s.Text("hi"); err != nil {
		t.Fatal(err)
	}
	lw := s.lastWrite.Load()
	lc := s.lastContent.Load()
	var spinTick int
	s.emitSpinnerFrame(&spinTick)
	s.emitSpinnerFrame(&spinTick)
	if s.lastWrite.Load() != lw {
		t.Fatalf("spinner frame reset the wedge clock: %d -> %d", lw, s.lastWrite.Load())
	}
	if s.lastContent.Load() != lc {
		t.Fatalf("spinner frame reset the content-stall clock: %d -> %d", lc, s.lastContent.Load())
	}
}

// TestSink_ToolActivityResetsWedgeClockOnly proves Solution B's
// invariants at the sink layer: a tool_call update resets the wedge
// clock (so a long tool isn't cut) but NOT the content-stall clock (so
// the keepalive still re-arms), records the spinner label, and never
// marks realWritten (a tool_call is not user-visible content).
func TestSink_ToolActivityResetsWedgeClockOnly(t *testing.T) {
	rec := httptest.NewRecorder()
	w, _ := poeproto.NewSSEWriter(rec)
	_ = w.Meta()
	s := newSink(w, 0, time.Hour)

	// Backdate both clocks so a reset is observable.
	old := time.Now().Add(-time.Hour).UnixNano()
	s.lastWrite.Store(old)
	s.lastContent.Store(old)

	s.ToolActivity("running bash")

	if s.realWritten() {
		t.Fatal("ToolActivity must not mark realWritten")
	}
	if got := s.snapshotActivity(); got != "running bash" {
		t.Fatalf("activity label = %q, want %q", got, "running bash")
	}
	if s.lastWrite.Load() == old {
		t.Fatal("ToolActivity must reset the wedge clock")
	}
	if s.lastContent.Load() != old {
		t.Fatal("ToolActivity must NOT reset the content-stall clock")
	}
	// A real content write clears the transient tool label.
	if err := s.Text("done"); err != nil {
		t.Fatal(err)
	}
	if got := s.snapshotActivity(); got != "" {
		t.Fatalf("real content must clear activity label, got %q", got)
	}
}

// TestSink_EmitSpinnerFrameNotStalled covers the fast-path tick: when
// output is flowing (not stalled) the heartbeat emits NO frame and keeps
// ticking only while the stream is open, exercising emitSpinnerFrame's
// non-stalled branch and orderedWriter.isClosed both ways.
func TestSink_EmitSpinnerFrameNotStalled(t *testing.T) {
	rec := httptest.NewRecorder()
	w, _ := poeproto.NewSSEWriter(rec)
	_ = w.Meta()
	// Large stall so a just-written stream is never considered stalled.
	s := newSink(w, 0, time.Hour)
	if err := s.Text("hi"); err != nil {
		t.Fatal(err)
	}
	var st int
	if keep := s.emitSpinnerFrame(&st); !keep {
		t.Fatal("non-stalled tick with an open stream must keep going")
	}
	if strings.Contains(rec.Body.String(), "Thinking") {
		t.Fatalf("non-stalled tick must not emit a spinner: %s", rec.Body.String())
	}
	if err := s.Done(); err != nil {
		t.Fatal(err)
	}
	if keep := s.emitSpinnerFrame(&st); keep {
		t.Fatal("non-stalled tick after seal must stop the heartbeat")
	}
}

// user-visible text, then ends the turn. Each tool_call must reset the
// wedge clock, so a turn far longer than IdleWriteTimeout must NOT be cut
// as long as tool activity keeps flowing. Models a long tool-heavy turn.
type toolPingAgent struct {
	*fakeAgent
	gap       time.Duration
	count     int
	completed chan struct{}
	once      sync.Once
}

func (a *toolPingAgent) Prompt(ctx context.Context, sid acp.SessionId, _ []acp.ContentBlock) (acp.StopReason, error) {
	a.fakeAgent.mu.Lock()
	sink := a.fakeAgent.sinks[sid]
	a.fakeAgent.mu.Unlock()
	for i := 0; i < a.count; i++ {
		select {
		case <-ctx.Done():
			return acp.StopReasonCancelled, ctx.Err()
		case <-time.After(a.gap):
		}
		_ = sink.OnUpdate(context.Background(), acp.SessionNotification{
			SessionId: sid,
			Update: acp.SessionUpdate{
				ToolCall: &acp.SessionUpdateToolCall{ToolCallId: "t1", Title: "bash", Kind: acp.ToolKindExecute},
			},
		})
	}
	// Finally emit some text so the turn produces a real answer.
	_ = sink.OnUpdate(context.Background(), acp.SessionNotification{
		SessionId: sid,
		Update:    acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("ok")}},
	})
	a.once.Do(func() { close(a.completed) })
	return acp.StopReasonEndTurn, nil
}

// TestHandler_ToolActivityKeepsWedgeAlive is the end-to-end proof of
// Solution B: a turn that emits only tool_call updates (no text) for far
// longer than IdleWriteTimeout is NOT cut by the wedge backstop, because
// each tool_call resets the idle clock. Contrast TestHandler_
// IdleWriteTimeout_CutsWedgedTurn, where a genuinely hung agent (no text
// AND no tool activity) IS cut.
func TestHandler_ToolActivityKeepsWedgeAlive(t *testing.T) {
	a := &toolPingAgent{
		fakeAgent: &fakeAgent{},
		gap:       20 * time.Millisecond,
		count:     12, // ~240ms of tool activity, far beyond the 50ms idle window
		completed: make(chan struct{}),
	}
	rtr, err := router.New(router.Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	// Short idle window; heartbeat off (this test is purely about the
	// wedge clock, driven by tool_call resets).
	h := New(Config{Router: rtr, IdleWriteTimeout: 50 * time.Millisecond})

	idleFired := make(chan struct{})
	old := idleWriteCancelHook
	idleWriteCancelHook = func() { close(idleFired) }
	defer func() { idleWriteCancelHook = old }()

	body := mustJSON(map[string]any{
		"type": "query", "conversation_id": "c-toolping",
		"query": []map[string]any{{"role": "user", "content": "hi"}},
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		req, _ := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader(body))
		resp, derr := http.DefaultClient.Do(req)
		if derr == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()

	select {
	case <-a.completed:
	case <-idleFired:
		t.Fatal("wedge backstop cut a turn that had continuous tool activity")
	case <-time.After(5 * time.Second):
		t.Fatal("tool-active turn never completed")
	}
	select {
	case <-idleFired:
		t.Fatal("wedge backstop fired despite continuous tool activity")
	default:
	}
	<-done
}

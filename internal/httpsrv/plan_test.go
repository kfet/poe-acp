package httpsrv

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/poe-acp/internal/router"

	"github.com/kfet/poe-acp/internal/poeproto"
	"github.com/kfet/poe-acp/internal/statusline"
)

// TestSink_PlanRendersInsideTransientFrame proves the plan checklist is
// part of the keepalive frame (a replace_response carrying acc + the
// transient region) and is wiped by the next real write — it never
// becomes durable answer text.
func TestSink_PlanRendersInsideTransientFrame(t *testing.T) {
	rec := newSSERecorder(t)
	s := newSink(rec.w, 0, 0)

	if err := s.Text("answer so far"); err != nil {
		t.Fatal(err)
	}
	s.SetPlan([]statusline.PlanEntry{
		{Content: "wire config knobs", Status: "completed"},
		{Content: "render tool lines", Status: "in_progress"},
	})
	var spinTick int
	s.emitSpinnerFrame(&spinTick)
	if err := s.Text(" and more"); err != nil {
		t.Fatal(err)
	}
	if err := s.Done(); err != nil {
		t.Fatal(err)
	}

	events := parseSSE(t, rec.body())
	var frame string
	for _, e := range events {
		if e.event == "replace_response" && strings.Contains(e.text, "✅") {
			frame = e.text
		}
	}
	want := "answer so far\n\n> _Thinking._\n> ✅ wire config knobs\n> ⏳ render tool lines"
	if frame != want {
		t.Fatalf("keepalive frame = %q, want %q", frame, want)
	}
	// The final body on the wire carries no checklist.
	last := events[len(events)-2]
	if strings.Contains(last.text, "✅") {
		t.Fatalf("plan leaked into durable output: %q", last.text)
	}
}

// TestSink_PlanChangeForcesFrameWhileStreaming is the one-shot re-arm:
// with output flowing (stall not reached) a tick is normally a no-op,
// but a plan change forces exactly ONE frame so progress shows promptly.
func TestSink_PlanChangeForcesFrameWhileStreaming(t *testing.T) {
	rec := newSSERecorder(t)
	// stall is effectively infinite: only the plan re-arm can fire.
	s := newSink(rec.w, 0, time.Hour)

	if err := s.Text("streaming"); err != nil {
		t.Fatal(err)
	}
	var spinTick int
	// No plan yet, not stalled → no frame.
	s.emitSpinnerFrame(&spinTick)
	if spinTick != 0 {
		t.Fatalf("un-stalled tick emitted a frame (spinTick=%d)", spinTick)
	}
	s.SetPlan([]statusline.PlanEntry{{Content: "step one", Status: "in_progress"}})
	s.emitSpinnerFrame(&spinTick)
	if spinTick != 1 {
		t.Fatalf("plan change did not force a frame (spinTick=%d)", spinTick)
	}
	// The re-arm is one-shot: the next tick is a no-op again.
	s.emitSpinnerFrame(&spinTick)
	if spinTick != 1 {
		t.Fatalf("plan re-arm was not one-shot (spinTick=%d)", spinTick)
	}
	if err := s.Done(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.body(), "step one") {
		t.Fatalf("forced frame missing the plan: %s", rec.body())
	}
}

// TestSink_PlanIsNotUserVisibleOutput pins the transient contract:
// SetPlan touches neither clock and never marks realWritten.
func TestSink_PlanIsNotUserVisibleOutput(t *testing.T) {
	rec := newSSERecorder(t)
	s := newSink(rec.w, 0, 0)
	lw, lc := s.lastWrite.Load(), s.lastContent.Load()
	s.SetPlan([]statusline.PlanEntry{{Content: "step", Status: "pending"}})
	if s.realWritten() {
		t.Fatal("SetPlan must not mark realWritten")
	}
	if s.lastWrite.Load() != lw || s.lastContent.Load() != lc {
		t.Fatal("SetPlan must not touch the wedge/stall clocks")
	}
}

// TestSink_EmptyPlanAddsNothing — an all-empty plan renders no checklist,
// so the frame is exactly the spinner line.
func TestSink_EmptyPlanAddsNothing(t *testing.T) {
	rec := newSSERecorder(t)
	s := newSink(rec.w, 0, 0)
	s.SetPlan([]statusline.PlanEntry{{Content: "", Status: "pending"}})
	var spinTick int
	s.emitSpinnerFrame(&spinTick)
	if err := s.Done(); err != nil {
		t.Fatal(err)
	}
	for _, e := range parseSSE(t, rec.body()) {
		if e.event == "replace_response" && e.text != "" && e.text != "> _Thinking._" {
			t.Fatalf("unexpected frame body %q", e.text)
		}
	}
}

// sseRecorder bundles the httptest recorder and SSE writer used by the
// sink tests, and reads the body back after the writer is done with it.
type sseRecorder struct {
	rec *httptest.ResponseRecorder
	w   *poeproto.SSEWriter
}

func newSSERecorder(t *testing.T) *sseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	w, err := poeproto.NewSSEWriter(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Meta(); err != nil {
		t.Fatal(err)
	}
	return &sseRecorder{rec: rec, w: w}
}

func (r *sseRecorder) body() string { return r.rec.Body.String() }

// toolOnlyAgent emits a single tool_call — no message text at all — then
// blocks until Cancel. Models the show_tools case where the FIRST
// user-visible byte of a turn is a durable tool line.
type toolOnlyAgent struct {
	*fakeAgent
	cancelled chan struct{}
	cancelOne sync.Once
}

func (a *toolOnlyAgent) Prompt(_ context.Context, sid acp.SessionId, _ []acp.ContentBlock) (acp.StopReason, error) {
	a.fakeAgent.mu.Lock()
	sink := a.fakeAgent.sinks[sid]
	a.fakeAgent.mu.Unlock()
	_ = sink.OnUpdate(context.Background(), acp.SessionNotification{
		SessionId: sid,
		Update: acp.SessionUpdate{ToolCall: &acp.SessionUpdateToolCall{
			ToolCallId: "t1", Title: "go test ./...", Kind: acp.ToolKindExecute,
		}},
	})
	<-a.cancelled
	return acp.StopReasonCancelled, nil
}

func (a *toolOnlyAgent) Cancel(_ context.Context, _ acp.SessionId) error {
	a.cancelOne.Do(func() { close(a.cancelled) })
	return nil
}

// TestHandler_ToolLineIsFirstOutputAndGatesCancel pins the deliberate
// behaviour change that show_tools brings: a durable tool line IS
// user-visible output, so it flips the gated-cancel discriminator. A
// disconnect after a tool line is treated as a real user Stop
// (session/cancel forwarded) rather than a pre-output transport drop
// (absorbed + buffered for redrive). This narrows the absorb window for
// tool-using turns — by design: the user has seen output, so a drop is
// far more likely a Stop than a transport glitch.
func TestHandler_ToolLineIsFirstOutputAndGatesCancel(t *testing.T) {
	a := &toolOnlyAgent{fakeAgent: &fakeAgent{}, cancelled: make(chan struct{})}
	rtr, err := router.New(router.Config{
		Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour,
		Defaults: router.Options{ShowTools: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Router: rtr, HeartbeatInterval: 0})

	srv := httptest.NewServer(h)
	defer srv.Close()

	body := mustJSON(map[string]any{
		"type": "query", "conversation_id": "c-toolcancel",
		"query": []map[string]any{{"role": "user", "content": "hi"}},
	})
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	// Wait for the tool line itself to land on the wire — it is the only
	// content this turn produces.
	gotLine := make(chan struct{})
	go func() {
		buf := make([]byte, 4096)
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 && strings.Contains(string(buf[:n]), "go test") {
				close(gotLine)
				return
			}
			if rerr != nil {
				return
			}
		}
	}()
	select {
	case <-gotLine:
	case <-time.After(3 * time.Second):
		t.Fatal("tool line never reached the client")
	}
	cancel()
	_ = resp.Body.Close()
	select {
	case <-a.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("disconnect after a tool line must propagate session/cancel")
	}
}

package httpsrv

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/poe-acp/internal/poeproto"
	"github.com/kfet/poe-acp/internal/router"
)

// wedgeAgent emits NO user-visible output and blocks until its prompt
// context is cancelled. Models a hung agent (no tokens, client still
// connected) — exactly what the idle-write backstop must cut.
type wedgeAgent struct {
	*fakeAgent
	returned chan struct{}
	once     sync.Once
}

func (a *wedgeAgent) Prompt(ctx context.Context, _ acp.SessionId, _ []acp.ContentBlock) (acp.StopReason, error) {
	<-ctx.Done()
	a.once.Do(func() { close(a.returned) })
	return acp.StopReasonCancelled, ctx.Err()
}

func TestHandler_IdleWriteTimeout_CutsWedgedTurn(t *testing.T) {
	a := &wedgeAgent{fakeAgent: &fakeAgent{}, returned: make(chan struct{})}
	rtr, err := router.New(router.Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	// Tiny idle timeout so the wedge is cut quickly; heartbeat ON to
	// prove keepalive frames do NOT reset the idle clock.
	h := New(Config{Router: rtr, HeartbeatInterval: 20 * time.Millisecond, IdleWriteTimeout: 40 * time.Millisecond})

	fired := make(chan struct{})
	old := idleWriteCancelHook
	idleWriteCancelHook = func() { close(fired) }
	defer func() { idleWriteCancelHook = old }()

	body := mustJSON(map[string]any{
		"type": "query", "conversation_id": "c-wedge",
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
			_ = resp.Body.Close()
		}
	}()

	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("idle-write backstop never fired")
	}
	select {
	case <-a.returned:
	case <-time.After(3 * time.Second):
		t.Fatal("wedged prompt never returned after idle cancel")
	}
	<-done
}

// progressAgent emits a user-visible chunk every `gap` for `count`
// iterations, then ends the turn. Each chunk resets the idle clock, so a
// turn whose total runtime far exceeds IdleWriteTimeout must NOT be cut as
// long as output keeps flowing. Models a long, actively-working turn.
type progressAgent struct {
	*fakeAgent
	gap       time.Duration
	count     int
	completed chan struct{} // closed iff the turn ran to completion (never cancelled)
	once      sync.Once
}

func (a *progressAgent) Prompt(ctx context.Context, sid acp.SessionId, _ []acp.ContentBlock) (acp.StopReason, error) {
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
				AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
					Content: acp.TextBlock("tick\n"),
				},
			},
		})
	}
	a.once.Do(func() { close(a.completed) })
	return acp.StopReasonEndTurn, nil
}

// TestHandler_NoTurnCeiling_ProgressKeepsTurnAlive locks the contract that
// with TurnTimeout==0 (no absolute ceiling) a turn producing periodic
// output survives well past IdleWriteTimeout — the progress-resetting idle
// backstop is the sole guard and never fires while output flows.
func TestHandler_NoTurnCeiling_ProgressKeepsTurnAlive(t *testing.T) {
	a := &progressAgent{
		fakeAgent: &fakeAgent{},
		gap:       20 * time.Millisecond,
		count:     12, // ~240ms total, far beyond the 50ms idle window
		completed: make(chan struct{}),
	}
	rtr, err := router.New(router.Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	// No TurnTimeout (0 => unbounded). Short idle window: each emitted
	// chunk must reset it, so the wedge backstop must NOT fire.
	h := New(Config{Router: rtr, IdleWriteTimeout: 50 * time.Millisecond})

	idleFired := make(chan struct{})
	old := idleWriteCancelHook
	idleWriteCancelHook = func() { close(idleFired) }
	defer func() { idleWriteCancelHook = old }()

	body := mustJSON(map[string]any{
		"type": "query", "conversation_id": "c-progress",
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
			// Drain the SSE stream: a real Poe client keeps the
			// connection open and reads, so emitted chunks land and
			// reset the idle clock. Closing early would disconnect and
			// wrongly trip the wedge backstop.
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()

	select {
	case <-a.completed:
	case <-idleFired:
		t.Fatal("idle-write backstop cut an actively-progressing turn")
	case <-time.After(5 * time.Second):
		t.Fatal("progressing turn never completed")
	}
	// Ensure the idle hook did not also fire in a late race.
	select {
	case <-idleFired:
		t.Fatal("idle-write backstop fired despite continuous progress")
	default:
	}
	<-done
}

func TestIdleCheckInterval(t *testing.T) {
	if got := idleCheckInterval(2 * time.Minute); got != 30*time.Second {
		t.Fatalf("large timeout: got %v", got)
	}
	if got := idleCheckInterval(20 * time.Millisecond); got != 10*time.Millisecond {
		t.Fatalf("floor: got %v", got)
	}
}

func TestSink_IdleSince(t *testing.T) {
	sse, err := poeproto.NewSSEWriter(httptest.NewRecorder())
	if err != nil {
		t.Fatal(err)
	}
	s := newSink(sse, 0)
	if s.idleSince() > time.Second {
		t.Fatalf("fresh sink idle too high: %v", s.idleSince())
	}
	time.Sleep(15 * time.Millisecond)
	before := s.idleSince()
	_ = s.Text("x")
	if s.idleSince() >= before {
		t.Fatalf("Text did not reset idle clock: before=%v after=%v", before, s.idleSince())
	}
}

package httpsrv

import (
	"bytes"
	"context"
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

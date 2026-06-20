package httpsrv

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/poe-acp/internal/router"
)

// absorbAgent emits no output until released, modelling a turn whose
// client dropped pre-output. Prompt count is tracked so the redrive can be
// asserted to be served from the buffer (not re-run).
type absorbAgent struct {
	*fakeAgent
	entered     chan struct{}
	release     chan struct{}
	promptCalls int32
}

func (a *absorbAgent) Prompt(_ context.Context, sid acp.SessionId, _ []acp.ContentBlock) (acp.StopReason, error) {
	atomic.AddInt32(&a.promptCalls, 1)
	a.fakeAgent.mu.Lock()
	sink := a.fakeAgent.sinks[sid]
	a.fakeAgent.mu.Unlock()
	close(a.entered)
	<-a.release
	_ = sink.OnUpdate(context.Background(), acp.SessionNotification{
		SessionId: sid,
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
				Content: acp.TextBlock("the answer\n"),
			},
		},
	})
	return acp.StopReasonEndTurn, nil
}

// A pre-output client disconnect is absorbed: the decoupled turn runs to
// completion and the answer is buffered, then served verbatim on the Poe
// redrive without re-running the agent.
func TestHandler_PreOutputDropBuffersAndRedriveServes(t *testing.T) {
	a := &absorbAgent{fakeAgent: &fakeAgent{}, entered: make(chan struct{}), release: make(chan struct{})}
	rtr, err := router.New(router.Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Router: rtr, HeartbeatInterval: 0})

	body := mustJSON(map[string]any{
		"type": "query", "conversation_id": "c1", "user_id": "u", "message_id": "req",
		"query": []map[string]any{{"role": "user", "content": "hi", "message_id": "m1"}},
	})

	// Request 1: cancel the request ctx BEFORE any output → absorb path.
	// The absorbDecidedHook lets us release the blocked turn only AFTER the
	// watcher has latched its decision, so the test is race-free.
	decided := make(chan struct{})
	absorbDecidedHook = func() { close(decided) }
	defer func() { absorbDecidedHook = nil }()

	ctx, cancel := context.WithCancel(context.Background())
	rec1 := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body)).WithContext(ctx)
		h.ServeHTTP(rec1, req)
		close(done)
	}()
	<-a.entered
	cancel()         // pre-output transport drop
	<-decided        // watcher latched absorb decision
	close(a.release) // let the decoupled turn finish
	<-done

	if got := atomic.LoadInt32(&a.promptCalls); got != 1 {
		t.Fatalf("turn1 prompt calls=%d want 1", got)
	}

	// Request 2: Poe redrives the same query (same latest message_id) →
	// served from the buffer, agent NOT re-run.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body)))
	out := rec2.Body.String()
	if !strings.Contains(out, "the answer") {
		t.Fatalf("redrive did not serve buffered answer: %q", out)
	}
	if !strings.Contains(out, "event: done") {
		t.Fatalf("redrive missing done event: %q", out)
	}
	if got := atomic.LoadInt32(&a.promptCalls); got != 1 {
		t.Fatalf("redrive must be served from buffer, not re-run: prompt calls=%d", got)
	}
}

// gatedAgent blocks the FIRST prompt until released, then answers every
// subsequent prompt immediately. Models a turn whose client dropped
// pre-output, followed by the user sending a brand-new message (a normal
// next turn) rather than a same-message redrive.
type gatedAgent struct {
	*fakeAgent
	entered     chan struct{}
	release     chan struct{}
	promptCalls int32
}

func (a *gatedAgent) Prompt(_ context.Context, sid acp.SessionId, _ []acp.ContentBlock) (acp.StopReason, error) {
	n := atomic.AddInt32(&a.promptCalls, 1)
	a.fakeAgent.mu.Lock()
	sink := a.fakeAgent.sinks[sid]
	a.fakeAgent.mu.Unlock()
	if n == 1 {
		close(a.entered)
		<-a.release
	}
	_ = sink.OnUpdate(context.Background(), acp.SessionNotification{
		SessionId: sid,
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
				Content: acp.TextBlock("answer-" + string(rune('0'+n)) + "\n"),
			},
		},
	})
	return acp.StopReasonEndTurn, nil
}

// After a pre-output drop is absorbed, the user's NEXT message (a new turn,
// not a same-message redrive) must answer cleanly from the reused session
// with NO user-visible error card. This is the common production case the
// buffer alone does not cover. Verifies the reseed/reuse path is graceful.
func TestHandler_NewMessageAfterAbsorbedDropAnswersCleanly(t *testing.T) {
	a := &gatedAgent{fakeAgent: &fakeAgent{}, entered: make(chan struct{}), release: make(chan struct{})}
	rtr, err := router.New(router.Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Router: rtr, HeartbeatInterval: 0})

	body1 := mustJSON(map[string]any{
		"type": "query", "conversation_id": "c1", "user_id": "u", "message_id": "req1",
		"query": []map[string]any{{"role": "user", "content": "first", "message_id": "m1"}},
	})

	decided := make(chan struct{})
	absorbDecidedHook = func() { close(decided) }
	defer func() { absorbDecidedHook = nil }()

	ctx, cancel := context.WithCancel(context.Background())
	rec1 := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body1)).WithContext(ctx)
		h.ServeHTTP(rec1, req)
		close(done)
	}()
	<-a.entered
	cancel()         // pre-output transport drop on turn 1
	<-decided        // watcher latched absorb decision
	close(a.release) // let the decoupled turn 1 finish + buffer
	<-done

	// Turn 2: the user sends a brand-new message. The transcript carries
	// the prior (dropped) user turn plus the new one — a benign append, so
	// the hot session is reused and the new turn answers cleanly.
	body2 := mustJSON(map[string]any{
		"type": "query", "conversation_id": "c1", "user_id": "u", "message_id": "req2",
		"query": []map[string]any{
			{"role": "user", "content": "first", "message_id": "m1"},
			{"role": "user", "content": "second", "message_id": "m2"},
		},
	})
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body2)))
	out := rec2.Body.String()
	if strings.Contains(out, "event: error") {
		t.Fatalf("new turn after absorbed drop surfaced an error card: %q", out)
	}
	if !strings.Contains(out, "answer-2") {
		t.Fatalf("new turn did not answer cleanly: %q", out)
	}
	if !strings.Contains(out, "event: done") {
		t.Fatalf("new turn missing done event: %q", out)
	}
	if got := atomic.LoadInt32(&a.promptCalls); got != 2 {
		t.Fatalf("expected 2 prompt calls (turn1 + turn2), got %d", got)
	}
}

// A prompt that returns an error (here: an empty user message rejected by
// the router) is logged by the handler. Covers the err != nil branch.
func TestHandler_PromptErrorLogged(t *testing.T) {
	rtr, err := router.New(router.Config{Agent: &fakeAgent{}, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Router: rtr, HeartbeatInterval: 0})
	body := mustJSON(map[string]any{
		"type": "query", "conversation_id": "c1", "user_id": "u", "message_id": "m1",
		"query": []map[string]any{{"role": "user", "content": "", "message_id": "m1"}},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body)))
	if !strings.Contains(rec.Body.String(), "event: error") {
		t.Fatalf("expected error event: %s", rec.Body.String())
	}
}

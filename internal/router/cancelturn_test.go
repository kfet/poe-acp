package router

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// tokenSink records the turn token the router binds to it.
type tokenSink struct {
	captureSink
	token atomic.Uint64
}

func (s *tokenSink) SetTurnToken(t uint64) { s.token.Store(t) }

// TestCancelTurn_IsTurnScoped is the Bug 2 regression: a cancel aimed at a
// finished turn must NOT kill the follow-up that replaced it. The blunt
// session-scoped Cancel (kept for other callers) does kill it — asserted
// last, which is exactly what the handler used to do.
func TestCancelTurn_IsTurnScoped(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan string, 4)
	agent := newFakeAgent(func(ctx context.Context, _ *fakeAgent, _ acp.SessionId, text string) (acp.StopReason, error) {
		entered <- text
		if text == "second" {
			select {
			case <-gate:
			case <-ctx.Done():
			}
		}
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})

	// Turn A runs to completion; keep its token.
	sa := &tokenSink{}
	if err := r.Prompt(context.Background(), "c", "u",
		[]Turn{{Role: "user", Content: "first", MessageID: "m1"}}, Options{}, sa); err != nil {
		t.Fatalf("turn A: %v", err)
	}
	<-entered
	tokenA := sa.token.Load()
	if tokenA == 0 {
		t.Fatal("router never bound a turn token")
	}

	// Turn B is now the live turn.
	sb := &tokenSink{}
	d := make(chan error, 1)
	go func() {
		d <- r.Prompt(context.Background(), "c", "u",
			[]Turn{{Role: "user", Content: "first", MessageID: "m1"},
				{Role: "user", Content: "second", MessageID: "m2"}}, Options{}, sb)
	}()
	<-entered

	// A's owner firing late must not touch B.
	if err := r.CancelTurn(context.Background(), "c", tokenA); err != nil {
		t.Fatalf("CancelTurn(stale): %v", err)
	}
	if n := atomic.LoadInt32(&agent.cancelCalls); n != 0 {
		t.Fatalf("stale token cancelled the live turn (%d calls)", n)
	}
	select {
	case <-d:
		t.Fatal("turn B was cancelled by turn A's token")
	case <-time.After(50 * time.Millisecond):
	}

	// B's own token does cancel it.
	if err := r.CancelTurn(context.Background(), "c", sb.token.Load()); err != nil {
		t.Fatalf("CancelTurn(live): %v", err)
	}
	if n := atomic.LoadInt32(&agent.cancelCalls); n != 1 {
		t.Fatalf("live token did not cancel: %d calls", n)
	}
	close(gate)
	if err := <-d; err != nil {
		t.Fatalf("turn B: %v", err)
	}
}

// TestCancelTurn_NoOpPaths: zero token, unknown conversation, and a
// conversation with no live turn are all silent no-ops.
func TestCancelTurn_NoOpPaths(t *testing.T) {
	agent := newFakeAgent(func(context.Context, *fakeAgent, acp.SessionId, string) (acp.StopReason, error) {
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})
	ctx := context.Background()
	if err := r.CancelTurn(ctx, "c", 0); err != nil {
		t.Fatalf("zero token: %v", err)
	}
	if err := r.CancelTurn(ctx, "nosuch", 7); err != nil {
		t.Fatalf("unknown conv: %v", err)
	}
	sink := &captureSink{}
	if err := r.Prompt(ctx, "c", "u",
		[]Turn{{Role: "user", Content: "hi", MessageID: "m1"}}, Options{}, sink); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	// Session exists, no turn is live.
	if err := r.CancelTurn(ctx, "c", 7); err != nil {
		t.Fatalf("idle session: %v", err)
	}
	if n := atomic.LoadInt32(&agent.cancelCalls); n != 0 {
		t.Fatalf("no-op paths issued %d cancels", n)
	}
}

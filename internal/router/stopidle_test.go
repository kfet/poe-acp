package router

import "testing"

// TestSessionQueue_StopIfIdle_RacingPushNotLost pins the invariant that
// ResetSession/gcOnce rely on: the "is it idle?" check and the "close it"
// transition must be ONE atomic step. Both callers hold Router.mu, but a
// submitter that already resolved its *sessionState can call push without
// it — so a separate idle() then stop() leaves a gap in which a turn is
// queued, detached by stop(), and its done channel never closed, parking
// submitTurn (and its HTTP handler) forever.
//
// stopIfIdle closes the gap: a turn that lands before it wins (the session
// is not torn down), one that lands after is cleanly rejected by push.
func TestSessionQueue_StopIfIdle_RacingPushNotLost(t *testing.T) {
	sq := newSessionQueue()

	// A turn queued in what used to be the gap must defeat the teardown.
	req := &turnReq{kind: turnUser, done: make(chan struct{})}
	if !sq.push(req) {
		t.Fatal("push onto a fresh queue was rejected")
	}
	pending, ok := sq.stopIfIdle()
	if ok {
		t.Fatal("stopIfIdle tore down a queue holding a turn — that turn's done would never close")
	}
	if pending != nil {
		t.Fatalf("stopIfIdle must not detach anything when it declines: %v", pending)
	}
	select {
	case <-req.done:
		t.Fatal("declining stopIfIdle must leave the turn runnable, not closed")
	default:
	}

	// Once the turn is drained the queue is idle and teardown succeeds.
	if got := sq.popOrWait(make(chan struct{})); got != req {
		t.Fatalf("popOrWait returned %v, want the queued turn", got)
	}
	sq.finishInFlight()
	pending, ok = sq.stopIfIdle()
	if !ok {
		t.Fatal("stopIfIdle declined an idle queue")
	}
	if pending != nil {
		t.Fatalf("an idle queue has nothing pending, got %v", pending)
	}

	// Post-teardown submitters are rejected outright, never silently lost.
	if sq.push(&turnReq{kind: turnUser, done: make(chan struct{})}) {
		t.Fatal("push succeeded after stopIfIdle")
	}
}

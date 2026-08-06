package router

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// cancelSignalAgent is a fakeAgent whose Cancel is observable: the test's
// blocked Prompt hook waits on `cancelled`, so "session/cancel reached the
// agent" and "the in-flight turn unwinds" are causally linked instead of
// timing-dependent.
type cancelSignalAgent struct {
	*fakeAgent
	cancelled chan struct{}
	once      sync.Once
}

func (c *cancelSignalAgent) Cancel(ctx context.Context, sid acp.SessionId) error {
	c.once.Do(func() { close(c.cancelled) })
	return c.fakeAgent.Cancel(ctx, sid)
}

// waitQueued blocks until conv's session has n reqs waiting behind the
// in-flight turn. The queue is internal state with no subscription point,
// so this is the documented last-resort poll (10ms, generous ceiling).
func waitQueued(t *testing.T, r *Router, convID string, n int) *sessionState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		st := r.sessions[convID]
		r.mu.Unlock()
		if st != nil {
			st.queue.mu.Lock()
			got := len(st.queue.q)
			st.queue.mu.Unlock()
			if got >= n {
				return st
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("conv %s never had %d queued turns", convID, n)
	return nil
}

// TestEvictSession_MidTurnIsBoundedAndFinalises is the Hazard A regression.
// Evicting a session while a turn is blocked inside Agent.Prompt must:
//   - cancel the agent-side prompt;
//   - finalise the in-flight turn's sink with a real terminal event;
//   - finalise the shed queued turn's sink too (never an empty answer);
//   - never park any goroutine — both Prompt calls return promptly.
//
// Before the fix evictSession closed drainStop first, so the runner's
// end-of-turn ack never came back and both Prompt calls hung forever.
func TestEvictSession_MidTurnIsBoundedAndFinalises(t *testing.T) {
	ag := &cancelSignalAgent{cancelled: make(chan struct{})}
	inPrompt := make(chan struct{})
	var once sync.Once
	ag.fakeAgent = newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		once.Do(func() { close(inPrompt) })
		<-ag.cancelled
		a.emit(sid, "partial")
		return acp.StopReasonCancelled, nil
	})
	r, _ := New(Config{Agent: ag, StateDir: t.TempDir(), SessionTTL: time.Hour})

	s1, s2 := &captureSink{}, &captureSink{}
	d1, d2 := make(chan error, 1), make(chan error, 1)
	q1 := []Turn{{Role: "user", Content: "first", MessageID: "m1"}}
	q2 := append(append([]Turn{}, q1...), Turn{Role: "user", Content: "second", MessageID: "m2"})
	go func() { d1 <- r.Prompt(context.Background(), "c", "u", q1, Options{}, s1) }()
	<-inPrompt
	go func() { d2 <- r.Prompt(context.Background(), "c", "u", q2, Options{}, s2) }()
	st := waitQueued(t, r, "c", 1)

	// The bug's trigger: evict while turn #1 is mid-Prompt.
	r.evictSession("c", st)

	// Queued turn: finalised with a real, non-empty terminal event.
	select {
	case err := <-d2:
		if err == nil {
			t.Fatal("shed turn returned nil error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shed queued turn hung")
	}
	s2.mu.Lock()
	shedErr, shedDone := s2.errText, s2.done
	s2.mu.Unlock()
	if shedErr == "" || !shedDone {
		t.Fatalf("queued turn not finalised: err=%q done=%v", shedErr, shedDone)
	}

	// In-flight turn: cancelled, finalised, and bounded.
	select {
	case <-ag.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("eviction never cancelled the in-flight turn")
	}
	select {
	case <-d1:
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight turn hung after eviction")
	}
	s1.mu.Lock()
	rep, done1 := s1.replaceText, s1.done
	s1.mu.Unlock()
	if rep != "_(cancelled)_" || !done1 {
		t.Fatalf("in-flight sink not finalised: replace=%q done=%v", rep, done1)
	}

	// Teardown completes once the turn unwound.
	select {
	case <-st.drainStop:
	case <-time.After(5 * time.Second):
		t.Fatal("evicted session never tore down")
	}
	<-st.runStop
}

// TestEvictSession_DivergenceMidTurn drives the same hazard through its real
// trigger: the user edits an earlier message while a turn is streaming, so
// getOrCreate's transcript-divergence branch evicts the live session.
func TestEvictSession_DivergenceMidTurn(t *testing.T) {
	ag := &cancelSignalAgent{cancelled: make(chan struct{})}
	inPrompt := make(chan struct{})
	var once sync.Once
	ag.fakeAgent = newFakeAgent(func(_ context.Context, _ *fakeAgent, _ acp.SessionId, text string) (acp.StopReason, error) {
		if text == "first" {
			once.Do(func() { close(inPrompt) })
			<-ag.cancelled
			return acp.StopReasonCancelled, nil
		}
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{Agent: ag, StateDir: t.TempDir(), SessionTTL: time.Hour})

	s1, s2 := &captureSink{}, &captureSink{}
	d1, d2 := make(chan error, 1), make(chan error, 1)
	go func() {
		d1 <- r.Prompt(context.Background(), "c", "u",
			[]Turn{{Role: "user", Content: "first", MessageID: "m1"}}, Options{}, s1)
	}()
	<-inPrompt
	// Same message id, edited content => transcriptDiverged => evictSession.
	go func() {
		d2 <- r.Prompt(context.Background(), "c", "u",
			[]Turn{{Role: "user", Content: "first (edited)", MessageID: "m1"}}, Options{}, s2)
	}()
	select {
	case err := <-d2:
		if err != nil {
			t.Fatalf("edited turn failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("edited turn hung behind the evicted session")
	}
	select {
	case <-d1:
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight turn hung after divergence eviction")
	}
	s1.mu.Lock()
	defer s1.mu.Unlock()
	if !s1.done {
		t.Fatal("evicted in-flight turn never finalised its sink")
	}
}

// TestEvictSession_TeardownGraceTimeout: an agent that ignores session/cancel
// must not keep an evicted session's goroutines alive forever. Teardown is
// forced after evictDrainGrace, and the turn still finalises its sink when
// Prompt eventually returns (through the drainStop-guarded ack path).
func TestEvictSession_TeardownGraceTimeout(t *testing.T) {
	release := make(chan struct{})
	inPrompt := make(chan struct{})
	var once sync.Once
	agent := newFakeAgent(func(_ context.Context, _ *fakeAgent, _ acp.SessionId, _ string) (acp.StopReason, error) {
		once.Do(func() { close(inPrompt) })
		<-release // deliberately ignores session/cancel
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})
	r.evictDrainGrace = 10 * time.Millisecond
	// A failing cancel RPC must not derail teardown (debug-logged only).
	agent.cancelErr = errors.New("cancel boom")

	sink := &captureSink{}
	d := make(chan error, 1)
	go func() {
		d <- r.Prompt(context.Background(), "c", "u",
			[]Turn{{Role: "user", Content: "hi", MessageID: "m1"}}, Options{}, sink)
	}()
	<-inPrompt
	r.mu.Lock()
	st := r.sessions["c"]
	r.mu.Unlock()
	r.evictSession("c", st)

	select {
	case <-st.drainStop:
	case <-time.After(5 * time.Second):
		t.Fatal("teardown did not force after the grace window")
	}
	close(release)
	select {
	case <-d:
	case <-time.After(5 * time.Second):
		t.Fatal("turn hung after forced teardown")
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if !sink.done {
		t.Fatal("sink not finalised after forced teardown")
	}
	if atomic.LoadInt32(&agent.cancelCalls) == 0 {
		t.Fatal("eviction did not issue session/cancel")
	}
}

// newDetachedSession builds a sessionState with no drain/run goroutines, so
// runOneTurn's teardown escapes can be driven directly.
func newDetachedSession(chunkCap int) *sessionState {
	return &sessionState{
		convID:    "c",
		sessionID: "sid",
		chunkCh:   make(chan chunkMsg, chunkCap),
		drainStop: make(chan struct{}),
		runStop:   make(chan struct{}),
		queue:     newSessionQueue(),
	}
}

// TestRunOneTurn_BeginTurnAfterTeardown: a turn that reaches the runner after
// its session was torn down must abort with a user-visible message instead of
// blocking on a chunk channel nobody drains.
func TestRunOneTurn_BeginTurnAfterTeardown(t *testing.T) {
	agent := newFakeAgent(func(context.Context, *fakeAgent, acp.SessionId, string) (acp.StopReason, error) {
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})
	st := newDetachedSession(0)
	close(st.drainStop)

	sink := &captureSink{}
	req := &turnReq{kind: turnUser, ctx: context.Background(), sink: sink, done: make(chan struct{})}
	done := make(chan struct{})
	go func() { defer close(done); r.runOneTurn(st, req) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runOneTurn blocked on beginTurn after teardown")
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.errText == "" || !sink.done {
		t.Fatalf("turn not finalised: err=%q done=%v", sink.errText, sink.done)
	}
	if req.err == nil {
		t.Fatal("req.err not set")
	}
	if atomic.LoadInt32(&agent.prompts) != 0 {
		t.Fatal("prompt dispatched to a torn-down session")
	}
}

// TestRunOneTurn_EndTurnAckTimeout: if the drain goroutine is gone but
// drainStop is somehow still open (a lost drain), the end-of-turn ack must
// time out and the turn must still finalise. Never an unbounded wait.
func TestRunOneTurn_EndTurnAckTimeout(t *testing.T) {
	agent := newFakeAgent(func(context.Context, *fakeAgent, acp.SessionId, string) (acp.StopReason, error) {
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})
	r.endTurnAckTimeout = 10 * time.Millisecond
	st := newDetachedSession(4) // buffered: sends succeed, nothing ever acks

	sink := &captureSink{}
	req := &turnReq{kind: turnUser, ctx: context.Background(), sink: sink, done: make(chan struct{})}
	done := make(chan struct{})
	go func() { defer close(done); r.runOneTurn(st, req) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runOneTurn parked on the end-of-turn ack")
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if !sink.done {
		t.Fatal("sink not finalised after ack timeout")
	}
}

// TestRunOneTurn_EndTurnAfterTeardown: the ack path also escapes when the
// session is torn down while the runner is trying to hand off its endTurn
// sentinel (chunkCh full, drain goroutine gone).
func TestRunOneTurn_EndTurnAfterTeardown(t *testing.T) {
	entered := make(chan struct{})
	agent := newFakeAgent(func(context.Context, *fakeAgent, acp.SessionId, string) (acp.StopReason, error) {
		close(entered)
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})
	// cap 1: beginTurn fills the buffer, so the endTurn send blocks and
	// drainStop is the only ready case — no select coin-flip.
	st := newDetachedSession(1)

	sink := &captureSink{}
	req := &turnReq{kind: turnUser, ctx: context.Background(), sink: sink, done: make(chan struct{})}
	done := make(chan struct{})
	go func() { defer close(done); r.runOneTurn(st, req) }()
	<-entered
	close(st.drainStop)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runOneTurn parked after teardown")
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if !sink.done {
		t.Fatal("sink not finalised after teardown")
	}
}

// TestRunOneTurn_AckLostToTeardown: the endTurn sentinel was handed off but
// the drain goroutine died before acking (session evicted). The runner must
// give up on the ack and finalise, not park on endDone.
func TestRunOneTurn_AckLostToTeardown(t *testing.T) {
	agent := newFakeAgent(func(context.Context, *fakeAgent, acp.SessionId, string) (acp.StopReason, error) {
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})
	st := newDetachedSession(0) // unbuffered: the test IS the drain

	sink := &captureSink{}
	req := &turnReq{kind: turnUser, ctx: context.Background(), sink: sink, done: make(chan struct{})}
	done := make(chan struct{})
	go func() { defer close(done); r.runOneTurn(st, req) }()
	if msg := <-st.chunkCh; msg.beginTurn == nil {
		t.Fatal("expected beginTurn")
	}
	// Receiving the sentinel proves the runner is inside the ack wait; drop
	// it on the floor (never close it) and tear the session down.
	if msg := <-st.chunkCh; msg.endTurn == nil {
		t.Fatal("expected endTurn")
	}
	close(st.drainStop)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runOneTurn parked on a lost ack")
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if !sink.done {
		t.Fatal("sink not finalised after a lost ack")
	}
}

// TestSessionQueue_WaitIdleTimeout: waitIdle is bounded and reports the
// in-flight turn honestly when the window expires.
func TestSessionQueue_WaitIdleTimeout(t *testing.T) {
	sq := newSessionQueue()
	if !sq.waitIdle(time.Second) {
		t.Fatal("empty queue is idle")
	}
	req := &turnReq{kind: turnReaction, done: make(chan struct{})}
	if !sq.push(req) {
		t.Fatal("push failed")
	}
	if got := sq.popOrWait(make(chan struct{})); got != req {
		t.Fatal("pop failed")
	}
	if sq.waitIdle(10 * time.Millisecond) {
		t.Fatal("waitIdle reported idle with a turn in flight")
	}
	// A turn that finishes wakes the waiter.
	go sq.finishInFlight()
	if !sq.waitIdle(5 * time.Second) {
		t.Fatal("waitIdle missed the finish signal")
	}
}

package router

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// TestRouter_TurnFIFO verifies that two prompts on the same conv are
// serialised through the per-session queue in submission order — the
// second waits until the first's runner pass completes.
func TestRouter_TurnFIFO(t *testing.T) {
	gate := make(chan struct{})
	var order []string
	var mu sync.Mutex
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, text string) (acp.StopReason, error) {
		mu.Lock()
		order = append(order, "start:"+text)
		mu.Unlock()
		if text == "first" {
			<-gate
		}
		a.emit(sid, "ok-"+text)
		mu.Lock()
		order = append(order, "end:"+text)
		mu.Unlock()
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})

	s1, s2 := &captureSink{}, &captureSink{}
	d1, d2 := make(chan error, 1), make(chan error, 1)
	go func() {
		d1 <- r.Prompt(context.Background(), "c", "u",
			[]Turn{{Role: "user", Content: "first"}}, Options{}, s1)
	}()
	// Give #1 time to enter the runner and block on `gate`.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&agent.prompts) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if atomic.LoadInt32(&agent.prompts) != 1 {
		t.Fatalf("first prompt didn't reach agent")
	}
	go func() {
		d2 <- r.Prompt(context.Background(), "c", "u",
			[]Turn{{Role: "user", Content: "second"}}, Options{}, s2)
	}()
	// #2 must NOT be in the agent yet — first is still blocked.
	time.Sleep(20 * time.Millisecond)
	if atomic.LoadInt32(&agent.prompts) != 1 {
		t.Fatalf("second prompt entered agent before first finished")
	}
	close(gate)
	if err := <-d1; err != nil {
		t.Fatalf("p1: %v", err)
	}
	if err := <-d2; err != nil {
		t.Fatalf("p2: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"start:first", "end:first", "start:second", "end:second"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("order=%v want %v", order, want)
	}
}

// TestRouter_PromptCtxCancel: when the caller's ctx is cancelled
// while a prompt is queued/running, Prompt returns but only AFTER the
// runner has finished (sink.Done has been called). Guards the
// invariant that the shared sink isn't written to after the HTTP
// handler returns.
func TestRouter_PromptCtxCancel(t *testing.T) {
	release := make(chan struct{})
	agent := newFakeAgent(func(ctx context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		// Deliberately ignore ctx so the runner stays inside Agent.Prompt
		// until the test releases it. This tests that Prompt waits for
		// the runner even after ctx fires.
		<-release
		a.emit(sid, "late")
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	sink := &captureSink{}
	d := make(chan error, 1)
	go func() {
		d <- r.Prompt(ctx, "c", "u",
			[]Turn{{Role: "user", Content: "hi"}}, Options{}, sink)
	}()
	// Wait for runner to reach agent.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&agent.prompts) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	// Prompt must not return until runner has unblocked. Release agent.
	select {
	case <-d:
		t.Fatalf("Prompt returned before runner finished")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-d; err == nil {
		// Could be ctx.Err() or nil — both acceptable. But sink.done MUST be true.
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if !sink.done {
		t.Fatalf("sink.Done not called by runner before Prompt return")
	}
}

// TestRouter_ReactionShedOldest fills the queue with reactions and
// adds one more; the oldest reaction must be shed, never a user
// prompt. Uses a blocking first turn to keep the runner busy.
func TestRouter_ReactionShedOldest(t *testing.T) {
	gate := make(chan struct{})
	agent := newFakeAgent(func(ctx context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		<-gate
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})

	// Kick off a user prompt to occupy the runner.
	sink := &captureSink{}
	pdone := make(chan error, 1)
	go func() {
		pdone <- r.Prompt(context.Background(), "c", "u",
			[]Turn{{Role: "user", Content: "real"}}, Options{}, sink)
	}()
	// Wait for runner to be inFlight.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&agent.prompts) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}

	st := r.sessions["c"]
	// Manually fill the queue with reactions to sessionQueueCap.
	for i := 0; i < sessionQueueCap; i++ {
		req := &turnReq{
			kind:       turnReaction,
			ctx:        context.Background(),
			sink:       discardSink{convID: "c"},
			blocks:     []acp.ContentBlock{acp.TextBlock("r")},
			enqueuedAt: r.cfg.Now().Add(time.Duration(i) * time.Millisecond),
			done:       make(chan struct{}),
		}
		if !st.queue.push(req) {
			t.Fatalf("reaction %d: push failed", i)
		}
	}
	// Snapshot the oldest reaction's done before overflowing.
	st.queue.mu.Lock()
	oldest := st.queue.q[0]
	st.queue.mu.Unlock()

	// Push one more reaction → must shed `oldest`.
	overflow := &turnReq{
		kind:       turnReaction,
		ctx:        context.Background(),
		sink:       discardSink{convID: "c"},
		blocks:     []acp.ContentBlock{acp.TextBlock("rx")},
		enqueuedAt: r.cfg.Now().Add(time.Hour),
		done:       make(chan struct{}),
	}
	if !st.queue.push(overflow) {
		t.Fatalf("overflow push rejected")
	}
	select {
	case <-oldest.done:
	case <-time.After(time.Second):
		t.Fatalf("oldest reaction not shed")
	}
	if !oldest.shed {
		t.Fatalf("oldest.shed=false")
	}

	// Push a user prompt: queue is again full, no reactions older than overflow,
	// and incoming is a user — must be accepted (queue grows past cap).
	user2 := &turnReq{
		kind:       turnUser,
		ctx:        context.Background(),
		sink:       discardSink{convID: "c"},
		blocks:     []acp.ContentBlock{acp.TextBlock("user2")},
		enqueuedAt: r.cfg.Now(),
		done:       make(chan struct{}),
	}
	if !st.queue.push(user2) {
		t.Fatalf("user prompt should never be shed")
	}

	close(gate)
	<-pdone
}

// TestRouter_NewReactionDropped: queue full of user prompts, incoming
// reaction must be dropped (push returns false) and no user prompt
// touched.
func TestRouter_NewReactionDropped(t *testing.T) {
	gate := make(chan struct{})
	agent := newFakeAgent(func(ctx context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		<-gate
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})

	// Trigger session creation by running one prompt.
	go r.Prompt(context.Background(), "c", "u",
		[]Turn{{Role: "user", Content: "occupy"}}, Options{}, &captureSink{})
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&agent.prompts) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}

	st := r.sessions["c"]
	for i := 0; i < sessionQueueCap; i++ {
		req := &turnReq{
			kind:       turnUser,
			ctx:        context.Background(),
			sink:       discardSink{convID: "c"},
			blocks:     []acp.ContentBlock{acp.TextBlock("u")},
			enqueuedAt: r.cfg.Now(),
			done:       make(chan struct{}),
		}
		if !st.queue.push(req) {
			t.Fatalf("user push %d failed", i)
		}
	}
	reaction := &turnReq{
		kind:       turnReaction,
		ctx:        context.Background(),
		sink:       discardSink{convID: "c"},
		blocks:     []acp.ContentBlock{acp.TextBlock("r")},
		enqueuedAt: r.cfg.Now(),
		done:       make(chan struct{}),
	}
	if st.queue.push(reaction) {
		t.Fatalf("reaction should have been dropped (queue full of user prompts)")
	}
	close(gate)
}

// TestRouter_ReactionAgeDrop: a reaction enqueued long ago is dropped
// at dequeue time without ever reaching the agent.
func TestRouter_ReactionAgeDrop(t *testing.T) {
	gate := make(chan struct{})
	agent := newFakeAgent(func(ctx context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		<-gate
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})

	go r.Prompt(context.Background(), "c", "u",
		[]Turn{{Role: "user", Content: "occupy"}}, Options{}, &captureSink{})
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&agent.prompts) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}

	st := r.sessions["c"]
	// Stale reaction: enqueuedAt 1 hour ago.
	stale := &turnReq{
		kind:       turnReaction,
		ctx:        context.Background(),
		sink:       discardSink{convID: "c"},
		blocks:     []acp.ContentBlock{acp.TextBlock("r")},
		enqueuedAt: r.cfg.Now().Add(-time.Hour),
		done:       make(chan struct{}),
	}
	st.queue.push(stale)

	before := atomic.LoadInt32(&agent.prompts)
	close(gate) // releases the user prompt; runner then processes stale reaction
	select {
	case <-stale.done:
	case <-time.After(2 * time.Second):
		t.Fatalf("stale reaction not processed")
	}
	if !stale.shed {
		t.Fatalf("stale.shed=false; want true (age-drop)")
	}
	// Agent.Prompt called once for the user turn, not for the stale reaction.
	if got := atomic.LoadInt32(&agent.prompts); got != before {
		t.Fatalf("agent called %d extra times for stale reaction", got-before)
	}
}

// TestRouter_EndTurnAckBeforeSinkDone: a chunk emitted right before
// Agent.Prompt returns must land on the sink BEFORE sink.Done() is
// called. Repros the historical race where Done() could close the
// SSE while a chunk was still being processed by the drain.
func TestRouter_EndTurnAckBeforeSinkDone(t *testing.T) {
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		// Emit a chunk, then return immediately. The drain must
		// process this chunk before endTurn-ack unblocks the runner,
		// otherwise sink.Done would race with sink.Text.
		a.emit(sid, "final-chunk")
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})

	type event = orderingEvent
	events := make(chan orderingEvent, 8)
	sink := &orderingSink{events: events}
	if err := r.Prompt(context.Background(), "c", "u",
		[]Turn{{Role: "user", Content: "hi"}}, Options{}, sink); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	close(events)
	var seq []event
	for e := range events {
		seq = append(seq, e)
	}
	if len(seq) < 2 {
		t.Fatalf("expected text then done, got %v", seq)
	}
	if seq[len(seq)-1].kind != "done" {
		t.Fatalf("last event = %v, want done", seq[len(seq)-1])
	}
	sawText := false
	for _, e := range seq {
		if e.kind == "text" && e.text == "final-chunk" {
			sawText = true
		}
		if e.kind == "done" && !sawText {
			t.Fatalf("done before text chunk; sequence=%v", seq)
		}
	}
	if !sawText {
		t.Fatalf("final chunk never delivered; sequence=%v", seq)
	}
}

// orderingSink records the order of sink calls.
type orderingEvent struct{ kind, text string }
type orderingSink struct {
	events chan orderingEvent
}

func (s *orderingSink) FirstChunk() {}
func (s *orderingSink) Text(t string) error {
	s.send("text", t)
	return nil
}
func (s *orderingSink) Replace(t string) error { s.send("replace", t); return nil }
func (s *orderingSink) Error(t, _ string) error {
	s.send("error", t)
	return nil
}
func (s *orderingSink) Done() error { s.send("done", ""); return nil }
func (s *orderingSink) send(kind, text string) {
	s.events <- orderingEvent{kind, text}
}

// TestRouter_ReportReactionFireAndForget verifies ReportReaction
// returns immediately (no wait for agent) and the runner eventually
// runs the synthetic turn against the agent with the marker prefix.
func TestRouter_ReportReactionFireAndForget(t *testing.T) {
	gotPrompt := make(chan string, 1)
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, text string) (acp.StopReason, error) {
		gotPrompt <- text
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})

	start := time.Now()
	if err := r.ReportReaction(context.Background(), "c", "u", "msg-1", "👍", "added"); err != nil {
		t.Fatalf("ReportReaction: %v", err)
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("ReportReaction blocked for %s (should be fire-and-forget)", d)
	}
	select {
	case text := <-gotPrompt:
		if !strings.Contains(text, "[poe-acp:out-of-band reaction]") {
			t.Fatalf("agent got prompt %q lacking marker", text)
		}
		if !strings.Contains(text, "msg-1") || !strings.Contains(text, "👍") || !strings.Contains(text, "added") {
			t.Fatalf("agent prompt missing fields: %q", text)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("agent never received reaction turn")
	}
}

// TestSessionQueue_StopDrainsPending: stop() closes pending reqs'
// done channels with shed=true.
func TestSessionQueue_StopDrainsPending(t *testing.T) {
	sq := newSessionQueue()
	req := &turnReq{kind: turnReaction, done: make(chan struct{})}
	if !sq.push(req) {
		t.Fatal("push failed")
	}
	sq.stop()
	select {
	case <-req.done:
	case <-time.After(time.Second):
		t.Fatal("done not closed by stop")
	}
	if !req.shed {
		t.Fatal("shed not set")
	}
	// Push after stop returns false.
	req2 := &turnReq{kind: turnReaction, done: make(chan struct{})}
	if sq.push(req2) {
		t.Fatal("push on stopped queue should fail")
	}
}

// TestPrompt_SessionTornDown: when the session's queue is stopped
// between getOrCreate and push, Prompt returns an error and the sink
// gets Error+Done.
func TestPrompt_SessionTornDown(t *testing.T) {
	agent := newFakeAgent(func(context.Context, *fakeAgent, acp.SessionId, string) (acp.StopReason, error) {
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})

	// Pre-populate a session whose queue is already stopped.
	st := &sessionState{
		convID:    "torn",
		queue:     newSessionQueue(),
		runStop:   make(chan struct{}),
		drainStop: make(chan struct{}),
		chunkCh:   make(chan chunkMsg, 4),
	}
	st.queue.stop()
	close(st.runStop)
	close(st.drainStop)
	r.sessions["torn"] = st

	sink := &captureSink{}
	err := r.Prompt(context.Background(), "torn", "u",
		[]Turn{{Role: "user", Content: "hi"}}, Options{}, sink)
	if err == nil {
		t.Fatal("want error")
	}
	if !sink.done || sink.errText == "" {
		t.Fatalf("sink not finalised: %+v", sink)
	}
}

// TestReportReaction_DroppedWhenQueueFullOfUsers: ReportReaction
// returns nil (logs and drops) when push fails.
func TestReportReaction_DroppedWhenQueueFullOfUsers(t *testing.T) {
	gate := make(chan struct{})
	agent := newFakeAgent(func(ctx context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		<-gate
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})

	// Bootstrap session via a real prompt that we then leave blocked.
	go r.Prompt(context.Background(), "c", "u",
		[]Turn{{Role: "user", Content: "x"}}, Options{}, &captureSink{})
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&agent.prompts) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}

	st := r.sessions["c"]
	for i := 0; i < sessionQueueCap; i++ {
		req := &turnReq{kind: turnUser, ctx: context.Background(),
			sink: discardSink{convID: "c"}, blocks: []acp.ContentBlock{acp.TextBlock("u")},
			enqueuedAt: r.cfg.Now(), done: make(chan struct{})}
		st.queue.push(req)
	}
	if err := r.ReportReaction(context.Background(), "c", "u", "m", "👍", "added"); err != nil {
		t.Fatalf("ReportReaction returned err on full queue: %v", err)
	}
	close(gate)
}

// TestReactionRunner_EmitsAllSinkPaths: a reaction turn whose agent
// emits chunks, replace_response and an error covers the discardSink
// branches and the chunk drain path under turnReaction.
func TestReactionRunner_EmitsAllSinkPaths(t *testing.T) {
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ack")
		return acp.StopReasonCancelled, nil // triggers sink.Replace("_(cancelled)_")
	})
	r, _ := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err := r.ReportReaction(context.Background(), "c", "u", "m", "👍", "added"); err != nil {
		t.Fatal(err)
	}
	// Allow runner to complete.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&agent.prompts) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	// Run a second reaction whose agent errors → sink.Error path.
	agent2 := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		return acp.StopReasonRefusal, nil // triggers sink.Error
	})
	r2, _ := New(Config{Agent: agent2, StateDir: t.TempDir(), SessionTTL: time.Hour})
	_ = r2.ReportReaction(context.Background(), "c", "u", "m", "👍", "added")
	deadline = time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&agent2.prompts) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	// And one whose Agent.Prompt errors outright → sink.Error via err path.
	agent3 := newFakeAgent(func(_ context.Context, _ *fakeAgent, _ acp.SessionId, _ string) (acp.StopReason, error) {
		return acp.StopReason(""), errFakePromptFail
	})
	r3, _ := New(Config{Agent: agent3, StateDir: t.TempDir(), SessionTTL: time.Hour})
	_ = r3.ReportReaction(context.Background(), "c", "u", "m", "👍", "added")
	deadline = time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&agent3.prompts) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	// Drain runner so discardSink.Done / Error close out. Slight pause.
	time.Sleep(50 * time.Millisecond)
}

var errFakePromptFail = fakeErr("agent boom")

type fakeErr string

func (e fakeErr) Error() string { return string(e) }

// TestReportReaction_DefaultActionAdded: empty action defaults to "added".
func TestReportReaction_DefaultActionAdded(t *testing.T) {
	got := make(chan string, 1)
	agent := newFakeAgent(func(_ context.Context, _ *fakeAgent, _ acp.SessionId, text string) (acp.StopReason, error) {
		got <- text
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err := r.ReportReaction(context.Background(), "", "u", "m", "👍", ""); err != nil {
		t.Fatal(err)
	}
	select {
	case text := <-got:
		if !strings.Contains(text, "(added)") {
			t.Fatalf("default action not 'added': %q", text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
	}
}

// TestReportReaction_GetOrCreateError: getOrCreate fails (NewSession
// error) → ReportReaction returns wrapped error.
func TestReportReaction_GetOrCreateError(t *testing.T) {
	agent := newFakeAgent(func(context.Context, *fakeAgent, acp.SessionId, string) (acp.StopReason, error) {
		return acp.StopReasonEndTurn, nil
	})
	agent.newSessErr = errFakePromptFail
	r, _ := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err := r.ReportReaction(context.Background(), "c", "u", "m", "👍", "added"); err == nil {
		t.Fatal("want error")
	}
}

// TestPrompt_CtxCancelReturnsCtxErr: caller ctx fires, agent finishes
// without error → Prompt returns ctx.Err().
func TestPrompt_CtxCancelReturnsCtxErr(t *testing.T) {
	release := make(chan struct{})
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		<-release
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	d := make(chan error, 1)
	go func() {
		d <- r.Prompt(ctx, "c", "u", []Turn{{Role: "user", Content: "hi"}}, Options{}, &captureSink{})
	}()
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&agent.prompts) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	close(release)
	err := <-d
	if err == nil {
		t.Fatal("want ctx err")
	}
}

// TestPrompt_CtxCancelPlusRunnerErr: ctx fires AND the runner sets
// req.err — Prompt must return req.err (not ctx.Err()).
func TestPrompt_CtxCancelPlusRunnerErr(t *testing.T) {
	release := make(chan struct{})
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		<-release
		return acp.StopReason(""), errFakePromptFail
	})
	r, _ := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	d := make(chan error, 1)
	go func() {
		d <- r.Prompt(ctx, "c", "u", []Turn{{Role: "user", Content: "hi"}}, Options{}, &captureSink{})
	}()
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&agent.prompts) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	close(release)
	err := <-d
	if err == nil || err.Error() == context.Canceled.Error() {
		t.Fatalf("want runner err (not ctx.Err), got %v", err)
	}
}

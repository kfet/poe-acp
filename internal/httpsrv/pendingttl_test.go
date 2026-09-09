package httpsrv

// The pending marker's bound is NOT AnswerTTL.
//
// AnswerTTL bounds how long a COMPLETED answer waits to be redriven. A
// marker bounds how long a WAITER should believe the owner is alive —
// which is IdleWriteTimeout, the same "maximum tolerable silence" the
// owner uses to declare its own turn wedged. Turns legitimately run for
// tens of minutes (hence the 30m swap-drain deadline), so the owner
// republishes its marker from watchTurn for as long as the turn is
// demonstrably alive. A wedged or killed owner stops refreshing and the
// marker expires, releasing the waiter instead of pinning it.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kfet/acp-kit/client"
	"github.com/kfet/poe-acp/internal/router"
)

// newGenOpts is newGen with an explicit AnswerTTL / IdleWriteTimeout, so
// the two bounds can be driven apart.
func newGenOpts(t *testing.T, stateDir string, a router.Agent, answerTTL, idleTO time.Duration) *Handler {
	t.Helper()
	rtr, err := router.New(router.Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{
		Router: rtr, HeartbeatInterval: 0, StateDir: stateDir,
		AnswerTTL: answerTTL, IdleWriteTimeout: idleTO,
	})
	h.answers.pollInterval = time.Millisecond
	return h
}

// The bound must come from the DEFAULTED IdleWriteTimeout. A zero
// pendingTTL would make every marker instantly dead and silently disable
// the cross-generation wait with nothing in the log to say so — the
// nastiest possible failure mode for this feature.
func TestPending_ZeroConfigStillYieldsALiveMarker(t *testing.T) {
	h := newGen(t, t.TempDir(), &fakeAgent{})
	if got := h.answers.store.pendingTTL; got != defaultIdleWriteTimeout {
		t.Fatalf("pendingTTL=%s want the defaulted IdleWriteTimeout %s", got, defaultIdleWriteTimeout)
	}
	if h.answers.store.pendingTTL == h.answers.store.ttl && defaultAnswerTTL != defaultIdleWriteTimeout {
		t.Fatal("pendingTTL is still coupled to AnswerTTL")
	}
	key := answerKey("c1", "m1")
	h.answers.markPending(key)
	if !h.answers.store.pendingLive(key) {
		t.Fatal("a freshly marked key is not live under a zero-value Config")
	}
}

// Re-marking is the refresh: it rewrites the marker and so extends it by
// another pendingTTL. It also recreates a marker that was swept or
// removed out from under the owner.
func TestPending_RemarkRefreshesAndRecreates(t *testing.T) {
	s := newAnswerStore(filepath.Join(t.TempDir(), "absorbed"), time.Minute, time.Minute)
	s.markPending("k")
	backdate(t, s.path("k", sufPending), 2*time.Minute)
	if s.pendingLive("k") {
		t.Fatal("backdated marker still live")
	}
	s.markPending("k")
	if !s.pendingLive("k") {
		t.Fatal("re-marking did not refresh the marker")
	}

	if err := os.Remove(s.path("k", sufPending)); err != nil {
		t.Fatal(err)
	}
	s.markPending("k")
	if !s.pendingLive("k") {
		t.Fatal("re-marking did not recreate a vanished marker")
	}
}

// The sweep applies the right TTL to the right suffix, in both
// directions — a marker must not be swept on the answer's clock, nor an
// answer on the marker's.
func TestPending_SweepIsSuffixAware(t *testing.T) {
	// Long-lived markers, short-lived answers.
	s := newAnswerStore(filepath.Join(t.TempDir(), "a"), time.Minute, time.Hour)
	s.markPending("keep")
	s.put("drop", []recCall{{op: opText, s1: "x"}})
	backdate(t, s.path("keep", sufPending), 30*time.Minute)
	backdate(t, s.path("drop", sufAnswer), 30*time.Minute)
	s.sweep()
	if !s.pendingLive("keep") {
		t.Fatal("marker swept on the answer's TTL")
	}
	if _, ok := s.claim("drop"); ok {
		t.Fatal("expired answer survived the sweep")
	}

	// And the reverse: long-lived answers, short-lived markers.
	r := newAnswerStore(filepath.Join(t.TempDir(), "b"), time.Hour, time.Minute)
	r.markPending("drop")
	r.put("keep", []recCall{{op: opText, s1: "x"}})
	backdate(t, r.path("drop", sufPending), 30*time.Minute)
	backdate(t, r.path("keep", sufAnswer), 30*time.Minute)
	r.sweep()
	if r.pendingLive("drop") {
		t.Fatal("expired marker survived the sweep")
	}
	if _, ok := r.claim("keep"); !ok {
		t.Fatal("answer swept on the marker's TTL")
	}
}

// THE FIX: a turn that outlives pendingTTL keeps its marker live, because
// watchTurn republishes it while the turn is alive — and the redrive on
// another generation is therefore still served instead of re-running.
func TestPending_LongTurnKeepsMarkerLiveAndRedriveIsServed(t *testing.T) {
	stateDir := t.TempDir()
	// pendingTTL = 2s, so watchTurn ticks every 500ms. The turn stays
	// well inside the wedge window for the length of this test.
	agentA := &absorbAgent{fakeAgent: &fakeAgent{}, entered: make(chan struct{}), release: make(chan struct{})}
	agentB := &absorbAgent{fakeAgent: &fakeAgent{}, entered: make(chan struct{}), release: make(chan struct{})}
	close(agentB.release) // an unexpected re-run must FAIL, not deadlock
	genA := newGenOpts(t, stateDir, agentA, time.Minute, 2*time.Second)
	genB := newGenOpts(t, stateDir, agentB, time.Minute, 2*time.Second)

	ticked := make(chan struct{}, 8)
	genA.idleTickHook = func() {
		select {
		case ticked <- struct{}{}:
		default:
		}
	}
	// If a loaded box delays the first tick past the 2s idle window the
	// backstop cuts the turn and `ticked` never fires. Watch for that so
	// it fails fast and by name instead of hanging to the test timeout.
	cut := make(chan struct{})
	genA.idleWriteCancelHook = func() { close(cut) }

	key := answerKey("c1", "m1")
	body := swapBody("m1")
	turnA := absorbOn(t, genA, agentA, body)

	// Age the marker past pendingTTL: without a refresh it is now dead
	// and a redrive would fall through to a re-run.
	backdate(t, genA.answers.store.path(key, sufPending), time.Hour)
	if genB.answers.store.pendingLive(key) {
		t.Fatal("backdated marker still live — test cannot prove the refresh")
	}

	// The hook fires on EVERY tick and `ticked` is buffered, so a signal
	// enqueued before the backdate proves nothing — and within one tick
	// the re-mark precedes the enqueue, so even a fresh signal's refresh
	// can predate the backdate. Wait for the observable state instead,
	// bounded by the idle backstop. Every subsequent tick re-marks while
	// the turn is absorbed and alive, so this converges.
	for !genB.answers.store.pendingLive(key) {
		select {
		case <-ticked:
		case <-cut:
			t.Fatal("idle backstop cut the turn before a refresh tick — box too slow for this timing")
		}
	}

	// And the redrive on the other generation is served, not re-run.
	// Release A first: the marker's survival is what this test is about,
	// and the WAIT path has its own test. Leaving A blocked while B
	// waited would put the rest of the test inside A's 2s idle window —
	// if the backstop cut A there, A would never publish, the marker
	// would expire, and B would silently re-run.
	close(agentA.release)
	<-turnA

	recB := httptest.NewRecorder()
	genB.ServeHTTP(recB, httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body)))

	if !strings.Contains(recB.Body.String(), "the answer") {
		t.Fatalf("long turn's redrive was not served: %q", recB.Body.String())
	}
	if got := atomic.LoadInt32(&agentB.promptCalls); got != 0 {
		t.Fatalf("long turn's redrive re-ran the agent: prompt calls=%d", got)
	}
}

// The other side of the same coin: an owner that died without publishing
// stops refreshing, its marker expires, and the redrive re-runs rather
// than waiting forever.
func TestPending_DeadOwnersMarkerExpiresAndRedriveReruns(t *testing.T) {
	stateDir := t.TempDir()
	a := &absorbAgent{fakeAgent: &fakeAgent{}, entered: make(chan struct{}), release: make(chan struct{})}
	close(a.release) // this generation answers immediately
	h := newGen(t, stateDir, a)

	// A marker left behind by a worker that was killed mid-turn.
	key := answerKey("c1", "m1")
	h.answers.markPending(key)
	backdate(t, h.answers.store.path(key, sufPending), 2*defaultIdleWriteTimeout)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(swapBody("m1"))))
	if !strings.Contains(rec.Body.String(), "the answer") {
		t.Fatalf("redrive behind a dead owner did not answer: %q", rec.Body.String())
	}
	if got := atomic.LoadInt32(&a.promptCalls); got != 1 {
		t.Fatalf("redrive behind a dead owner must re-run: prompt calls=%d", got)
	}
}

// A normally-connected turn never publishes a marker, so an unrelated
// live turn can never keep a dead worker's marker alive.
func TestPending_ConnectedTurnPublishesNoMarker(t *testing.T) {
	stateDir := t.TempDir()
	h := newGen(t, stateDir, &fakeAgent{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(swapBody("m1"))))

	ents, err := os.ReadDir(h.answers.store.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), sufPending) {
			t.Fatalf("a connected turn published a pending marker: %s", e.Name())
		}
	}
}

// A WEDGED absorbed turn stops refreshing, gets cut by the idle backstop,
// and publishes whatever the recorder holds — the router's error card.
// The waiter is therefore released with that card rather than hanging or
// silently re-running into the same wedged session. Take-once means the
// user's NEXT retry runs cleanly.
func TestPending_WedgedAbsorbedTurnReleasesWaiterWithItsErrorCard(t *testing.T) {
	stateDir := t.TempDir()
	a := &wedgeAgent{fakeAgent: &fakeAgent{}, returned: make(chan struct{})}
	genA := newGenOpts(t, stateDir, a, time.Minute, 40*time.Millisecond)
	genB := newGenOpts(t, stateDir, &fakeAgent{}, time.Minute, time.Minute)

	cut := make(chan struct{})
	genA.idleWriteCancelHook = func() { close(cut) }
	decided := make(chan struct{})
	genA.absorbDecidedHook = func() { close(decided) }

	body := swapBody("m1")
	ctx, cancel := context.WithCancel(context.Background())
	turnA := make(chan struct{})
	go func() {
		defer close(turnA)
		req := httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body)).WithContext(ctx)
		genA.ServeHTTP(httptest.NewRecorder(), req)
	}()
	cancel() // pre-output transport drop: the turn is absorbed
	<-decided
	<-cut // ... then the idle backstop cuts it, wedged and unrefreshed
	<-a.returned
	<-turnA

	key := answerKey("c1", "m1")
	if genB.answers.store.pendingLive(key) {
		t.Fatal("a cut turn left its pending marker behind")
	}
	calls, out := genB.answers.waitPending(context.Background(), key)
	if out != waitMiss {
		t.Fatalf("outcome=%v want waitMiss (marker already cleared)", out)
	}
	if calls != nil {
		t.Fatal("waitMiss returned calls")
	}
	// The cut turn's card is on disk for the redrive, claimable once.
	if _, ok := genB.answers.take(key); !ok {
		t.Fatal("cut turn published nothing for the redrive")
	}
	if _, ok := genB.answers.take(key); ok {
		t.Fatal("cut turn's card was served twice")
	}
}

// A redrive whose OWN client goes away mid-wait must not start a
// duplicate of the turn already running elsewhere — that would be a
// second agent invocation, with real side effects, for work in flight.
func TestPending_AbortedWaitDoesNotDuplicateTheTurn(t *testing.T) {
	stateDir := t.TempDir()
	agentB := &absorbAgent{fakeAgent: &fakeAgent{}, entered: make(chan struct{}), release: make(chan struct{})}
	close(agentB.release)
	genB := newGen(t, stateDir, agentB)

	key := answerKey("c1", "m1")
	genB.answers.markPending(key) // an owner elsewhere holds this turn

	ctx, cancel := context.WithCancel(context.Background())
	// Kill this redrive's client from inside the wait loop, so the abort
	// arm is driven deterministically rather than racing the loop entry.
	genB.answers.waitTickHook = cancel

	req := httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(swapBody("m1"))).WithContext(ctx)
	genB.ServeHTTP(httptest.NewRecorder(), req)

	if got := atomic.LoadInt32(&agentB.promptCalls); got != 0 {
		t.Fatalf("aborted wait duplicated a turn owned elsewhere: prompt calls=%d", got)
	}
	if !genB.answers.store.pendingLive(key) {
		t.Fatal("aborted wait disturbed the owner's marker")
	}
}

// Narrowness guard for the above: an already-dead client with NO owner
// must still run and buffer its turn. That is the original absorb
// feature — a blanket "client gone, return early" would regress it.
func TestPending_DeadClientWithNoOwnerStillRunsAndBuffers(t *testing.T) {
	stateDir := t.TempDir()
	a := &absorbAgent{fakeAgent: &fakeAgent{}, entered: make(chan struct{}), release: make(chan struct{})}
	h := newGen(t, stateDir, a)

	// Gate the agent's output on the latch, like every other absorb test:
	// pre-releasing it races the router round-trip against the watcher,
	// and a win for the router flips the decision to CancelTurn.
	decided := make(chan struct{})
	h.absorbDecidedHook = func() { close(decided) }

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(swapBody("m1"))).WithContext(ctx)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-decided
	close(a.release)
	<-done

	if got := atomic.LoadInt32(&a.promptCalls); got != 1 {
		t.Fatalf("a dead client with no owner must still run the turn: prompt calls=%d", got)
	}
	if _, ok := h.answers.take(answerKey("c1", "m1")); !ok {
		t.Fatal("absorbed turn was not buffered for the redrive")
	}
}

// TestWatchTurn_CutStopsTheMarkerRefresh pins the half of the
// pending-marker contract that the wedge cut owns: an absorbed turn
// republishes its marker on every tick, but the instant the liveness
// watcher cuts the turn the loop returns, so a wedged owner stops
// refreshing and its marker is allowed to expire — releasing any redrive
// waiting behind it instead of pinning it forever.
//
// Before Stage 2 this was implicit in watchIdle's control flow (the cut
// branch returned). It is now a return on turnCtx.Done(), which reacts
// AT the cut rather than up to a quarter-window later, so it is worth
// pinning directly.
func TestWatchTurn_CutStopsTheMarkerRefresh(t *testing.T) {
	h := newGenOpts(t, t.TempDir(), &fakeAgent{}, time.Minute, 40*time.Millisecond)

	ticked := make(chan struct{}, 64)
	h.idleTickHook = func() {
		select {
		case ticked <- struct{}{}:
		default:
		}
	}
	cut := make(chan struct{})
	h.idleWriteCancelHook = func() { close(cut) }

	// A turn with no progress at all: the watcher cuts it after the
	// window, exactly as a wedged agent would be cut.
	_, turnCtx, stop := client.StartTurnLiveness(context.Background(),
		client.TurnLivenessConfig{
			NoProgressTimeout: 40 * time.Millisecond,
			NoProgressCause:   noProgressCause{window: 40 * time.Millisecond},
		})
	defer stop()

	key := answerKey("c1", "m1")
	h.answers.markPending(key)
	var absorbed atomic.Bool
	absorbed.Store(true)

	done := make(chan struct{})
	defer close(done)
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		h.watchTurn(turnCtx, done, "c1", key, &absorbed)
	}()

	select {
	case <-cut:
	case <-time.After(3 * time.Second):
		t.Fatal("the liveness watcher never cut a turn with no progress")
	}
	select {
	case <-returned:
	case <-time.After(3 * time.Second):
		t.Fatal("watchTurn kept refreshing the marker after the turn was cut")
	}
	// The loop was demonstrably refreshing before the cut — otherwise
	// "it stopped" would be vacuous.
	select {
	case <-ticked:
	default:
		t.Fatal("the marker was never refreshed at all, so the test proves nothing about it stopping")
	}
}

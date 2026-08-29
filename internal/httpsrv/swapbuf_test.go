package httpsrv

// Cross-worker-generation absorbed-answer survival.
//
// Reproduces the kopione 2026-08-28 incident shape: a SIGHUP swap retires
// the worker holding an absorbed turn, and the redrive lands on a
// different worker generation. Each `*Handler` here IS a worker
// generation — separate process memory, same on-disk state dir.

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

	"github.com/kfet/poe-acp/internal/router"
)

// newGen builds one worker generation: its own Handler (its own answer
// map) over the shared on-disk state dir, plus its own router/agent.
// Durations take their production defaults; see newGenOpts to drive the
// answer and marker bounds apart.
func newGen(t *testing.T, stateDir string, a router.Agent) *Handler {
	t.Helper()
	return newGenOpts(t, stateDir, a, 0, 0)
}

func swapBody(msgID string) []byte {
	return mustJSON(map[string]any{
		"type": "query", "conversation_id": "c1", "user_id": "u", "message_id": "req",
		"query": []map[string]any{{"role": "user", "content": "hi", "message_id": msgID}},
	})
}

// absorbOn runs a query on gen h, drops the client pre-output (so the
// turn is absorbed), and returns once the absorb decision is latched. The
// returned channel closes when the decoupled turn has finished and
// published (or abandoned) its answer.
func absorbOn(t *testing.T, h *Handler, a *absorbAgent, body []byte) <-chan struct{} {
	t.Helper()
	decided := make(chan struct{})
	h.absorbDecidedHook = func() { close(decided) }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body)).WithContext(ctx)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-a.entered
	cancel()
	<-decided
	return done
}

// THE INCIDENT: generation A absorbs a pre-output drop; the turn is STILL
// RUNNING when generation B receives the redrive. B must block on the
// pending marker, then serve A's answer — with zero prompts of its own.
func TestSwap_RedriveWaitsForInFlightTurnOnRetiringGeneration(t *testing.T) {
	stateDir := t.TempDir()
	agentA := &absorbAgent{fakeAgent: &fakeAgent{}, entered: make(chan struct{}), release: make(chan struct{})}
	agentB := &absorbAgent{fakeAgent: &fakeAgent{}, entered: make(chan struct{}), release: make(chan struct{})}
	// Pre-released: gen B must never be prompted, and if the fix ever
	// regresses we want a clear assertion failure, not a deadlock on an
	// agent that blocks forever.
	close(agentB.release)
	genA := newGen(t, stateDir, agentA)
	genB := newGen(t, stateDir, agentB)

	body := swapBody("m1")
	turnA := absorbOn(t, genA, agentA, body)

	// Generation B takes the redrive while A's turn is still blocked.
	recB := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		defer close(served)
		genB.ServeHTTP(recB, httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body)))
	}()

	// B must NOT have answered yet: it is waiting on A's pending marker.
	select {
	case <-served:
		t.Fatal("gen B answered before the in-flight absorbed turn published")
	case <-time.After(50 * time.Millisecond):
	}

	close(agentA.release) // A's turn completes and publishes
	<-turnA
	<-served

	out := recB.Body.String()
	if !strings.Contains(out, "the answer") {
		t.Fatalf("gen B did not serve gen A's absorbed answer: %q", out)
	}
	if !strings.Contains(out, "event: done") {
		t.Fatalf("gen B response missing done event: %q", out)
	}
	if got := atomic.LoadInt32(&agentB.promptCalls); got != 0 {
		t.Fatalf("gen B must not re-prompt the router: prompt calls=%d", got)
	}
	if got := atomic.LoadInt32(&agentA.promptCalls); got != 1 {
		t.Fatalf("gen A prompt calls=%d want 1", got)
	}
	// Take-once: a second redrive on either generation finds nothing.
	if _, ok := genB.answers.take(answerKey("c1", "m1")); ok {
		t.Fatal("answer served twice")
	}
}

// The easy sibling: A's turn has already COMPLETED and published before
// the redrive lands on B. B serves it straight from disk, no wait.
func TestSwap_RedriveServesCompletedAnswerFromOtherGeneration(t *testing.T) {
	stateDir := t.TempDir()
	agentA := &absorbAgent{fakeAgent: &fakeAgent{}, entered: make(chan struct{}), release: make(chan struct{})}
	agentB := &absorbAgent{fakeAgent: &fakeAgent{}, entered: make(chan struct{}), release: make(chan struct{})}
	// Pre-released: gen B must never be prompted, and if the fix ever
	// regresses we want a clear assertion failure, not a deadlock on an
	// agent that blocks forever.
	close(agentB.release)
	genA := newGen(t, stateDir, agentA)
	genB := newGen(t, stateDir, agentB)

	body := swapBody("m1")
	turnA := absorbOn(t, genA, agentA, body)
	close(agentA.release)
	<-turnA

	recB := httptest.NewRecorder()
	genB.ServeHTTP(recB, httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body)))
	if !strings.Contains(recB.Body.String(), "the answer") {
		t.Fatalf("gen B did not serve the completed answer: %q", recB.Body.String())
	}
	if got := atomic.LoadInt32(&agentB.promptCalls); got != 0 {
		t.Fatalf("gen B must not re-prompt: prompt calls=%d", got)
	}
	// Pending marker must be gone once the answer landed.
	if genB.answers.store.pendingLive(answerKey("c1", "m1")) {
		t.Fatal("pending marker outlived the published answer")
	}
}

// The pending marker's lifecycle: published at the absorb latch (so a
// mid-flight redrive on ANY generation can see it), gone once the answer
// is published.
func TestSwap_PendingMarkerLifecycle(t *testing.T) {
	stateDir := t.TempDir()
	a := &absorbAgent{fakeAgent: &fakeAgent{}, entered: make(chan struct{}), release: make(chan struct{})}
	genA := newGen(t, stateDir, a)
	genB := newGen(t, stateDir, &fakeAgent{})

	key := answerKey("c1", "m1")
	turnA := absorbOn(t, genA, a, swapBody("m1"))
	// Visible to the OTHER generation while the turn is still running.
	if !genB.answers.store.pendingLive(key) {
		t.Fatal("absorb latch did not publish a pending marker visible cross-generation")
	}
	close(a.release)
	<-turnA
	if genB.answers.store.pendingLive(key) {
		t.Fatal("pending marker outlived the published answer")
	}
}

// A redrive whose own client drops while it is waiting must give up
// promptly rather than hold the wait open.
func TestSwap_WaitAbortsOnRedriveClientDisconnect(t *testing.T) {
	stateDir := t.TempDir()
	h := newGen(t, stateDir, &fakeAgent{})
	key := answerKey("c1", "m1")
	h.answers.markPending(key)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, out := h.answers.waitPending(ctx, key); out != waitAborted {
		t.Fatalf("waitPending outcome=%v want waitAborted", out)
	}
}

// A pending marker whose owner died without publishing TTL-expires, and
// the waiter falls through to a re-run.
func TestSwap_ExpiredPendingMarkerFallsThroughToRerun(t *testing.T) {
	stateDir := t.TempDir()
	h := newGen(t, stateDir, &fakeAgent{})
	key := answerKey("c1", "m1")
	h.answers.markPending(key)
	backdate(t, h.answers.store.path(key, sufPending), 2*defaultIdleWriteTimeout)

	if _, out := h.answers.waitPending(context.Background(), key); out != waitMiss {
		t.Fatalf("expired pending marker: outcome=%v want waitMiss", out)
	}
}

// A pending marker cleared mid-wait (the owner abandoned) releases the
// waiter through the in-loop pendingLive check.
func TestSwap_PendingClearedMidWaitReleasesWaiter(t *testing.T) {
	stateDir := t.TempDir()
	h := newGen(t, stateDir, &fakeAgent{})
	key := answerKey("c1", "m1")
	h.answers.markPending(key)

	// Clear the marker from inside the loop's own tick, so the "owner
	// abandoned mid-wait" branch is driven deterministically rather than
	// racing waitPending's entry check.
	h.answers.waitTickHook = func() { h.answers.store.abandon(key) }
	if _, out := h.answers.waitPending(context.Background(), key); out != waitMiss {
		t.Fatalf("cleared marker: outcome=%v want waitMiss", out)
	}
}

// backdate rewinds path's mtime by d, driving TTL expiry without sleeping.
func backdate(t *testing.T, path string, d time.Duration) {
	t.Helper()
	old := time.Now().Add(-d)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

// A stale answer file on disk is never served; it is discarded on claim.
func TestSwap_ExpiredDiskAnswerNotServed(t *testing.T) {
	s := newAnswerStore(filepath.Join(t.TempDir(), "absorbed"), time.Minute, time.Minute)
	s.put("k", []recCall{{op: opText, s1: "stale"}})
	backdate(t, s.path("k", sufAnswer), 2*time.Minute)
	if _, ok := s.claim("k"); ok {
		t.Fatal("expired answer was served")
	}
}

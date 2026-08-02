//go:build unix

package supervisor

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
)

// wedgedServer is a Shutdowner whose Shutdown never returns until the
// context expires — exactly what *http.Server does while a handler is
// stuck. It records whether Close was called.
type wedgedServer struct {
	mu     sync.Mutex
	closed bool
}

func (s *wedgedServer) Shutdown(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (s *wedgedServer) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}

func (s *wedgedServer) wasClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// quickServer drains instantly, modelling every in-flight stream
// completing well inside the deadline.
type quickServer struct{ closed bool }

func (s *quickServer) Shutdown(context.Context) error { return nil }
func (s *quickServer) Close() error                   { s.closed = true; return nil }

// TestDrainServer_Graceful: below the deadline the drain is exactly the
// old behaviour — Shutdown returns, no force-close, no onForce log.
func TestDrainServer_Graceful(t *testing.T) {
	srv := &quickServer{}
	forced := false
	got, err := DrainServer(srv, time.Second, func() { forced = true })
	if got != DrainGraceful || err != nil {
		t.Fatalf("got (%v,%v) want (graceful,nil)", got, err)
	}
	if forced || srv.closed {
		t.Fatal("a graceful drain must neither report nor force-close")
	}
}

// TestDrainServer_ForcedOnDeadline is the core regression: a server that
// never finishes draining (the wedged worker from the 2026-07-26 incident)
// must NOT block forever. On the old code this was
// srv.Shutdown(context.Background()) and hung until systemd SIGKILLed the
// control group.
func TestDrainServer_ForcedOnDeadline(t *testing.T) {
	srv := &wedgedServer{}
	forced := make(chan struct{})
	done := make(chan DrainResult, 1)
	go func() {
		res, _ := DrainServer(srv, 100*time.Millisecond, func() { close(forced) })
		done <- res
	}()

	select {
	case res := <-done:
		if res != DrainForced {
			t.Fatalf("got %v want DrainForced", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DrainServer never returned — the drain is still unbounded")
	}
	select {
	case <-forced:
	case <-time.After(time.Second):
		t.Fatal("onForce was not invoked, so abandoned streams would go unlogged")
	}
	if !srv.wasClosed() {
		t.Fatal("a forced drain must force-close the server")
	}
}

// TestDrainServer_NilHookAndDefaultDeadline covers the nil-onForce and
// deadline<=0 branches (<=0 means "use the default", never "wait forever").
func TestDrainServer_NilHookAndDefaultDeadline(t *testing.T) {
	srv := &quickServer{}
	if got, err := DrainServer(srv, 0, nil); got != DrainGraceful || err != nil {
		t.Fatalf("got (%v,%v) want (graceful,nil)", got, err)
	}
}

// errServer drains fine but reports a listener-close error — that is a
// completed drain, not a force-cut, and must not be logged as one.
type errServer struct{ closed bool }

func (s *errServer) Shutdown(context.Context) error { return errors.New("listener close failed") }
func (s *errServer) Close() error                   { s.closed = true; return nil }

func TestDrainServer_NonDeadlineErrorIsStillGraceful(t *testing.T) {
	srv := &errServer{}
	forced := false
	got, err := DrainServer(srv, time.Second, func() { forced = true })
	if got != DrainGraceful || err == nil {
		t.Fatalf("got (%v,%v) want (graceful,err)", got, err)
	}
	if forced || srv.closed {
		t.Fatal("a completed drain must not force-close or report abandoned streams")
	}
}

// TestDrainServer_RealHTTPServerWedgedHandler proves the bound against a
// REAL http.Server with a handler that never returns (a stream the
// idle-write backstop cannot clear): Shutdown blocks on it, the deadline
// fires, Close tears the connection down, and the worker gets to exit.
func TestDrainServer_RealHTTPServerWedgedHandler(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	srv := &http.Server{
		ReadHeaderTimeout: time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			close(entered)
			<-release // wedged: never returns on its own
		}),
	}
	go func() { _ = srv.Serve(ln) }()

	reqDone := make(chan struct{})
	go func() {
		defer close(reqDone)
		resp, err := http.Get("http://" + ln.Addr().String() + "/wedge")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never ran")
	}

	res := make(chan DrainResult, 1)
	go func() {
		got, _ := DrainServer(srv, 150*time.Millisecond, nil)
		res <- got
	}()
	select {
	case got := <-res:
		if got != DrainForced {
			t.Fatalf("got %v want DrainForced", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("DrainServer wedged behind a stuck handler")
	}
	<-reqDone
}

// ---- ForceExitAfter ----

func TestForceExitAfter_Fires(t *testing.T) {
	fired := make(chan struct{})
	ForceExitAfter(time.Millisecond, func() { close(fired) })
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("force exit never fired")
	}
}

func TestForceExitAfter_Stopped(t *testing.T) {
	fired := make(chan struct{}, 1)
	stop := ForceExitAfter(time.Hour, func() { fired <- struct{}{} })
	stop()
	select {
	case <-fired:
		t.Fatal("stopped timer must not fire")
	case <-time.After(20 * time.Millisecond):
	}
}

// ---- Retire ----

// sigRecorder replaces the process-signal seam so no real signal is ever
// delivered, recording what would have been sent and publishing each on a
// channel so tests can synchronise without polling.
type sigRecorder struct {
	mu   sync.Mutex
	sent []os.Signal
	ch   chan os.Signal
}

// stubSignals installs the seam. errFn, when non-nil, decides the error
// returned for a given signal.
func stubSignals(t *testing.T, errFn func(sig os.Signal) error) *sigRecorder {
	t.Helper()
	r := &sigRecorder{ch: make(chan os.Signal, 8)}
	old := signalProc
	signalProc = func(_ *os.Process, sig os.Signal) error {
		r.mu.Lock()
		r.sent = append(r.sent, sig)
		r.mu.Unlock()
		var err error
		if errFn != nil {
			err = errFn(sig)
		}
		r.ch <- sig
		return err
	}
	t.Cleanup(func() { signalProc = old })
	return r
}

func (r *sigRecorder) list() []os.Signal {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]os.Signal(nil), r.sent...)
}

// fireAfter is an `after` seam that fires immediately on every call, so
// both the escalation deadline and the post-kill reap wait expire at once.
func fireAfter(time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	ch <- time.Now()
	return ch
}

// fireOnce returns an `after` seam that expires immediately the FIRST
// time (the escalation deadline) and never again (the post-kill reap
// wait), so an escalation test resolves deterministically on the worker's
// death rather than racing a second timer.
func fireOnce() func(time.Duration) <-chan time.Time {
	var once sync.Once
	return func(time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		once.Do(func() { ch <- time.Now() })
		return ch
	}
}

func TestRetire_Graceful(t *testing.T) {
	s := newSup(t)
	rec := stubSignals(t, nil)
	dead := make(chan struct{})
	close(dead)
	res, err := s.Retire(&os.Process{Pid: 4242}, dead, DrainOrder{Deadline: time.Minute}, nil)
	if err != nil || res != RetireGraceful {
		t.Fatalf("got (%v,%v) want (graceful,nil)", res, err)
	}
	if sent := rec.list(); len(sent) != 1 || sent[0] != syscall.SIGTERM {
		t.Fatalf("want a single SIGTERM, got %v", sent)
	}
	if len(s.Retiring()) != 0 {
		t.Fatal("a completed retirement must not stay tracked")
	}
}

// TestRetire_EscalatesToSIGKILL is the supervisor-side half of the fix: a
// worker that ignores SIGTERM past the deadline gets SIGKILLed instead of
// being waited on forever.
func TestRetire_EscalatesToSIGKILL(t *testing.T) {
	s := newSup(t)
	term := stubSignals(t, nil)
	killed := stubKill(t, nil)
	dead := make(chan struct{})
	// The worker "exits" only once the group SIGKILL has been delivered.
	go func() { <-killed.ch; close(dead) }()
	res, err := s.Retire(&os.Process{Pid: 4242}, dead, DrainOrder{Deadline: time.Millisecond}, fireOnce())
	if err != nil || res != RetireKilled {
		t.Fatalf("got (%v,%v) want (killed,nil)", res, err)
	}
	if sent := term.list(); len(sent) != 1 || sent[0] != syscall.SIGTERM {
		t.Fatalf("want exactly one SIGTERM, got %v", sent)
	}
	// The escalation must target the worker's PROCESS GROUP (-pid) so the
	// agent children die with it instead of reparenting to init.
	if got := killed.list(); len(got) != 1 || got[0] != -4242 {
		t.Fatalf("want group kill of -4242, got %v", got)
	}
}

func TestRetire_SigtermFails(t *testing.T) {
	s := newSup(t)
	stubSignals(t, func(os.Signal) error { return errors.New("boom") })
	res, err := s.Retire(&os.Process{Pid: 4242}, make(chan struct{}), DrainOrder{Deadline: time.Minute}, nil)
	if err == nil || res != RetireFailed {
		t.Fatalf("got (%v,%v) want (failed,err)", res, err)
	}
}

// TestRetire_KillRacesNaturalExit: SIGKILL fails because the worker
// already exited and was reaped — that is a graceful retirement, not a
// failure.
func TestRetire_KillRacesNaturalExit(t *testing.T) {
	s := newSup(t)
	dead := make(chan struct{})
	stubSignals(t, nil)
	old := kill
	kill = func(int, syscall.Signal) error {
		close(dead) // reaper observed the exit just now
		return errors.New("process already finished")
	}
	t.Cleanup(func() { kill = old })
	res, err := s.Retire(&os.Process{Pid: 4242}, dead, DrainOrder{Deadline: time.Millisecond}, fireOnce())
	if err != nil || res != RetireGraceful {
		t.Fatalf("got (%v,%v) want (graceful,nil)", res, err)
	}
}

func TestRetire_KillFails(t *testing.T) {
	s := newSup(t)
	stubSignals(t, nil)
	stubKill(t, errors.New("boom"))
	res, err := s.Retire(&os.Process{Pid: 4242}, make(chan struct{}), DrainOrder{Deadline: time.Millisecond}, fireOnce())
	if err == nil || res != RetireFailed {
		t.Fatalf("got (%v,%v) want (failed,err)", res, err)
	}
}

// TestRetire_KilledButNeverReaped covers the pathological branch where
// even the post-SIGKILL reap is not observed: Retire still returns.
func TestRetire_KilledButNeverReaped(t *testing.T) {
	s := newSup(t)
	stubSignals(t, nil)
	stubKill(t, nil)
	res, err := s.Retire(&os.Process{Pid: 4242}, make(chan struct{}), DrainOrder{Deadline: time.Millisecond}, fireAfter)
	if err == nil || res != RetireFailed {
		t.Fatalf("got (%v,%v) want (failed,err)", res, err)
	}
}

// TestRetire_DefaultDeadline covers the deadline<=0 defaulting branch.
func TestRetire_DefaultDeadline(t *testing.T) {
	s := newSup(t)
	stubSignals(t, nil)
	dead := make(chan struct{})
	close(dead)
	if res, err := s.Retire(&os.Process{Pid: 4242}, dead, DrainOrder{}, nil); err != nil || res != RetireGraceful {
		t.Fatalf("got (%v,%v) want (graceful,nil)", res, err)
	}
}

func TestRetireResultString(t *testing.T) {
	for r, want := range map[RetireResult]string{
		RetireGraceful: "graceful", RetireKilled: "killed", RetireFailed: "failed",
	} {
		if got := r.String(); got != want {
			t.Fatalf("%d => %q want %q", r, got, want)
		}
	}
}

// ---- Retiring / KillRetiring ----

// TestRetiring_TracksGenerationsOldestFirst proves the pileup signature
// from the incident is observable: several generations retiring at once,
// reported oldest first.
func TestRetiring_TracksGenerationsOldestFirst(t *testing.T) {
	s := newSup(t)
	s.trackRetiring(101)
	time.Sleep(2 * time.Millisecond)
	s.trackRetiring(102)
	got := s.Retiring()
	if len(got) != 2 || got[0].Pid != 101 || got[1].Pid != 102 {
		t.Fatalf("want [101 102] oldest-first, got %+v", got)
	}
	s.untrackRetiring(101)
	if got := s.Retiring(); len(got) != 1 || got[0].Pid != 102 {
		t.Fatalf("untrack failed: %+v", got)
	}
}

// killRecorder records the pid-based kill seam's calls (group SIGKILLs)
// without ever delivering a real signal.
type killRecorder struct {
	mu   sync.Mutex
	pids []int
	ch   chan int
}

func stubKill(t *testing.T, err error) *killRecorder {
	t.Helper()
	r := &killRecorder{ch: make(chan int, 8)}
	old := kill
	kill = func(pid int, sig syscall.Signal) error {
		if sig != syscall.SIGKILL {
			t.Errorf("want SIGKILL, got %v", sig)
		}
		r.mu.Lock()
		r.pids = append(r.pids, pid)
		r.mu.Unlock()
		r.ch <- pid
		return err
	}
	t.Cleanup(func() { kill = old })
	return r
}

func (r *killRecorder) list() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.pids...)
}

func TestKillRetiring(t *testing.T) {
	s := newSup(t)
	pids := stubKill(t, nil)
	if reps := s.KillRetiring(); len(reps) != 0 {
		t.Fatalf("nothing retiring => no reports, got %+v", reps)
	}
	s.trackRetiring(4242)
	reps := s.KillRetiring()
	if len(reps) != 1 || reps[0].Pid != 4242 || reps[0].Err != nil {
		t.Fatalf("want one clean kill report, got %+v", reps)
	}
	if got := pids.list(); len(got) != 1 || got[0] != -4242 {
		t.Fatalf("want group SIGKILL to -4242, got %v", got)
	}
}

func TestKillRetiring_Error(t *testing.T) {
	s := newSup(t)
	stubKill(t, errors.New("boom"))
	s.trackRetiring(4242)
	reps := s.KillRetiring()
	if len(reps) != 1 || reps[0].Err == nil {
		t.Fatalf("want a kill error report, got %+v", reps)
	}
}

// TestKillRetiring_RefusesBroadcastPid: a bogus tracked pid must never be
// signalled — kill(0)/kill(-1) would fan out to the process group.
func TestKillRetiring_RefusesBroadcastPid(t *testing.T) {
	s := newSup(t)
	pids := stubKill(t, nil)
	s.trackRetiring(0)
	reps := s.KillRetiring()
	if len(reps) != 1 || reps[0].Err == nil {
		t.Fatalf("want a refusal report, got %+v", reps)
	}
	if got := pids.list(); len(got) != 0 {
		t.Fatalf("must not signal, got %v", got)
	}
}

// TestKillWorkerGroup_RefusesSelf: the group kill must never target the
// supervisor's own pid or process group — that would take the supervisor
// (and, under a terminal, the whole session) down with the worker.
func TestKillWorkerGroup_RefusesSelf(t *testing.T) {
	rec := stubKill(t, nil)
	for _, pid := range []int{os.Getpid(), syscall.Getpgrp()} {
		if err := killWorkerGroup(pid); err == nil {
			t.Fatalf("pid %d: want refusal", pid)
		}
	}
	if got := rec.list(); len(got) != 0 {
		t.Fatalf("must not signal, got %v", got)
	}
}

//go:build unix

package supervisor

// Bounded drain: the outer safety net around worker retirement.
//
// A worker retires by receiving SIGTERM and calling http.Server.Shutdown,
// which returns only once every in-flight handler has returned. The
// per-stream idle-write backstop (internal/httpsrv) cancels a turn that
// produces no agent output, and that is normally enough — but it is an
// INNER net: it can only cut a turn whose wedge it can observe. A handler
// stuck somewhere the idle clock keeps being touched (or stuck after the
// turn, in finalization) leaves Shutdown blocked forever, the worker never
// exits, and every reload leaks another live worker generation. That is
// exactly what happened in production: five worker generations alive after
// 17 days of reloads, none of which exited on SIGTERM, until systemd's
// TimeoutStopSec expired and SIGKILLed the whole control group.
//
// So drain is bounded at two levels:
//
//   - Worker: DrainServer gives Shutdown a deadline. On expiry the caller
//     logs what is being abandoned, srv.Close() force-closes the listener
//     and every remaining connection, and ForceExitAfter guarantees the
//     process leaves even if post-drain cleanup itself wedges.
//   - Supervisor: Retire sends SIGTERM and escalates to SIGKILL if the
//     worker has not exited within its own (longer) deadline, so a wedged
//     worker can never outlive the supervisor that retired it.
//
// ...and it is bounded by a DIFFERENT deadline in each of the two
// situations a worker is retired in — a service stop (externally bounded,
// DefaultDrainDeadline) and a SIGHUP worker swap (nothing external
// waiting, DefaultSwapDrainDeadline). The supervisor says which, over the
// control pipe; see drainorder.go.
//
// Graceful semantics are unchanged below the deadline: a normal in-flight
// SSE stream completes undisturbed on a reload, and the worker exits as
// soon as the last one finishes.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"syscall"
	"time"
)

// DefaultDrainDeadline bounds a worker's graceful drain when the SERVICE
// is stopping (DrainStop): systemd SIGTERMs the cgroup and gives up at
// TimeoutStopSec=90s, and a supervisor self-upgrade is not serving until
// the worker is gone. Something external is waiting either way, so the
// budget is short. The chain on a service stop is
//
//	worker drain (45s) + ForceExitGrace (5s)   => worker gone by ~50s
//	supervisor escalation (45s + RetireGrace)  => SIGKILL at ~60s
//	supervisor exits                           => ~60s, 30s of margin
//
// Operators who want more room raise -drain-deadline; keep it under
// TimeoutStopSec minus RetireGrace.
const DefaultDrainDeadline = 45 * time.Second

// DefaultSwapDrainDeadline bounds a worker's graceful drain when it has
// been SWAPPED OUT by a SIGHUP reload (DrainSwap). Nothing external is
// waiting on this chain: the supervisor survives, the new worker is
// already accepting, and the retiring generation exists only to finish
// the streams it still holds. The chain on a swap is
//
//	worker drain (30m) + ForceExitGrace (5s)   => worker gone by ~30m
//	supervisor escalation (30m + RetireGrace)  => SIGKILL at ~30m15s
//	supervisor keeps serving throughout        => no external timeout
//
// Agent turns legitimately run tens of minutes, and bounding them by
// wall-clock is what made a reload force-close a HEALTHY 42-minute turn
// and show the user a red "peer disconnected before response". Liveness,
// not time, is the right resource to bound here: the per-stream
// idle-write backstop (-idle-write-timeout, progress-resetting) already
// reaps a genuinely wedged turn in minutes, so this is a LEAK BACKSTOP —
// the thing that stops an unkillable generation living forever — not a
// working bound. Raise it with -swap-drain-deadline.
const DefaultSwapDrainDeadline = 30 * time.Minute

// RetireGrace is the extra headroom the supervisor allows a retiring
// worker beyond that worker's own drain deadline before escalating to
// SIGKILL. It covers the worker's post-deadline force-close plus process
// teardown; without it the supervisor could kill a worker that was about
// to exit on its own.
const RetireGrace = 15 * time.Second

// ForceExitGrace bounds post-deadline cleanup (agent shutdown, MCP host
// close, deferred closers) once a drain has already been force-cut. If
// cleanup itself is wedged — a hung agent child is the likely cause — the
// process exits anyway rather than becoming the next undead generation.
const ForceExitGrace = 5 * time.Second

// killReapGrace bounds how long Retire waits for a SIGKILLed worker's
// exit to be reaped before giving up on the wait. SIGKILL is unblockable,
// so this only ever fires if the caller's reaper is itself stuck.
const killReapGrace = 5 * time.Second

// Shutdowner is the subset of *http.Server that DrainServer drives.
// *http.Server satisfies it.
type Shutdowner interface {
	Shutdown(ctx context.Context) error
	Close() error
}

// DrainResult reports how a bounded drain resolved.
type DrainResult int

const (
	// DrainGraceful: every in-flight stream completed within the deadline.
	DrainGraceful DrainResult = iota
	// DrainForced: the deadline expired with streams still in flight; the
	// server was force-closed and they were abandoned.
	DrainForced
)

// DrainServer stops srv accepting and drains its in-flight requests,
// bounded by deadline. Below the deadline the behaviour is exactly the
// old unbounded one: Shutdown returns as soon as the last handler does,
// so a legitimately streaming turn finishes undisturbed.
//
// On expiry onForce (if non-nil) is called BEFORE the force-close, so the
// caller can log precisely which streams are being abandoned while they
// are still registered, and srv.Close() then tears down the listener and
// every remaining connection. deadline <= 0 means DefaultDrainDeadline —
// there is deliberately no "wait forever" value.
//
// A non-deadline error from Shutdown (a listener that failed to close)
// means the drain itself DID complete, so it is reported as graceful with
// the error returned for logging rather than dressed up as a force-cut.
func DrainServer(srv Shutdowner, deadline time.Duration, onForce func()) (DrainResult, error) {
	if deadline <= 0 {
		deadline = DefaultDrainDeadline
	}
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	err := srv.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		return DrainGraceful, err
	}
	if onForce != nil {
		onForce()
	}
	_ = srv.Close()
	return DrainForced, err
}

// ForceExitAfter arms a last-resort process exit d from now and returns a
// stop function that disarms it. Use it around post-deadline cleanup: the
// drain has already been force-cut, so nothing left to do justifies
// hanging the process. exit must be non-nil — the caller owns the exit
// status and the log line that explains it.
func ForceExitAfter(d time.Duration, exit func()) (stop func()) {
	t := time.AfterFunc(d, exit)
	return func() { t.Stop() }
}

// ---------------------------------------------------------------------------
// Supervisor-side retirement
// ---------------------------------------------------------------------------

// signalProc is the process-signal seam (default (*os.Process).Signal).
// Tests stub it so no real signal is ever delivered — see the `kill` seam
// comment for why that matters.
var signalProc = func(p *os.Process, sig os.Signal) error { return p.Signal(sig) }

// killWorkerGroup SIGKILLs a worker's entire PROCESS GROUP. Every worker
// is spawned with Setpgid (see Spawn), so its pgid equals its pid and the
// group also holds the agent child processes — the incident's "pids …
// plus children". Killing only the worker would reparent a wedged agent
// to init and leak it, which is half the pileup we are fixing.
//
// The guards matter: a non-positive pid, our own pid, or our own process
// group would turn this into a broadcast that kills the supervisor (and
// under a terminal, the whole session).
func killWorkerGroup(pid int) error {
	switch {
	case pid <= 1:
		return fmt.Errorf("supervisor: refusing to kill non-worker pid %d", pid)
	case pid == os.Getpid() || pid == syscall.Getpgrp():
		return fmt.Errorf("supervisor: refusing to kill own process group %d", pid)
	}
	return kill(-pid, syscall.SIGKILL)
}

// RetireResult reports how a worker retirement resolved.
type RetireResult int

const (
	// RetireGraceful: the worker exited on SIGTERM within the deadline.
	RetireGraceful RetireResult = iota
	// RetireKilled: the deadline expired and the worker was SIGKILLed.
	RetireKilled
	// RetireFailed: a signal could not be delivered, or the killed
	// worker's exit was never reaped.
	RetireFailed
)

func (r RetireResult) String() string {
	switch r {
	case RetireGraceful:
		return "graceful"
	case RetireKilled:
		return "killed"
	default:
		return "failed"
	}
}

// RetiringWorker describes a worker generation that has been told to
// retire but has not yet been observed to exit.
type RetiringWorker struct {
	Pid   int
	Since time.Time
}

// Retire drains worker p and escalates to SIGKILL if it has not exited
// within order.RetireDeadline(). order is the retirement contract (see
// drainorder.go): it is handed to the worker over its control pipe just
// before the SIGTERM, so BOTH bounds — the worker's own drain deadline
// and this escalation — are derived from the same value and can never
// drift apart. dead is the caller's reaper signal — a channel closed (or
// sent to) once p's Wait has returned. after is the timer seam (defaults
// to time.After).
//
// Retire blocks until p's exit is observed or escalation has run, so a
// caller that must be quiescent (supervisor self-reexec, shutdown) can
// call it inline; a caller that must keep serving (SIGHUP worker swap)
// runs it in a goroutine. While it is in flight the pid is listed by
// Retiring, so a pileup of undead generations is observable.
func (s *Supervisor) Retire(p *os.Process, dead <-chan struct{}, order DrainOrder, after func(time.Duration) <-chan time.Time) (RetireResult, error) {
	if after == nil {
		after = time.After
	}
	deadline := order.RetireDeadline()
	s.trackRetiring(p.Pid)
	defer s.untrackRetiring(p.Pid)

	if err := s.drain(p, order); err != nil {
		return RetireFailed, err
	}
	select {
	case <-dead:
		return RetireGraceful, nil
	case <-after(deadline):
	}
	if err := killWorkerGroup(p.Pid); err != nil {
		// Raced with a natural exit: if the reaper has observed it by
		// now the retirement was graceful after all.
		select {
		case <-dead:
			return RetireGraceful, nil
		default:
			return RetireFailed, fmt.Errorf("supervisor: kill worker %d: %w", p.Pid, err)
		}
	}
	select {
	case <-dead:
		return RetireKilled, nil
	case <-after(killReapGrace):
		return RetireFailed, fmt.Errorf("supervisor: worker %d not reaped %s after SIGKILL", p.Pid, killReapGrace)
	}
}

// trackRetiring records pid as a worker generation being retired.
func (s *Supervisor) trackRetiring(pid int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retiring == nil {
		s.retiring = make(map[int]time.Time)
	}
	s.retiring[pid] = time.Now()
}

// untrackRetiring drops pid from the retiring set.
func (s *Supervisor) untrackRetiring(pid int) {
	s.mu.Lock()
	delete(s.retiring, pid)
	s.mu.Unlock()
}

// Retiring returns the worker generations currently being retired,
// oldest first. A non-empty result at the moment of a new swap means a
// previous generation has not exited yet — the pileup signature from the
// production incident — so the caller should log it loudly.
func (s *Supervisor) Retiring() []RetiringWorker {
	s.mu.Lock()
	out := make([]RetiringWorker, 0, len(s.retiring))
	for pid, since := range s.retiring {
		out = append(out, RetiringWorker{Pid: pid, Since: since})
	}
	s.mu.Unlock()
	slices.SortFunc(out, func(a, b RetiringWorker) int { return a.Since.Compare(b.Since) })
	return out
}

// KillReport records the outcome of one SIGKILL delivered by
// KillRetiring.
type KillReport struct {
	RetiringWorker
	// Err is non-nil if the signal could not be delivered (the worker may
	// have exited in the meantime).
	Err error
}

// KillRetiring SIGKILLs every worker generation still tracked as
// retiring and reports what it did. Called on supervisor shutdown: a
// retiring worker must never outlive the supervisor that retired it —
// the parent-liveness pipe already prompts a healthy worker to exit, and
// this covers one too wedged to notice. Delivery is a process-group
// SIGKILL (see killWorkerGroup) so the worker's agent children go with
// it instead of being reparented to init.
func (s *Supervisor) KillRetiring() []KillReport {
	rs := s.Retiring()
	out := make([]KillReport, 0, len(rs))
	for _, w := range rs {
		rep := KillReport{RetiringWorker: w}
		if err := killWorkerGroup(w.Pid); err != nil {
			rep.Err = fmt.Errorf("supervisor: kill retiring worker %d: %w", w.Pid, err)
		}
		out = append(out, rep)
	}
	return out
}

// interface guard: *http.Server is the production Shutdowner.
var _ Shutdowner = (*http.Server)(nil)

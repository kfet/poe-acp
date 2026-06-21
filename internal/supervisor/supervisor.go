//go:build unix

// Package supervisor implements a master/worker process model that makes
// zero-downtime relay upgrades structurally safe on BOTH systemd and
// launchd.
//
// The shape:
//
//   - Supervisor S is the process the init system launches and tracks
//     (systemd MainPID / launchd label PID). It binds the listen socket
//     ONCE (net.Listen) and never rebinds. It forks worker processes,
//     handing each the listener fd (cleared of O_CLOEXEC, delivered as
//     POE_ACP_WORKER_FD=3) plus the read end of a parent-liveness pipe
//     (POE_ACP_DEATH_FD=4). S is tiny and rarely changes; it never exits
//     during a worker upgrade, so the init system never observes the
//     tracked PID vanish — EADDRINUSE on relaunch is impossible.
//
//   - Worker W is detected by POE_ACP_WORKER_FD being set. It recovers
//     the listener via net.FileListener and runs ALL the relay logic
//     (httpsrv/router/...). This is where churn lives.
//
// Hot upgrade (the common path): S receives SIGHUP, forks a NEW worker W2
// on the SAME fd, waits for W2 to signal ready (SIGUSR1 to S), then tells
// W1 to stop accepting and drain its in-flight streams to completion
// (SIGTERM, which the worker maps to http.Server.Shutdown). Because the
// socket is bound by S and shared, the kernel keeps queueing new
// connections throughout — there is never a refusal window. The
// Poe-SSE-specific drain semantics (honour r.Context().Done, the
// per-stream idle-write backstop) live in internal/httpsrv; this package
// is transport-generic and knows only about a net.Listener and child
// processes.
//
// Parent-death detection: each worker holds the read end of a pipe whose
// write end S keeps open. If S dies, the read end EOFs and the worker
// exits, freeing the socket so the init system can relaunch S cleanly
// (a brief cold blip) rather than orphan-locking the port.
//
// Shim self-upgrade (rare): S's own code seldom changes. When it must, S
// re-execs ITSELF in place via syscall.Exec — same PID, inheriting the
// listen fd (POE_ACP_SUPERVISOR_FD) so it does not rebind. The caller
// drains the current worker first so no in-flight stream is dropped.
//
// Wire contract (env vars S sets on a forked worker):
//
//	POE_ACP_WORKER_FD=3   listener inherited as ExtraFiles[0]; presence => worker
//	POE_ACP_DEATH_FD=4    parent-liveness pipe read end as ExtraFiles[1]
//
// And on a supervisor self-reexec:
//
//	POE_ACP_SUPERVISOR_FD=N  listener fd to recover instead of binding
//
// Unix-only by deployment target.
package supervisor

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
)

const (
	// EnvWorkerFD names the env var carrying the inherited listener fd in
	// a worker. Its presence also signals "you are a worker".
	EnvWorkerFD = "POE_ACP_WORKER_FD"
	// EnvDeathFD names the env var carrying the parent-liveness pipe read
	// end in a worker.
	EnvDeathFD = "POE_ACP_DEATH_FD"
	// EnvSupervisorFD names the env var carrying the listener fd a
	// supervisor recovers after a self-reexec instead of binding fresh.
	EnvSupervisorFD = "POE_ACP_SUPERVISOR_FD"

	// listenerChildFD / deathChildFD are the fixed fds the inherited
	// files land on in a worker (0,1,2 are stdio, so ExtraFiles[0]=3,
	// ExtraFiles[1]=4).
	listenerChildFD = 3
	deathChildFD    = 4
)

// IsWorker reports whether this process was forked as a worker (the
// listener-fd env var is set). Call it before flag parsing to branch
// into worker vs supervisor mode.
func IsWorker() bool { return os.Getenv(EnvWorkerFD) != "" }

// getppid is a seam for the worker's parent (supervisor) pid, overridden
// in tests so NotifyReady never signals the real test-runner parent.
var getppid = os.Getppid

// kill is the signal-delivery seam (default syscall.Kill). Tests MUST stub
// this and assert on its arguments rather than deliver a real signal: a
// real syscall.Kill with a non-positive pid (0 => caller's process group,
// -1 => every process the user can signal) would propagate to the test
// runner and any sibling agent processes and terminate them.
var kill = syscall.Kill

// ---------------------------------------------------------------------------
// Worker side
// ---------------------------------------------------------------------------

// WorkerListener recovers the serving listener from the inherited fd named
// by EnvWorkerFD. It is an error to call this in a process that is not a
// worker (EnvWorkerFD unset).
func WorkerListener() (net.Listener, error) {
	fdStr := os.Getenv(EnvWorkerFD)
	if fdStr == "" {
		return nil, fmt.Errorf("supervisor: %s unset (not a worker)", EnvWorkerFD)
	}
	fd, err := strconv.Atoi(fdStr)
	if err != nil {
		return nil, fmt.Errorf("supervisor: bad %s=%q: %w", EnvWorkerFD, fdStr, err)
	}
	f := os.NewFile(uintptr(fd), "worker-listener")
	ln, err := net.FileListener(f)
	closeFile(f) // FileListener dups internally; our copy is redundant.
	if err != nil {
		return nil, fmt.Errorf("supervisor: recover listener fd %d: %w", fd, err)
	}
	return ln, nil
}

// WatchParent spawns a goroutine that blocks reading the parent-liveness
// pipe named by EnvDeathFD. The supervisor holds the write end; when the
// supervisor dies the read end EOFs and onDeath is invoked (typically
// os.Exit to free the socket). It is a no-op when EnvDeathFD is unset
// (e.g. a worker started outside a supervisor, in tests).
func WatchParent(onDeath func()) {
	fdStr := os.Getenv(EnvDeathFD)
	if fdStr == "" {
		return
	}
	fd, err := strconv.Atoi(fdStr)
	if err != nil {
		// A malformed fd means a broken contract; treat as parent-gone
		// rather than ignore, so we never wedge holding the socket.
		onDeath()
		return
	}
	f := os.NewFile(uintptr(fd), "parent-liveness")
	go func() {
		defer closeFile(f)
		// Any read return (EOF on parent death, or error) means the
		// parent is gone or the pipe is unusable: relinquish the socket.
		_, _ = io.Copy(io.Discard, f)
		onDeath()
	}()
}

// NotifyReady signals the supervisor (this worker's parent) that the
// worker's Serve is live and an upgrade may proceed to drain the previous
// worker. It is a no-op when EnvWorkerFD is unset (worker started outside
// a supervisor).
func NotifyReady() error {
	if os.Getenv(EnvWorkerFD) == "" {
		return nil
	}
	pid := getppid()
	if pid <= 1 {
		// Guard the contract: a non-positive pid would broadcast (0 =>
		// our process group, -1 => every signallable process) and pid 1
		// is never our supervisor. Refuse rather than deliver wildly.
		return fmt.Errorf("supervisor: refusing to signal non-worker parent pid %d", pid)
	}
	if err := kill(pid, syscall.SIGUSR1); err != nil {
		return fmt.Errorf("supervisor: signal supervisor ready: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Supervisor side
// ---------------------------------------------------------------------------

// Supervisor owns the bound listener and forks/manages workers.
type Supervisor struct {
	ln      net.Listener
	binPath string
	args    []string
	env     []string

	deathR *os.File // read end handed to each worker
	deathW *os.File // write end held open for the supervisor's lifetime

	// start is the process-spawn seam (default cmd.Start); overridden in
	// tests to assert command assembly without forking the test binary.
	start func(*exec.Cmd) error
	// execSelf is the in-place re-exec seam (default syscall.Exec).
	execSelf func(argv0 string, argv, envv []string) error
	// dup is the fd-dup seam (default syscall.Dup) for SelfReexec.
	dup func(int) (int, error)

	mu  sync.Mutex
	cur *os.Process // the current serving worker
}

// Config configures a Supervisor.
type Config struct {
	// Addr is the listen address bound on a cold start. Ignored when the
	// process is a supervisor self-reexec (listener recovered from
	// EnvSupervisorFD).
	Addr string
	// BinPath is the executable to fork as workers / re-exec. Defaults to
	// os.Args[0].
	BinPath string
	// Args are the worker argv tail. Defaults to os.Args[1:].
	Args []string
	// Env is the base environment for workers; the worker contract vars
	// are appended per spawn. Defaults to os.Environ().
	Env []string
}

// New binds (or, on a self-reexec, recovers) the listener and prepares the
// parent-liveness pipe. The caller drives the lifecycle via Spawn / Swap /
// SelfReexec and the WaitReady helper.
func New(cfg Config) (*Supervisor, error) {
	ln, err := supervisorListen(cfg.Addr)
	if err != nil {
		return nil, err
	}
	r, w, err := pipeFn()
	if err != nil {
		closeListener(ln)
		return nil, fmt.Errorf("supervisor: death pipe: %w", err)
	}
	s := &Supervisor{
		ln:       ln,
		binPath:  cfg.BinPath,
		args:     cfg.Args,
		env:      cfg.Env,
		deathR:   r,
		deathW:   w,
		start:    func(c *exec.Cmd) error { return c.Start() },
		execSelf: syscall.Exec,
		dup:      syscall.Dup,
	}
	if s.binPath == "" {
		s.binPath = os.Args[0]
	}
	if s.args == nil {
		s.args = os.Args[1:]
	}
	if s.env == nil {
		s.env = os.Environ()
	}
	return s, nil
}

// supervisorListen recovers the listener from EnvSupervisorFD after a
// self-reexec, or binds addr fresh on a cold start.
func supervisorListen(addr string) (net.Listener, error) {
	if fdStr := os.Getenv(EnvSupervisorFD); fdStr != "" {
		fd, err := strconv.Atoi(fdStr)
		if err != nil {
			return nil, fmt.Errorf("supervisor: bad %s=%q: %w", EnvSupervisorFD, fdStr, err)
		}
		f := os.NewFile(uintptr(fd), "supervisor-listener")
		ln, ferr := net.FileListener(f)
		closeFile(f)
		if ferr != nil {
			return nil, fmt.Errorf("supervisor: recover listener fd %d: %w", fd, ferr)
		}
		return ln, nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return ln, nil
}

// Addr returns the bound listener's address (for logging).
func (s *Supervisor) Addr() net.Addr { return s.ln.Addr() }

// Current returns the current serving worker process (may be nil before
// the first Spawn).
func (s *Supervisor) Current() *os.Process {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

// SetCurrent records p as the current serving worker.
func (s *Supervisor) SetCurrent(p *os.Process) {
	s.mu.Lock()
	s.cur = p
	s.mu.Unlock()
}

// Spawn forks a worker that inherits the listener and parent-liveness pipe
// plus the worker contract env. It does NOT set the worker as current; the
// caller does so once the worker has signalled ready.
func (s *Supervisor) Spawn() (*os.Process, error) {
	if _, err := os.Stat(s.binPath); err != nil {
		return nil, fmt.Errorf("supervisor: preflight %s: %w", s.binPath, err)
	}
	lf, err := listenerFile(s.ln)
	if err != nil {
		return nil, fmt.Errorf("supervisor: dup listener: %w", err)
	}
	defer closeFile(lf)

	cmd := exec.Command(s.binPath, s.args...)
	cmd.Env = append(append([]string{}, s.env...),
		fmt.Sprintf("%s=%d", EnvWorkerFD, listenerChildFD),
		fmt.Sprintf("%s=%d", EnvDeathFD, deathChildFD),
	)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{lf, s.deathR}
	// Put each worker in its own process group so a signal aimed at the
	// supervisor's group (e.g. a terminal SIGINT) never reaches workers
	// directly: the supervisor mediates all worker lifecycle signals.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := s.start(cmd); err != nil {
		return nil, fmt.Errorf("supervisor: start worker: %w", err)
	}
	return cmd.Process, nil
}

// Drain tells worker p to stop accepting and drain its in-flight streams
// to natural completion by sending SIGTERM (the worker maps SIGTERM to
// http.Server.Shutdown). p exits once drained; the caller reaps it.
func (s *Supervisor) Drain(p *os.Process) error {
	if err := p.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("supervisor: drain worker %d: %w", p.Pid, err)
	}
	return nil
}

// SelfReexec replaces the supervisor process in place with a fresh copy of
// the on-disk binary, preserving the bound listener fd (handed via
// EnvSupervisorFD) so the new image does not rebind. The caller MUST have
// already drained and reaped the current worker so no in-flight stream is
// dropped; the new supervisor cold-starts a fresh worker. On success this
// does not return.
func (s *Supervisor) SelfReexec() error {
	lf, err := listenerFile(s.ln)
	if err != nil {
		return fmt.Errorf("supervisor: dup listener: %w", err)
	}
	// Re-dup to a plain (non-O_CLOEXEC) fd so it survives execve.
	fd, err := s.dup(int(lf.Fd()))
	closeFile(lf)
	if err != nil {
		return fmt.Errorf("supervisor: dup listener fd: %w", err)
	}
	argv := append([]string{s.binPath}, s.args...)
	env := append(append([]string{}, s.env...),
		fmt.Sprintf("%s=%d", EnvSupervisorFD, fd))
	if err := s.execSelf(s.binPath, argv, env); err != nil {
		return fmt.Errorf("supervisor: re-exec self: %w", err)
	}
	return nil // unreachable on success (execve replaces the image)
}

// ---------------------------------------------------------------------------
// Readiness arbitration (pure logic, signal/death/timeout sources injected)
// ---------------------------------------------------------------------------

// ReadyResult reports how WaitReady resolved.
type ReadyResult int

const (
	// ReadyOK: the worker signalled ready.
	ReadyOK ReadyResult = iota
	// ReadyDied: the worker exited before signalling ready.
	ReadyDied
	// ReadyTimeout: neither ready nor death arrived within the budget.
	ReadyTimeout
)

func (r ReadyResult) String() string {
	switch r {
	case ReadyOK:
		return "ready"
	case ReadyDied:
		return "died"
	default:
		return "timeout"
	}
}

// WaitReady blocks until a freshly spawned worker signals ready, dies, or
// the timeout elapses, whichever comes first. ready receives a token when
// a SIGUSR1 from the worker arrives; dead is closed (or receives) when the
// worker's Wait returns; after produces the timeout channel (seam for
// tests, defaults to time.After when nil).
func WaitReady(ready <-chan struct{}, dead <-chan struct{}, timeout time.Duration, after func(time.Duration) <-chan time.Time) ReadyResult {
	if after == nil {
		after = time.After
	}
	select {
	case <-ready:
		return ReadyOK
	case <-dead:
		return ReadyDied
	case <-after(timeout):
		return ReadyTimeout
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// pipeFn is the pipe-creation seam (default os.Pipe) so the death-pipe
// allocation failure path in New is testable.
var pipeFn = os.Pipe

// closeFile closes f discarding the error: every caller closes a redundant
// dup or a post-success fd where a Close failure is not actionable.
func closeFile(f *os.File) { _ = f.Close() }

// closeListener closes ln discarding the error (cleanup on a New failure
// path; nothing actionable remains).
func closeListener(ln net.Listener) { _ = ln.Close() }

// listenerFile returns a dup of the listener's fd ready to pass as
// ExtraFiles (os/exec clears O_CLOEXEC on the child copy). The original
// net.Listener keeps working in the supervisor.
func listenerFile(ln net.Listener) (*os.File, error) {
	tl, ok := ln.(*net.TCPListener)
	if !ok {
		return nil, fmt.Errorf("listener is %T, want *net.TCPListener", ln)
	}
	return tl.File()
}

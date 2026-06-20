//go:build unix

// Package graceful implements zero-downtime restart for a single
// listening TCP socket via the classic two-overlapping-processes
// pattern: the parent hands its listener fd to a freshly exec'd child,
// the child takes new connections, and the parent keeps serving its
// already-accepted connections until they drain, then exits.
//
// The package is deliberately transport-generic: it knows about a
// net.Listener and an *http.Server, nothing about Poe SSE. The
// Poe-specific stream-drain semantics (honour r.Context().Done, the
// per-stream idle-write backstop) live in internal/httpsrv. This keeps
// a future extract into a shared module a lift rather than a rewrite.
//
// Wire contract (env vars set by parent on the child):
//
//	POE_ACP_GRACEFUL_FD=3          listener inherited as ExtraFiles[0]
//	POE_ACP_GRACEFUL_PARENT_PID=N  child SIGUSR1s this pid once live
//
// Unix-only by deployment target.
package graceful

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
)

const (
	// EnvFD names the env var carrying the inherited listener fd number.
	// Its presence also signals "you are a graceful-restart child".
	EnvFD = "POE_ACP_GRACEFUL_FD"
	// EnvParentPID names the env var carrying the parent pid the child
	// SIGUSR1s once its own Serve is live.
	EnvParentPID = "POE_ACP_GRACEFUL_PARENT_PID"
	// childFD is the fixed fd the listener lands on in the child (0,1,2
	// are stdio, so ExtraFiles[0] becomes fd 3).
	childFD = 3
)

// ErrUpgradeInFlight is returned by Manager.Upgrade when a child spawned
// by a previous Upgrade has not yet been resolved (readied or reaped).
var ErrUpgradeInFlight = errors.New("graceful: upgrade already in flight")

// Listen acquires the serving listener. When EnvFD is set this process
// is a graceful-restart child and the listener is recovered from the
// inherited fd; child is true. Otherwise it binds addr fresh; child is
// false. The child bool tells the caller whether it must signal the
// parent ready (via NotifyParentReady) once its Serve is live.
func Listen(addr string) (ln net.Listener, child bool, err error) {
	if fdStr := os.Getenv(EnvFD); fdStr != "" {
		fd, perr := strconv.Atoi(fdStr)
		if perr != nil {
			return nil, false, fmt.Errorf("graceful: bad %s=%q: %w", EnvFD, fdStr, perr)
		}
		f := os.NewFile(uintptr(fd), "graceful-listener")
		l, ferr := net.FileListener(f)
		// FileListener dups the fd internally, so our copy is redundant.
		closeFile(f)
		if ferr != nil {
			return nil, false, fmt.Errorf("graceful: recover listener fd %d: %w", fd, ferr)
		}
		return l, true, nil
	}
	l, lerr := net.Listen("tcp", addr)
	if lerr != nil {
		return nil, false, lerr
	}
	return l, false, nil
}

// NotifyParentReady signals the parent (named by EnvParentPID) that this
// child's Serve is live and the parent may begin draining. It is a no-op
// when EnvParentPID is unset (cold start, no parent to notify).
func NotifyParentReady() error {
	pidStr := os.Getenv(EnvParentPID)
	if pidStr == "" {
		return nil
	}
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		return fmt.Errorf("graceful: bad %s=%q: %w", EnvParentPID, pidStr, err)
	}
	if err := syscall.Kill(pid, syscall.SIGUSR1); err != nil {
		return fmt.Errorf("graceful: signal parent %d: %w", pid, err)
	}
	return nil
}

// Manager owns the parent-side upgrade lifecycle for one listener.
type Manager struct {
	ln      net.Listener
	binPath string
	args    []string
	env     []string

	// start is the process-spawn seam (default cmd.Start); overridden in
	// tests to assert command assembly without forking the test binary.
	start func(*exec.Cmd) error

	mu        sync.Mutex
	upgrading bool
}

// Config configures a Manager.
type Config struct {
	// Listener is the live parent listener whose fd will be handed to
	// the child. Required.
	Listener net.Listener
	// BinPath is the executable to re-exec. Defaults to os.Args[0].
	BinPath string
	// Args are the child's argv tail. Defaults to os.Args[1:].
	Args []string
	// Env is the base environment for the child; the graceful contract
	// vars are appended. Defaults to os.Environ().
	Env []string
}

// New builds a Manager from cfg, filling defaults.
func New(cfg Config) *Manager {
	m := &Manager{
		ln:      cfg.Listener,
		binPath: cfg.BinPath,
		args:    cfg.Args,
		env:     cfg.Env,
		start:   func(c *exec.Cmd) error { return c.Start() },
	}
	if m.binPath == "" {
		m.binPath = os.Args[0]
	}
	if m.args == nil {
		m.args = os.Args[1:]
	}
	if m.env == nil {
		m.env = os.Environ()
	}
	return m
}

// Upgrade preflights the binary, dups the listener fd, and execs a child
// that inherits the listener and the graceful contract env. It returns
// the child *os.Process so the caller can Wait on it (to detect a child
// that dies before signalling ready). A second Upgrade while one is in
// flight returns ErrUpgradeInFlight.
func (m *Manager) Upgrade() (*os.Process, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.upgrading {
		return nil, ErrUpgradeInFlight
	}
	// Preflight: fail loud rather than fork a broken child.
	if _, err := os.Stat(m.binPath); err != nil {
		return nil, fmt.Errorf("graceful: preflight %s: %w", m.binPath, err)
	}
	lf, err := listenerFile(m.ln)
	if err != nil {
		return nil, fmt.Errorf("graceful: dup listener: %w", err)
	}
	defer closeFile(lf)

	cmd := exec.Command(m.binPath, m.args...)
	cmd.Env = append(append([]string{}, m.env...),
		fmt.Sprintf("%s=%d", EnvFD, childFD),
		fmt.Sprintf("%s=%d", EnvParentPID, os.Getpid()),
	)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{lf}

	if err := m.start(cmd); err != nil {
		return nil, fmt.Errorf("graceful: start child: %w", err)
	}
	m.upgrading = true
	return cmd.Process, nil
}

// Reset clears the in-flight guard after a failed upgrade (child died
// before signalling ready), letting the operator retry.
func (m *Manager) Reset() {
	m.mu.Lock()
	m.upgrading = false
	m.mu.Unlock()
}

// Drain closes the listener (idempotent with the child's own dup) and
// waits, with no wall-clock cap, for every accepted connection to finish.
// http.Server.Shutdown respects hijacked SSE connections, so a turn
// legitimately streaming for minutes survives; a caller-cancelled turn
// ends the instant its connection drops.
func Drain(srv *http.Server) error {
	return srv.Shutdown(context.Background())
}

// closeFile closes f, discarding the error: every caller closes either a
// redundant dup (FileListener already dup'd) or a post-success fd where a
// Close failure is not actionable.
func closeFile(f *os.File) { _ = f.Close() }

// listenerFile returns a dup of the listener's fd with O_CLOEXEC cleared
// (so it survives exec), ready to pass as ExtraFiles. The original
// net.Listener keeps working in the parent.
func listenerFile(ln net.Listener) (*os.File, error) {
	tl, ok := ln.(*net.TCPListener)
	if !ok {
		return nil, fmt.Errorf("listener is %T, want *net.TCPListener", ln)
	}
	return tl.File()
}

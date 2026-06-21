//go:build unix

package supervisor

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// fakeListener is a non-*net.TCPListener so listenerFile's type-assertion
// failure branch is reachable.
type fakeListener struct{}

func (fakeListener) Accept() (net.Conn, error) { return nil, errors.New("nope") }
func (fakeListener) Close() error              { return nil }
func (fakeListener) Addr() net.Addr            { return &net.TCPAddr{} }

func trueBin(t *testing.T) string {
	t.Helper()
	for _, p := range []string{"/usr/bin/true", "/bin/true"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Skip("no true binary")
	return ""
}

func newSup(t *testing.T) *Supervisor {
	t.Helper()
	os.Unsetenv(EnvSupervisorFD)
	s, err := New(Config{Addr: "127.0.0.1:0", BinPath: trueBin(t), Args: []string{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		closeListener(s.ln)
		closeFile(s.deathR)
		closeFile(s.deathW)
	})
	return s
}

// ---- IsWorker / WorkerListener ----

func TestIsWorker(t *testing.T) {
	os.Unsetenv(EnvWorkerFD)
	if IsWorker() {
		t.Fatal("unset => not worker")
	}
	t.Setenv(EnvWorkerFD, "3")
	if !IsWorker() {
		t.Fatal("set => worker")
	}
}

func TestWorkerListener_Unset(t *testing.T) {
	os.Unsetenv(EnvWorkerFD)
	if _, err := WorkerListener(); err == nil {
		t.Fatal("want error when unset")
	}
}

func TestWorkerListener_BadFD(t *testing.T) {
	t.Setenv(EnvWorkerFD, "nope")
	if _, err := WorkerListener(); err == nil {
		t.Fatal("want error on bad fd")
	}
}

func TestWorkerListener_Recover(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	f, err := ln.(*net.TCPListener).File()
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	defer f.Close()
	fd, err := syscall.Dup(int(f.Fd()))
	if err != nil {
		t.Fatalf("dup: %v", err)
	}
	t.Setenv(EnvWorkerFD, strconv.Itoa(fd))
	got, err := WorkerListener()
	if err != nil {
		t.Fatalf("WorkerListener: %v", err)
	}
	got.Close()
}

func TestWorkerListener_FileListenerError(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "notsock")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	defer tmp.Close()
	fd, err := syscall.Dup(int(tmp.Fd()))
	if err != nil {
		t.Fatalf("dup: %v", err)
	}
	t.Setenv(EnvWorkerFD, strconv.Itoa(fd))
	if _, err := WorkerListener(); err == nil {
		t.Fatal("want error recovering listener from a regular file")
	}
}

// ---- WatchParent ----

func TestWatchParent_Unset(t *testing.T) {
	os.Unsetenv(EnvDeathFD)
	called := make(chan struct{}, 1)
	WatchParent(func() { called <- struct{}{} })
	select {
	case <-called:
		t.Fatal("must be a no-op when unset")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestWatchParent_BadFD(t *testing.T) {
	t.Setenv(EnvDeathFD, "nope")
	called := make(chan struct{}, 1)
	WatchParent(func() { called <- struct{}{} })
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("bad fd should trigger onDeath")
	}
}

func TestWatchParent_EOFOnParentDeath(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer r.Close()
	fd, err := syscall.Dup(int(r.Fd()))
	if err != nil {
		t.Fatalf("dup: %v", err)
	}
	t.Setenv(EnvDeathFD, strconv.Itoa(fd))
	called := make(chan struct{}, 1)
	WatchParent(func() { called <- struct{}{} })
	// Simulate supervisor death: close the write end => read EOF.
	_ = w.Close()
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("parent death should trigger onDeath")
	}
}

// ---- NotifyReady ----

func TestNotifyReady_Unset(t *testing.T) {
	os.Unsetenv(EnvWorkerFD)
	if err := NotifyReady(); err != nil {
		t.Fatalf("unset must be no-op: %v", err)
	}
}

// TestNotifyReady_Signals asserts NotifyReady delivers SIGUSR1 to the
// parent pid via the kill seam. It MUST NOT deliver a real signal: the
// seam is stubbed and its arguments asserted, so nothing — not the test
// runner, not a sibling agent — is ever actually signalled.
func TestNotifyReady_Signals(t *testing.T) {
	t.Setenv(EnvWorkerFD, "3")
	oldPpid, oldKill := getppid, kill
	getppid = func() int { return 4242 }
	var gotPid int
	var gotSig syscall.Signal
	kill = func(pid int, sig syscall.Signal) error {
		gotPid, gotSig = pid, sig
		return nil
	}
	defer func() { getppid, kill = oldPpid, oldKill }()
	if err := NotifyReady(); err != nil {
		t.Fatalf("NotifyReady: %v", err)
	}
	if gotPid != 4242 || gotSig != syscall.SIGUSR1 {
		t.Fatalf("kill(%d,%v) want kill(4242,SIGUSR1)", gotPid, gotSig)
	}
}

// TestNotifyReady_KillError asserts NotifyReady surfaces a delivery error
// from the kill seam — again with NO real signal sent.
func TestNotifyReady_KillError(t *testing.T) {
	t.Setenv(EnvWorkerFD, "3")
	oldPpid, oldKill := getppid, kill
	getppid = func() int { return 4242 }
	kill = func(int, syscall.Signal) error { return errors.New("boom") }
	defer func() { getppid, kill = oldPpid, oldKill }()
	if err := NotifyReady(); err == nil {
		t.Fatal("want error when kill seam fails")
	}
}

// TestNotifyReady_NonPositiveParent asserts NotifyReady refuses to signal
// a non-positive (broadcast) or pid-1 parent — the guard that prevents a
// kill(-1)/kill(0) fan-out from ever reaching the test runner.
func TestNotifyReady_NonPositiveParent(t *testing.T) {
	t.Setenv(EnvWorkerFD, "3")
	oldPpid, oldKill := getppid, kill
	killed := false
	kill = func(int, syscall.Signal) error { killed = true; return nil }
	defer func() { getppid, kill = oldPpid, oldKill }()
	for _, pid := range []int{-1, 0, 1} {
		getppid = func() int { return pid }
		if err := NotifyReady(); err == nil {
			t.Fatalf("pid %d: want refusal error", pid)
		}
	}
	if killed {
		t.Fatal("kill seam must never be invoked for a non-positive/pid-1 parent")
	}
}

// ---- supervisorListen / New ----

func TestNew_ColdBind(t *testing.T) {
	s := newSup(t)
	if s.Addr() == nil {
		t.Fatal("nil addr")
	}
}

func TestNew_BindError(t *testing.T) {
	os.Unsetenv(EnvSupervisorFD)
	if _, err := New(Config{Addr: "bad-addr"}); err == nil {
		t.Fatal("want bind error")
	}
}

func TestNew_Defaults(t *testing.T) {
	os.Unsetenv(EnvSupervisorFD)
	s, err := New(Config{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { closeListener(s.ln); closeFile(s.deathR); closeFile(s.deathW) }()
	if s.binPath != os.Args[0] || s.args == nil || s.env == nil {
		t.Fatal("defaults not filled")
	}
}

func TestNew_PipeError(t *testing.T) {
	os.Unsetenv(EnvSupervisorFD)
	old := pipeFn
	pipeFn = func() (*os.File, *os.File, error) { return nil, nil, errors.New("boom") }
	defer func() { pipeFn = old }()
	if _, err := New(Config{Addr: "127.0.0.1:0"}); err == nil {
		t.Fatal("want pipe error")
	}
}

func TestSupervisorListen_SelfReexecRecover(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	f, err := ln.(*net.TCPListener).File()
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	defer f.Close()
	fd, err := syscall.Dup(int(f.Fd()))
	if err != nil {
		t.Fatalf("dup: %v", err)
	}
	t.Setenv(EnvSupervisorFD, strconv.Itoa(fd))
	got, err := supervisorListen("")
	if err != nil {
		t.Fatalf("supervisorListen: %v", err)
	}
	got.Close()
}

func TestSupervisorListen_BadFD(t *testing.T) {
	t.Setenv(EnvSupervisorFD, "nope")
	if _, err := supervisorListen(""); err == nil {
		t.Fatal("want error on bad fd")
	}
}

func TestSupervisorListen_RecoverError(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "notsock")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	defer tmp.Close()
	fd, err := syscall.Dup(int(tmp.Fd()))
	if err != nil {
		t.Fatalf("dup: %v", err)
	}
	t.Setenv(EnvSupervisorFD, strconv.Itoa(fd))
	if _, err := supervisorListen(""); err == nil {
		t.Fatal("want recover error from regular file")
	}
}

// ---- Current / SetCurrent ----

func TestCurrentSetCurrent(t *testing.T) {
	s := newSup(t)
	if s.Current() != nil {
		t.Fatal("nil before spawn")
	}
	p := &os.Process{Pid: 1234}
	s.SetCurrent(p)
	if s.Current() != p {
		t.Fatal("SetCurrent/Current mismatch")
	}
}

// ---- Spawn ----

func TestSpawn_Success(t *testing.T) {
	s := newSup(t)
	p, err := s.Spawn()
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if p == nil {
		t.Fatal("nil process")
	}
	_, _ = p.Wait()
}

func TestSpawn_PreflightFail(t *testing.T) {
	s := newSup(t)
	s.binPath = "/no/such/binary-xyz"
	if _, err := s.Spawn(); err == nil {
		t.Fatal("want preflight error")
	}
}

func TestSpawn_ListenerNotTCP(t *testing.T) {
	s := newSup(t)
	s.ln = fakeListener{}
	if _, err := s.Spawn(); err == nil {
		t.Fatal("want dup error for non-TCP listener")
	}
}

func TestSpawn_StartFail(t *testing.T) {
	s := newSup(t)
	s.start = func(*exec.Cmd) error { return errors.New("boom") }
	if _, err := s.Spawn(); err == nil {
		t.Fatal("want start error")
	}
}

// ---- Drain ----

func TestDrain_Success(t *testing.T) {
	// A long sleep we can SIGTERM. Its own pgroup so the signal can never
	// fan out beyond this specific child.
	cmd := exec.Command("/bin/sh", "-c", "sleep 30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	s := newSup(t)
	if err := s.Drain(cmd.Process); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	_, _ = cmd.Process.Wait()
}

func TestDrain_Error(t *testing.T) {
	s := newSup(t)
	cmd := exec.Command(trueBin(t))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	_, _ = cmd.Process.Wait() // reap so the next Signal errors
	if err := s.Drain(cmd.Process); err == nil {
		t.Fatal("want error signalling a finished process")
	}
}

// ---- SelfReexec ----

func TestSelfReexec_ListenerNotTCP(t *testing.T) {
	s := newSup(t)
	s.ln = fakeListener{}
	if err := s.SelfReexec(); err == nil {
		t.Fatal("want dup error for non-TCP listener")
	}
}

func TestSelfReexec_DupError(t *testing.T) {
	s := newSup(t)
	s.dup = func(int) (int, error) { return 0, errors.New("boom") }
	if err := s.SelfReexec(); err == nil {
		t.Fatal("want dup error")
	}
}

func TestSelfReexec_ExecError(t *testing.T) {
	s := newSup(t)
	s.execSelf = func(string, []string, []string) error { return errors.New("boom") }
	if err := s.SelfReexec(); err == nil {
		t.Fatal("want exec error")
	}
}

func TestSelfReexec_ExecSuccessReturnsNil(t *testing.T) {
	s := newSup(t)
	s.execSelf = func(string, []string, []string) error { return nil }
	if err := s.SelfReexec(); err != nil {
		t.Fatalf("want nil on exec success, got %v", err)
	}
}

// ---- WaitReady ----

func TestWaitReady_OK(t *testing.T) {
	ready := make(chan struct{})
	close(ready)
	if got := WaitReady(ready, make(chan struct{}), time.Second, nil); got != ReadyOK {
		t.Fatalf("got %v want ReadyOK", got)
	}
}

func TestWaitReady_Died(t *testing.T) {
	dead := make(chan struct{})
	close(dead)
	if got := WaitReady(make(chan struct{}), dead, time.Second, nil); got != ReadyDied {
		t.Fatalf("got %v want ReadyDied", got)
	}
}

func TestWaitReady_Timeout(t *testing.T) {
	fire := make(chan time.Time, 1)
	fire <- time.Now()
	after := func(time.Duration) <-chan time.Time { return fire }
	if got := WaitReady(make(chan struct{}), make(chan struct{}), time.Second, after); got != ReadyTimeout {
		t.Fatalf("got %v want ReadyTimeout", got)
	}
}

func TestReadyResultString(t *testing.T) {
	for r, want := range map[ReadyResult]string{ReadyOK: "ready", ReadyDied: "died", ReadyTimeout: "timeout"} {
		if got := r.String(); got != want {
			t.Fatalf("%d => %q want %q", r, got, want)
		}
	}
}

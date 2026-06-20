//go:build unix

package graceful

import (
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
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

func tcpListener(t *testing.T) *net.TCPListener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln.(*net.TCPListener)
}

func TestListen_ColdStart(t *testing.T) {
	t.Setenv(EnvFD, "")
	os.Unsetenv(EnvFD)
	ln, child, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	if child {
		t.Fatal("cold start must report child=false")
	}
}

func TestListen_ColdStartError(t *testing.T) {
	os.Unsetenv(EnvFD)
	_, _, err := Listen("missing-port")
	if err == nil {
		t.Fatal("want error on bad addr")
	}
}

func TestListen_ChildRecover(t *testing.T) {
	tl := tcpListener(t)
	f, err := tl.File()
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	defer f.Close()
	fd, err := syscall.Dup(int(f.Fd()))
	if err != nil {
		t.Fatalf("dup: %v", err)
	}
	t.Setenv(EnvFD, strconv.Itoa(fd))
	ln, child, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen child: %v", err)
	}
	defer ln.Close()
	if !child {
		t.Fatal("recover must report child=true")
	}
}

func TestListen_ChildBadFD(t *testing.T) {
	t.Setenv(EnvFD, "not-a-number")
	_, _, err := Listen("127.0.0.1:0")
	if err == nil {
		t.Fatal("want error on non-numeric fd")
	}
}

func TestListen_ChildFileListenerError(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "notsock")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	defer tmp.Close()
	fd, err := syscall.Dup(int(tmp.Fd()))
	if err != nil {
		t.Fatalf("dup: %v", err)
	}
	t.Setenv(EnvFD, strconv.Itoa(fd))
	_, _, err = Listen("127.0.0.1:0")
	if err == nil {
		t.Fatal("want error recovering listener from a regular file")
	}
}

func TestNotifyParentReady_NoParent(t *testing.T) {
	os.Unsetenv(EnvParentPID)
	if err := NotifyParentReady(); err != nil {
		t.Fatalf("no-parent must be no-op: %v", err)
	}
}

func TestNotifyParentReady_BadPID(t *testing.T) {
	t.Setenv(EnvParentPID, "xyz")
	if err := NotifyParentReady(); err == nil {
		t.Fatal("want error on bad pid")
	}
}

func TestNotifyParentReady_Signals(t *testing.T) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGUSR1)
	defer signal.Stop(ch)
	t.Setenv(EnvParentPID, strconv.Itoa(os.Getpid()))
	if err := NotifyParentReady(); err != nil {
		t.Fatalf("NotifyParentReady: %v", err)
	}
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("parent did not receive SIGUSR1")
	}
}

func TestNotifyParentReady_KillError(t *testing.T) {
	// A pid that almost certainly does not exist → syscall.Kill ESRCH.
	t.Setenv(EnvParentPID, "2147483646")
	if err := NotifyParentReady(); err == nil {
		t.Fatal("want error signalling a dead pid")
	}
}

func TestUpgrade_Success(t *testing.T) {
	m := New(Config{Listener: tcpListener(t), BinPath: "/usr/bin/true", Args: []string{}})
	// Fall back to /bin/true on systems that put it there.
	if _, err := os.Stat(m.binPath); err != nil {
		m.binPath = "/bin/true"
	}
	proc, err := m.Upgrade()
	if err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if proc == nil {
		t.Fatal("nil process")
	}
	_, _ = proc.Wait()
	// Second upgrade rejected while first in flight.
	if _, err := m.Upgrade(); !errors.Is(err, ErrUpgradeInFlight) {
		t.Fatalf("want ErrUpgradeInFlight, got %v", err)
	}
	m.Reset()
	if _, err := m.Upgrade(); err != nil {
		t.Fatalf("Upgrade after Reset: %v", err)
	}
	m.Reset()
}

func TestUpgrade_PreflightFail(t *testing.T) {
	m := New(Config{Listener: tcpListener(t), BinPath: "/no/such/binary-xyz"})
	if _, err := m.Upgrade(); err == nil {
		t.Fatal("want preflight error")
	}
}

func TestUpgrade_ListenerNotTCP(t *testing.T) {
	m := New(Config{Listener: fakeListener{}, BinPath: "/usr/bin/true"})
	if _, err := os.Stat(m.binPath); err != nil {
		m.binPath = "/bin/true"
	}
	if _, err := m.Upgrade(); err == nil {
		t.Fatal("want dup-listener error for non-TCP listener")
	}
}

func TestUpgrade_StartFail(t *testing.T) {
	m := New(Config{Listener: tcpListener(t), BinPath: "/usr/bin/true"})
	if _, err := os.Stat(m.binPath); err != nil {
		m.binPath = "/bin/true"
	}
	m.start = func(*exec.Cmd) error { return errors.New("boom") }
	if _, err := m.Upgrade(); err == nil {
		t.Fatal("want start error")
	}
}

func TestNew_Defaults(t *testing.T) {
	m := New(Config{Listener: tcpListener(t)})
	if m.binPath != os.Args[0] {
		t.Fatalf("binPath default = %q", m.binPath)
	}
	if m.args == nil || m.env == nil {
		t.Fatal("args/env defaults not filled")
	}
}

func TestDrain(t *testing.T) {
	ln := tcpListener(t)
	srv := &http.Server{Handler: http.NewServeMux()}
	go func() { _ = srv.Serve(ln) }()
	if err := Drain(srv); err != nil {
		t.Fatalf("Drain: %v", err)
	}
}

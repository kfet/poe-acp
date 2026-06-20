//go:build unix

package sdnotify

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// listenUnixgram binds a unixgram socket in a temp dir and returns its
// path plus the live connection to read datagrams from.
func listenUnixgram(t *testing.T) (string, *net.UnixConn) {
	t.Helper()
	// Keep the path short: the sockaddr_un sun_path limit is ~104 bytes
	// on darwin, and t.TempDir() paths can be long.
	dir, err := os.MkdirTemp("", "sdn")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "n.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen unixgram: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return path, conn
}

func TestNotify_NoSocket(t *testing.T) {
	os.Unsetenv("NOTIFY_SOCKET")
	sent, err := Notify("READY=1")
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if sent {
		t.Fatal("want sent=false with no NOTIFY_SOCKET")
	}
}

func TestNotify_Success(t *testing.T) {
	path, conn := listenUnixgram(t)
	t.Setenv("NOTIFY_SOCKET", path)

	sent, err := Notify("READY=1")
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if !sent {
		t.Fatal("want sent=true")
	}

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 256)
	n, _, err := conn.ReadFromUnix(buf)
	if err != nil {
		t.Fatalf("read datagram: %v", err)
	}
	if got := string(buf[:n]); got != "READY=1" {
		t.Fatalf("datagram = %q, want READY=1", got)
	}
}

func TestReady(t *testing.T) {
	path, conn := listenUnixgram(t)
	t.Setenv("NOTIFY_SOCKET", path)
	if _, err := Ready(); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 256)
	n, _, err := conn.ReadFromUnix(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(buf[:n]); got != "READY=1" {
		t.Fatalf("datagram = %q", got)
	}
}

func TestReadyMainPID(t *testing.T) {
	path, conn := listenUnixgram(t)
	t.Setenv("NOTIFY_SOCKET", path)
	if _, err := ReadyMainPID(4242); err != nil {
		t.Fatalf("ReadyMainPID: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 256)
	n, _, err := conn.ReadFromUnix(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(buf[:n]); got != "MAINPID=4242\nREADY=1" {
		t.Fatalf("datagram = %q", got)
	}
}

func TestNotify_DialError(t *testing.T) {
	// A path with no listening socket → connect(2) fails at dial.
	t.Setenv("NOTIFY_SOCKET", filepath.Join(t.TempDir(), "absent.sock"))
	sent, err := Notify("READY=1")
	if err == nil {
		t.Fatal("want dial error for absent socket")
	}
	if sent {
		t.Fatal("want sent=false on dial error")
	}
}

func TestNotify_WriteError(t *testing.T) {
	path, _ := listenUnixgram(t)
	t.Setenv("NOTIFY_SOCKET", path)
	// A datagram larger than the unix socket max message size makes the
	// connected write fail deterministically (EMSGSIZE), exercising the
	// post-dial write-error branch.
	sent, err := Notify(strings.Repeat("x", 1<<20))
	if err == nil {
		t.Fatal("want write error for oversized datagram")
	}
	if sent {
		t.Fatal("want sent=false on write error")
	}
}

func TestResolveAddr_Path(t *testing.T) {
	a := resolveAddr("/run/systemd/notify")
	if a.Name != "/run/systemd/notify" || a.Net != "unixgram" {
		t.Fatalf("path addr = %+v", a)
	}
}

func TestResolveAddr_Abstract(t *testing.T) {
	a := resolveAddr("@abstract")
	if a.Name != "\x00abstract" {
		t.Fatalf("abstract addr name = %q, want NUL-prefixed", a.Name)
	}
}

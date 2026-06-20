//go:build unix

// Package sdnotify implements the minimal subset of the sd_notify(3)
// protocol needed to make graceful restart survive under systemd, with
// no external dependencies.
//
// systemd tracks a Type=notify service by its MainPID. poe-acp's
// zero-downtime restart (see internal/graceful) forks a child, hands it
// the listener, and lets the old parent drain and exit. Without telling
// systemd, the parent IS the tracked MainPID: when it exits cleanly
// systemd considers the service stopped, tears down the cgroup, and
// kills the freshly-promoted child — a permanent outage after a reload.
//
// The fix is the MAINPID handshake: the child notifies systemd of the
// new MainPID (and READY) BEFORE the parent exits, so systemd never sees
// the tracked PID disappear without already knowing its successor. This
// requires NotifyAccess=all on the unit (so systemd accepts a notify
// from a process that is not yet the tracked main PID).
//
// All functions are no-ops returning (false, nil) when NOTIFY_SOCKET is
// unset, so launchd and bare-process deployments are unaffected.
package sdnotify

import (
	"fmt"
	"net"
	"os"
	"strings"
)

// resolveAddr converts a NOTIFY_SOCKET value into a unixgram address.
// A leading '@' selects the Linux abstract namespace, encoded on the
// wire as a leading NUL byte; otherwise socket is a filesystem path.
func resolveAddr(socket string) *net.UnixAddr {
	name := socket
	if strings.HasPrefix(socket, "@") {
		name = "\x00" + socket[1:]
	}
	return &net.UnixAddr{Name: name, Net: "unixgram"}
}

// Notify sends state — a newline-separated set of VAR=value assignments
// per the sd_notify(3) protocol — to the socket named by NOTIFY_SOCKET.
// It returns (false, nil) when NOTIFY_SOCKET is unset so non-systemd
// deployments are unaffected; the bool reports whether a datagram was
// actually sent.
func Notify(state string) (bool, error) {
	socket := os.Getenv("NOTIFY_SOCKET")
	if socket == "" {
		return false, nil
	}
	conn, err := net.DialUnix("unixgram", nil, resolveAddr(socket))
	if err != nil {
		return false, fmt.Errorf("sdnotify: dial %q: %w", socket, err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte(state)); err != nil {
		return false, fmt.Errorf("sdnotify: write: %w", err)
	}
	return true, nil
}

// Ready notifies systemd that startup is complete (READY=1). For a
// Type=notify unit the initial process must call this once it is
// listening, or systemd hangs in "activating" until the start timeout.
func Ready() (bool, error) {
	return Notify("READY=1")
}

// ReadyMainPID tells systemd the new main PID and that it is ready in a
// single datagram (MAINPID=<pid>\nREADY=1). A graceful-restart child
// calls this before signalling the old parent to drain, so systemd
// re-targets MainPID onto the successor and does not reap it when the
// parent exits. Requires NotifyAccess=all on the unit.
func ReadyMainPID(pid int) (bool, error) {
	return Notify(fmt.Sprintf("MAINPID=%d\nREADY=1", pid))
}

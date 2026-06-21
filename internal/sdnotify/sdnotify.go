//go:build unix

// Package sdnotify implements the minimal subset of the sd_notify(3)
// protocol needed to signal readiness under systemd, with no external
// dependencies.
//
// Under the master/worker supervisor model (see internal/supervisor) the
// supervisor S is the process systemd tracks as MainPID. S is stable
// across worker upgrades — it binds the socket once and never exits
// during a worker swap — so the v0.35.0 MAINPID re-point handshake is no
// longer needed. S simply sends READY=1 once its first worker is serving
// (Type=notify ordering), and NotifyAccess=all is no longer required.
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
// Type=notify unit the supervisor must call this once its first worker
// is serving, or systemd hangs in "activating" until the start timeout.
func Ready() (bool, error) {
	return Notify("READY=1")
}

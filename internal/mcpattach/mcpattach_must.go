package mcpattach

import "os"

// mustChmod tightens the freshly-created unix socket to owner-only. A
// chmod failure on a socket this process just created in its own
// runtime dir is not reachable on any POSIX FS we target; treat it as
// fatal rather than leaving a world-connectable socket. Excluded from
// coverage via the _must.go rule.
func mustChmod(path string) {
	if err := os.Chmod(path, 0o600); err != nil {
		panic("mcpattach: chmod socket: " + err.Error())
	}
}

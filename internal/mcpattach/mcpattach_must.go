package mcpattach

import (
	"crypto/rand"
	"os"
)

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

// mustRand fills b with cryptographically-random bytes. crypto/rand on
// the platforms we target does not fail; a failure here means the system
// CSPRNG is unavailable, which is fatal-grade — we must not mint a
// predictable auth token. Excluded from coverage via the _must.go rule.
func mustRand(b []byte) {
	if _, err := rand.Read(b); err != nil {
		panic("mcpattach: crypto/rand: " + err.Error())
	}
}

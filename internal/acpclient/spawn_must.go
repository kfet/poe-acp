// Package acpclient: defensive helpers for agent-spawn paths the
// production caller cannot trigger. Excluded from coverage via the
// `_must.go` suffix rule in .covignore.
package acpclient

// mustPipe panics if StdinPipe/StdoutPipe returns an error. exec.Cmd
// only fails to allocate a pipe when the kernel refuses an
// os.Pipe() call — not reachable in any of our tests, and the cmd
// has not been Started yet so there is nothing to clean up.
func mustPipe(err error, which string) {
	if err != nil {
		panic("acpclient: " + which + " pipe: " + err.Error())
	}
}

// mustTempDir panics if os.MkdirTemp fails. Only reachable when
// $TMPDIR is unwritable; every other test in this package proves
// the contrary in the same process.
func mustTempDir(err error) {
	if err != nil {
		panic("acpclient: probe mkdir tmp: " + err.Error())
	}
}

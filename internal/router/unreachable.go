// Package router: defensive helpers for paths the production caller
// cannot trigger. Excluded from coverage via /unreachable\.go: in
// .covignore.
package router

// mustOpen panics if err is non-nil. Used after the second OpenFile
// attempt against an os.Root: the first try uses the user-supplied
// name (which Root may reject for path components like ".."); the
// second uses a SHA-derived ASCII fallback that Root has no grounds
// to reject. Reaching this panic would mean the kernel rejected a
// fresh hash-derived filename in our own dedicated dir — not
// observed on any POSIX FS in practice.
func mustOpen(err error) {
	if err != nil {
		panic("router: fallback attachment OpenFile rejected: " + err.Error())
	}
}

// mustClose panics if Close on a freshly-created *os.File returns an
// error after io.Copy already succeeded. Not observed on any
// POSIX-compliant FS we test against; treating it as fatal keeps the
// caller branchless.
func mustClose(err error) {
	if err != nil {
		panic("router: attachment Close failed after successful copy: " + err.Error())
	}
}

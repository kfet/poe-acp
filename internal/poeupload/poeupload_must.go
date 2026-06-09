package poeupload

// Defensive helpers for multipart-writer error paths the production
// caller cannot trigger. Excluded from coverage via the `_must.go`
// rule in .covignore.

// mustNil panics if err is non-nil. Used for *multipart.Writer
// operations against an in-memory bytes.Buffer: CreateFormFile only
// fails on an invalid MIME boundary (we use the library default) and
// Close only fails if the underlying writer fails — a bytes.Buffer
// never does. Reaching either would indicate a stdlib invariant break,
// not a runtime condition a caller could provoke.
func mustNil(err error) {
	if err != nil {
		panic("poeupload: multipart writer error on in-memory buffer: " + err.Error())
	}
}

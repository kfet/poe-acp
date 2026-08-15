// Package install: defensive helpers for paths the production caller
// cannot trigger. Excluded from coverage via the `_must.go` suffix rule
// in .covignore.
package install

import "encoding/json"

// mustMarshalJSON marshals v and panics on error. json.Marshal only
// fails for unsupported types (channels, funcs, cyclic structures); the
// only values this package persists are a map[string]Pin and a
// []time.Time, both plain data. Panicking keeps writeJSON branchless.
func mustMarshalJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic("mustMarshalJSON: " + err.Error())
	}
	return b
}

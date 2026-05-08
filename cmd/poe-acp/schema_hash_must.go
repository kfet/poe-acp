// Package main: defensive helpers for paths the production caller
// cannot trigger. Excluded from coverage via the `_must.go` suffix
// rule in .covignore. Each helper carries a justifying comment.
package main

import "encoding/json"

// mustMarshalJSON marshals v and panics on error. json.Marshal only
// fails for unsupported types (channels, funcs, cyclic structures);
// every call site in this binary marshals plain data structs (e.g.
// poeproto.ParameterControls) where failure is impossible. Panicking
// keeps callers branchless.
func mustMarshalJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic("mustMarshalJSON: " + err.Error())
	}
	return b
}

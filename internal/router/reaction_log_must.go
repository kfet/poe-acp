// Package router: defensive helper for the reaction ledger encoder.
// Excluded from coverage via the `_must.go` suffix rule in .covignore.
package router

import "encoding/json"

// mustMarshalJSON encodes v as JSON and panics if encoding fails. Used
// for reactionRecord, a flat struct of strings: encoding/json has no
// failure mode for it (no channels, funcs, cycles or NaNs), so the
// error return is unreachable from any test and panicking keeps the
// ledger writer branchless.
func mustMarshalJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic("router: reaction record marshal failed: " + err.Error())
	}
	return b
}

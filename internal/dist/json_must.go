package dist

import (
	"encoding/json"
	"fmt"
)

// mustJSON marshals v for an atomic write. Every value this package
// writes (cacheFile, Status) is a concrete struct of strings, slices and
// maps with no channels, functions or NaNs, so encoding/json cannot fail
// on it; a failure here would mean the struct definitions changed into
// something unencodable, which is a programming error rather than a
// runtime condition a caller could handle.
func mustJSON(v any) []byte {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(fmt.Sprintf("dist: marshal %T: %v", v, err))
	}
	return append(b, '\n')
}

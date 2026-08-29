package httpsrv

import "encoding/json"

// mustMarshalCalls encodes the absorbed-answer wire form.
//
// The input is a []wireCall — a slice of a struct whose every field is an
// int or a string. encoding/json cannot fail on that: there is no
// channel, func, complex, cyclic pointer or failing MarshalJSON anywhere
// in the type, and it is constructed field-by-field by encodeCalls, so no
// test can drive the error branch. Panicking here keeps the caller free
// of an uncoverable `if err != nil`.
func mustMarshalCalls(w []wireCall) []byte {
	b, err := json.Marshal(w)
	if err != nil {
		panic("httpsrv: marshal absorbed answer: " + err.Error())
	}
	return b
}

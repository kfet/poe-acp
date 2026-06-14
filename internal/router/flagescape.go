package router

import (
	"strings"

	"github.com/kfet/poe-acp/internal/poeproto"
)

// flagEscaper makes a streamed assistant message safe to send by defusing
// reserved double-dash flag tokens (poeproto.EscapeReservedFlags) before
// they reach the user — Poe's chat client would otherwise reject any
// message containing one (see poeproto/reserved.go for why).
//
// The escaper holds back the trailing incomplete (whitespace-delimited)
// token so a reserved flag split across chunk boundaries is still caught:
// a reserved flag is always whitespace- or end-terminated, so emitting
// only up to the last whitespace guarantees every escaped token is whole.
type flagEscaper struct{ pending string }

// feed appends s and returns the escaped, emit-ready prefix (text up to
// and including the last whitespace). Any trailing partial token is held
// until the next feed or flush.
func (e *flagEscaper) feed(s string) string {
	e.pending += s
	i := strings.LastIndexAny(e.pending, " \t\r\n")
	if i < 0 {
		return "" // whole buffer is one unterminated token; hold it
	}
	emit := e.pending[:i+1]
	e.pending = e.pending[i+1:]
	return poeproto.EscapeReservedFlags(emit)
}

// flush returns the escaped remainder held by feed and clears it. Call at
// end of the message stream (turn end or before an interrupting thought).
func (e *flagEscaper) flush() string {
	s := e.pending
	e.pending = ""
	return poeproto.EscapeReservedFlags(s)
}

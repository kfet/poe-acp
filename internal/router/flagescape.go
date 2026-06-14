package router

import (
	"regexp"
	"strings"
)

// Poe's chat client parses double-dash flag tokens in message text and
// binds them to this bot's declared parameter_controls (see
// internal/paramctl: "model", "provider", "thinking", "hide_thinking",
// and per-provider "model_<provider>"). Each is a strict-enum drop_down,
// so a freeform value (or none) fails validation and Poe rejects the
// whole message *before it reaches the bot* — wedging the conversation
// every time it is sent, quoted, or re-submitted. The bot must therefore
// never emit such a token verbatim. We defuse it by inserting a
// zero-width space after the leading "--": the text reads identically but
// Poe no longer matches it as a reserved flag.
//
// The model_<provider> branch is matched generically (model_ + word
// chars) so the guard stays correct for any configured provider without
// re-deriving the live list.
var reservedFlagRe = regexp.MustCompile(`--(model_[A-Za-z0-9_]+|model|provider|hide_thinking|thinking)\b`)

const zeroWidthSpace = "\u200b"

// escapeReservedFlags inserts a zero-width space after the leading "--"
// of every reserved flag token in s. All tokens in s must be complete;
// use flagEscaper for streaming input where a token may span chunks.
func escapeReservedFlags(s string) string {
	if !strings.Contains(s, "--") {
		return s
	}
	return reservedFlagRe.ReplaceAllString(s, "--"+zeroWidthSpace+"${1}")
}

// flagEscaper applies escapeReservedFlags to a streamed message, holding
// back the trailing incomplete (whitespace-delimited) token so a reserved
// flag split across chunk boundaries is still caught. A reserved flag is
// always whitespace- or end-terminated, so emitting only up to the last
// whitespace guarantees every escaped token is whole.
type flagEscaper struct{ pending string }

// feed appends s and returns the escaped, emit-ready prefix (text up to
// and including the last whitespace). The trailing partial token, if any,
// is held until the next feed or flush.
func (e *flagEscaper) feed(s string) string {
	e.pending += s
	i := strings.LastIndexAny(e.pending, " \t\r\n")
	if i < 0 {
		return "" // whole buffer is one unterminated token; hold it
	}
	emit := e.pending[:i+1]
	e.pending = e.pending[i+1:]
	return escapeReservedFlags(emit)
}

// flush returns the escaped remainder held by feed and clears it. Call at
// end of the message stream (turn end or before an interrupting thought).
func (e *flagEscaper) flush() string {
	s := e.pending
	e.pending = ""
	return escapeReservedFlags(s)
}

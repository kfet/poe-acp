package poeproto

import "testing"

// "--host" is reserved only for bots that declare the Host dropdown, so
// the default escaper must leave it byte-identical.
func TestEscapeReservedFlags_HostUntouchedByDefault(t *testing.T) {
	in := "run `ssh --host zboxserver` and --model x"
	got := EscapeReservedFlags(in)
	if want := "run `ssh --host zboxserver` and --" + zeroWidthSpace + "model x"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestEscapeReservedFlagsWithHost(t *testing.T) {
	got := EscapeReservedFlagsWithHost("use --host boxy with --thinking high")
	want := "use --" + zeroWidthSpace + "host boxy with --" + zeroWidthSpace + "thinking high"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	// No "--" at all: fast path, unchanged.
	if got := EscapeReservedFlagsWithHost("plain host text"); got != "plain host text" {
		t.Fatalf("fast path changed text: %q", got)
	}
	// A longer word starting with "host" must not match.
	if got := EscapeReservedFlagsWithHost("--hostname x"); got != "--hostname x" {
		t.Fatalf("prefix over-match: %q", got)
	}
}

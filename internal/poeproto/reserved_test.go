package poeproto

import "testing"

func TestEscapeReservedFlags(t *testing.T) {
	const z = "\u200b"
	cases := []struct{ in, want string }{
		{"plain text", "plain text"},
		{"no dashes here", "no dashes here"},
		{"fir --model x", "fir --" + z + "model x"},
		{"--provider bifrost", "--" + z + "provider bifrost"},
		{"use --thinking high", "use --" + z + "thinking high"},
		{"--hide_thinking", "--" + z + "hide_thinking"},
		{"--model_anthropic pick", "--" + z + "model_anthropic pick"},
		{"both --provider a --model b", "both --" + z + "provider a --" + z + "model b"},
		{"--other --words --are fine", "--other --words --are fine"},     // undeclared: untouched
		{"-p ping single dash", "-p ping single dash"},                   // single dash: untouched
		{"<!--poe-attach path=\"x\"-->", "<!--poe-attach path=\"x\"-->"}, // directive untouched
	}
	for _, c := range cases {
		if got := EscapeReservedFlags(c.in); got != c.want {
			t.Errorf("EscapeReservedFlags(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

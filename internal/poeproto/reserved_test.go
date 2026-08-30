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
		{"--show_thinking", "--" + z + "show_thinking"},
		// Deprecated inbound alias: accepted as a parameter but never
		// declared, so Poe cannot bind the token — it must pass through
		// verbatim.
		{"--hide_thinking", "--hide_thinking"},
		{"--show_plans on", "--" + z + "show_plans on"},
		{"--show_tools off", "--" + z + "show_tools off"},
		{"--show_tool_details", "--" + z + "show_tool_details"},
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

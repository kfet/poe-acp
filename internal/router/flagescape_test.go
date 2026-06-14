package router

import "testing"

const zwsp = "\u200b"

func TestEscapeReservedFlags(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain text", "plain text"},
		{"no dashes here", "no dashes here"},
		{"fir --model x", "fir --" + zwsp + "model x"},
		{"--provider bifrost", "--" + zwsp + "provider bifrost"},
		{"use --thinking high", "use --" + zwsp + "thinking high"},
		{"--hide_thinking", "--" + zwsp + "hide_thinking"},
		{"--model_anthropic pick", "--" + zwsp + "model_anthropic pick"},
		{"both --provider a --model b", "both --" + zwsp + "provider a --" + zwsp + "model b"},
		{"--other --words --are fine", "--other --words --are fine"},     // undeclared: untouched
		{"-p ping single dash", "-p ping single dash"},                   // single dash: untouched
		{"<!--poe-attach path=\"x\"-->", "<!--poe-attach path=\"x\"-->"}, // directive untouched
	}
	for _, c := range cases {
		if got := escapeReservedFlags(c.in); got != c.want {
			t.Errorf("escapeReservedFlags(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFlagEscaper_StreamsAndHoldsPartialToken(t *testing.T) {
	var e flagEscaper
	// "--model" split across two feeds with no whitespace until the 2nd.
	if got := e.feed("fir --mod"); got != "fir " {
		t.Fatalf("feed1=%q want %q", got, "fir ")
	}
	if got := e.feed("el openai\n"); got != "--"+zwsp+"model openai\n" {
		t.Fatalf("feed2=%q", got)
	}
	if got := e.flush(); got != "" {
		t.Fatalf("flush=%q want empty", got)
	}
}

func TestFlagEscaper_HoldsWholeBufferWhenNoWhitespace(t *testing.T) {
	var e flagEscaper
	if got := e.feed("--provider"); got != "" {
		t.Fatalf("feed=%q want empty (held)", got)
	}
	// flush releases and escapes the held token.
	if got := e.flush(); got != "--"+zwsp+"provider" {
		t.Fatalf("flush=%q", got)
	}
}

func TestFlagEscaper_PlainFlushEmpty(t *testing.T) {
	var e flagEscaper
	if got := e.feed("hello world "); got != "hello world " {
		t.Fatalf("feed=%q", got)
	}
	if got := e.flush(); got != "" {
		t.Fatalf("flush=%q want empty", got)
	}
}

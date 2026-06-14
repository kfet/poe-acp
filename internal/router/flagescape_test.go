package router

import "testing"

const zwsp = "\u200b"

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

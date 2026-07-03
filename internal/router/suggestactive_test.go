package router

import (
	"io"
	"strings"
	"testing"
)

func TestSuggestActive_NoActiveTurn(t *testing.T) {
	r := &Router{active: map[string]activeTurn{}}
	if err := r.SuggestActive("nope", []string{"Yes"}); err == nil {
		t.Fatal("want error when no active turn")
	}
}

func TestSuggestActive_NoUsableReplies(t *testing.T) {
	r := &Router{active: map[string]activeTurn{}}
	r.setActiveTurn("c", &captureSink{}, t.TempDir())
	// all empty/whitespace → nothing usable
	if err := r.SuggestActive("c", []string{"", "   ", "\t"}); err == nil {
		t.Fatal("want error when no usable replies")
	}
}

func TestSuggestActive_Success(t *testing.T) {
	r := &Router{active: map[string]activeTurn{}}
	sink := &captureSink{}
	r.setActiveTurn("c", sink, t.TempDir())
	if err := r.SuggestActive("c", []string{"Yes", " No ", ""}); err != nil {
		t.Fatalf("SuggestActive: %v", err)
	}
	// empties dropped, surrounding whitespace trimmed
	if len(sink.replies) != 2 || sink.replies[0] != "Yes" || sink.replies[1] != "No" {
		t.Fatalf("replies = %v", sink.replies)
	}
}

func TestSuggestActive_CapsCountAndLength(t *testing.T) {
	r := &Router{active: map[string]activeTurn{}}
	sink := &captureSink{}
	r.setActiveTurn("c", sink, t.TempDir())
	long := strings.Repeat("x", maxSuggestedReplyRunes+20)
	// long first so it is processed (truncated) before the count cap trips.
	in := []string{long, "a", "b", "c", "d", "e", "f", "g"}
	if err := r.SuggestActive("c", in); err != nil {
		t.Fatalf("SuggestActive: %v", err)
	}
	if len(sink.replies) != maxSuggestedReplies {
		t.Fatalf("want capped to %d, got %d: %v", maxSuggestedReplies, len(sink.replies), sink.replies)
	}
	for _, rep := range sink.replies {
		if len([]rune(rep)) > maxSuggestedReplyRunes {
			t.Fatalf("reply over rune cap: %q (%d)", rep, len([]rune(rep)))
		}
	}
}

// errSuggestSink makes SuggestedReply fail to exercise the error path.
type errSuggestSink struct{ *captureSink }

func (errSuggestSink) SuggestedReply(string) error { return io.ErrClosedPipe }

func TestSuggestActive_SinkError(t *testing.T) {
	r := &Router{active: map[string]activeTurn{}}
	r.setActiveTurn("c", errSuggestSink{&captureSink{}}, t.TempDir())
	if err := r.SuggestActive("c", []string{"Yes"}); err == nil {
		t.Fatal("want error when sink.SuggestedReply fails")
	}
}

func TestDiscardSink_SuggestedReply(t *testing.T) {
	if err := (discardSink{convID: "c"}).SuggestedReply("x"); err != nil {
		t.Fatalf("discardSink.SuggestedReply: %v", err)
	}
}

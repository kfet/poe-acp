package httpsrv

import (
	"testing"
	"time"
)

// TestInflight_TracksAndReleases covers registration, oldest-first
// ordering (the drain log should name the longest-running casualty
// first), and release.
func TestInflight_TracksAndReleases(t *testing.T) {
	i := newInflight()
	if got := i.snapshot(); len(got) != 0 {
		t.Fatalf("empty registry => empty snapshot, got %+v", got)
	}
	relA := i.add("c-a", "u-a", "m-a")
	time.Sleep(2 * time.Millisecond) // ages differ; ordering is by age
	relB := i.add("c-b", "u-b", "m-b")

	got := i.snapshot()
	if len(got) != 2 || got[0].ConvID != "c-a" || got[1].ConvID != "c-b" {
		t.Fatalf("want [c-a c-b] oldest-first, got %+v", got)
	}
	if got[0].UserID != "u-a" || got[0].MessageID != "m-a" || got[0].Age <= 0 {
		t.Fatalf("entry not fully recorded: %+v", got[0])
	}

	relA()
	if got := i.snapshot(); len(got) != 1 || got[0].ConvID != "c-b" {
		t.Fatalf("release failed: %+v", got)
	}
	relB()
	if got := i.snapshot(); len(got) != 0 {
		t.Fatalf("want empty after all releases, got %+v", got)
	}
}

// TestHandler_InFlightEmptyWhenIdle: an idle worker reports nothing to
// abandon.
func TestHandler_InFlightEmptyWhenIdle(t *testing.T) {
	h := New(Config{})
	if got := h.InFlight(); len(got) != 0 {
		t.Fatalf("idle handler must report no streams, got %+v", got)
	}
}

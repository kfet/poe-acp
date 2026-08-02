package httpsrv

import (
	"testing"
	"time"
)

// TestSealInFlight_NoStreams: an idle worker has nothing to seal, and a
// non-positive timeout falls back to the default.
func TestSealInFlight_NoStreams(t *testing.T) {
	h := New(Config{})
	if n := h.SealInFlight(AbandonMessage, 0); n != 0 {
		t.Fatalf("idle handler must seal nothing, got %d", n)
	}
}

// TestSealInFlight_SkipsUnsealableAndReleased: a stream registered but
// not yet carrying a sink has no sealer, and a released entry cannot
// acquire one.
func TestSealInFlight_SkipsUnsealableAndReleased(t *testing.T) {
	h := New(Config{})
	_, relA := h.streams.add("c-a", "u-a", "m-a") // registered, no sink yet
	defer relA()

	id, relB := h.streams.add("c-b", "u-b", "m-b")
	relB()
	h.streams.setSeal(id, func(string) { t.Error("released stream must not be sealed") })

	if n := h.SealInFlight(AbandonMessage, time.Second); n != 0 {
		t.Fatalf("want 0 sealable streams, got %d", n)
	}
}

// TestSealInFlight_BoundedByTimeout: a stream whose seal is stuck (a
// dead reader applying backpressure) must not wedge the drain — the
// step returns on its own cap and the process goes on to force-close.
func TestSealInFlight_BoundedByTimeout(t *testing.T) {
	h := New(Config{})
	id, rel := h.streams.add("c-stuck", "u-1", "m-1")
	defer rel()

	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	h.streams.setSeal(id, func(string) {
		close(entered)
		<-release
	})

	returned := make(chan int, 1)
	go func() { returned <- h.SealInFlight(AbandonMessage, 10*time.Millisecond) }()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("seal never ran")
	}
	select {
	case n := <-returned:
		if n != 1 {
			t.Fatalf("want 1 stream reported, got %d", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SealInFlight is not bounded by its timeout")
	}
}

// TestSealInFlight_CompletesBeforeTimeout covers the fast path: every
// seal returns, so the step finishes on the done channel rather than on
// the cap.
func TestSealInFlight_CompletesBeforeTimeout(t *testing.T) {
	h := New(Config{})
	id, rel := h.streams.add("c-ok", "u-1", "m-1")
	defer rel()

	got := make(chan string, 1)
	h.streams.setSeal(id, func(text string) { got <- text })

	if n := h.SealInFlight(AbandonMessage, 5*time.Second); n != 1 {
		t.Fatalf("want 1 sealed, got %d", n)
	}
	select {
	case text := <-got:
		if text != AbandonMessage {
			t.Fatalf("seal text = %q want %q", text, AbandonMessage)
		}
	default:
		t.Fatal("seal did not run")
	}
}

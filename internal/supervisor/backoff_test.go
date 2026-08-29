package supervisor

import (
	"testing"
	"time"
)

// fixed returns a float01 seam that always yields f.
func fixed(f float64) func() float64 { return func() float64 { return f } }

func TestBackoffZeroValueUsesDefaults(t *testing.T) {
	b := &Backoff{float01: fixed(0)}
	if got := b.Next(); got != DefaultRespawnBase/2 {
		t.Fatalf("first delay = %s, want %s", got, DefaultRespawnBase/2)
	}
	if b.Attempts() != 1 {
		t.Fatalf("attempts = %d, want 1", b.Attempts())
	}
}

func TestBackoffDoublesAndCaps(t *testing.T) {
	b := &Backoff{Base: time.Second, Max: 8 * time.Second, float01: fixed(0)}
	want := []time.Duration{
		500 * time.Millisecond,
		time.Second,
		2 * time.Second,
		4 * time.Second,
		4 * time.Second, // capped at Max=8s => 8/2 with zero jitter
		4 * time.Second,
	}
	for i, w := range want {
		if got := b.Next(); got != w {
			t.Fatalf("delay[%d] = %s, want %s", i, got, w)
		}
	}
	if b.Attempts() != len(want) {
		t.Fatalf("attempts = %d, want %d", b.Attempts(), len(want))
	}
	b.Reset()
	if b.Attempts() != 0 {
		t.Fatalf("attempts after Reset = %d, want 0", b.Attempts())
	}
	if got := b.Next(); got != 500*time.Millisecond {
		t.Fatalf("delay after Reset = %s, want 500ms", got)
	}
}

// Jitter must keep every delay inside [raw/2, raw) so the schedule stays
// bounded by Max no matter what the RNG returns.
func TestBackoffJitterBounds(t *testing.T) {
	for _, f := range []float64{0, 0.5, 0.999999} {
		b := &Backoff{Base: time.Second, Max: time.Second, float01: fixed(f)}
		got := b.Next()
		if got < 500*time.Millisecond || got >= time.Second {
			t.Fatalf("f=%v: delay %s outside [500ms, 1s)", f, got)
		}
	}
	// Default RNG path (no seam): still in range.
	b := &Backoff{Base: time.Second, Max: time.Second}
	if got := b.Next(); got < 500*time.Millisecond || got >= time.Second {
		t.Fatalf("default rng: delay %s outside [500ms, 1s)", got)
	}
}

// Max below Base is operator error, not a crash: clamp to Base.
func TestBackoffMaxBelowBaseClamps(t *testing.T) {
	b := &Backoff{Base: 4 * time.Second, Max: time.Second, float01: fixed(0)}
	if got := b.Next(); got != 2*time.Second {
		t.Fatalf("delay = %s, want 2s (clamped to Base)", got)
	}
}

// A long outage must not overflow the shift: after many attempts the
// delay is still exactly the cap, never negative.
func TestBackoffLongOutageSaturates(t *testing.T) {
	b := &Backoff{Base: time.Second, Max: 60 * time.Second, float01: fixed(0)}
	for i := 0; i < 200; i++ {
		if got := b.Next(); got <= 0 || got > 60*time.Second {
			t.Fatalf("attempt %d: delay %s out of range", i, got)
		}
	}
	if got := b.Next(); got != 30*time.Second {
		t.Fatalf("saturated delay = %s, want 30s", got)
	}
}

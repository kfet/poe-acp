package supervisor

import (
	"math/rand/v2"
	"time"
)

// DefaultRespawnBase is the first respawn delay after a failed worker
// spawn. Short enough that the ordinary case — a worker that died for a
// transient reason and comes straight back — costs the user nothing.
const DefaultRespawnBase = time.Second

// DefaultRespawnMaxBackoff caps the respawn delay. The failure this
// bounds is "the agent's backing host is simply down": retrying every
// second forever burns ssh connects and fills the journal, while
// retrying every few minutes leaves the bot dead long after the host is
// back. A minute is the compromise, and it is the operator-visible
// default for `agent.restart.max_backoff`.
const DefaultRespawnMaxBackoff = 60 * time.Second

// Backoff is the supervisor's respawn schedule: exponential with equal
// jitter, capped. It exists so a spawn failure that repeats — a down
// remote host, a bad agent command — degrades into a slow retry loop
// instead of a hot loop or, worse, a Fatalf that takes the whole bot
// down with it.
//
// The zero value is usable: Base and Max fall back to
// DefaultRespawnBase / DefaultRespawnMaxBackoff. Not safe for
// concurrent use; the supervisor loop is single-threaded.
type Backoff struct {
	// Base is the first delay. <=0 means DefaultRespawnBase.
	Base time.Duration
	// Max caps the delay. <=0 means DefaultRespawnMaxBackoff. A Max
	// below Base clamps to Base.
	Max time.Duration

	// n is the number of Next calls since the last Reset.
	n int
	// float01 returns a number in [0,1). Test seam; nil means rand.
	float01 func() float64
}

// Attempts reports how many consecutive failures have been scheduled
// since the last Reset. Log it — "attempt 7, retrying in 60s" is the
// difference between a bot that looks dead and one that is visibly
// waiting for a host to come back.
func (b *Backoff) Attempts() int { return b.n }

// Reset returns the schedule to its first delay. Call it as soon as a
// worker is serving again.
func (b *Backoff) Reset() { b.n = 0 }

// Next returns the delay to wait before the next respawn attempt and
// advances the schedule. Delays follow base, 2*base, 4*base, ... capped
// at Max, each cut by equal jitter — the returned value is uniform in
// [raw/2, raw) — so several relays that lost the same remote host do not
// retry in lockstep.
func (b *Backoff) Next() time.Duration {
	base := b.Base
	if base <= 0 {
		base = DefaultRespawnBase
	}
	max := b.Max
	if max <= 0 {
		max = DefaultRespawnMaxBackoff
	}
	if max < base {
		max = base
	}
	raw := base
	// Shift by n, saturating at max. Guard the shift itself: 1<<63
	// overflows time.Duration, and n is unbounded in a long outage.
	for i := 0; i < b.n && raw < max; i++ {
		raw *= 2
	}
	if raw > max || raw <= 0 {
		raw = max
	}
	b.n++
	f := rand.Float64
	if b.float01 != nil {
		f = b.float01
	}
	return raw/2 + time.Duration(f()*float64(raw/2))
}

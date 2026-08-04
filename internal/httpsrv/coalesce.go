package httpsrv

import (
	"log"
	"time"
)

// Outbound flush coalescing — why this exists.
//
// poe-acp is a Poe *server bot*: it emits SSE frames to Poe's servers,
// which relay them to the Poe mobile app. The phone leg is radio, and
// radio energy is dominated by how often the modem WAKES, not by how
// many bytes it carries. One packet every 20–100ms is the pathological
// case: too frequent for LTE/5G cDRX micro-sleep (which needs ~100–320ms
// of quiet to engage) and too slow to be an efficient bulk transfer —
// roughly a watt of modem draw at near-zero throughput.
//
// The un-coalesced relay emits one `text` frame per ACP chunk, i.e.
// exactly that pattern. The fix belongs HERE, upstream, because
// coalescing is monotone: Poe's server cannot invent frames it was never
// given, so at most one frame per period out of the relay is at most one
// frame per period at the phone. The relay→Poe hop is wired datacenter
// traffic, so the added latency costs nothing energetically.
//
// Grid alignment is the part that matters once more than one bot is in
// play. The operator runs several bots on several hosts; if each stream
// coalesces on its own independent timer, four 3s streams produce a wake
// roughly every 750ms and cDRX still never engages — all of the cost,
// none of the benefit. Flushing at absolute wall-clock instants
// (multiples of the period since the epoch) needs no coordination
// protocol: the wall clock is already shared state, so every stream on
// every host lands in the same wake window.

// eagerFlushChunks and eagerFlushWindow bound the eager first-flush
// window at the start of a turn. Poe DROPS a new-conversation bot
// connection that sees only the preamble + `meta` (a non-content event)
// during the initial gap — the cold-start spinner exists solely to close
// that window (see sink.heartbeat). Coalescing must not reintroduce it,
// so the first chunks of a turn are written straight through, 1:1, until
// EITHER enough chunks have landed OR the window elapses. Grid cadence
// applies only after that. The cost is bounded: at most
// eagerFlushChunks extra frames per turn.
const (
	eagerFlushChunks = 30
	eagerFlushWindow = 1500 * time.Millisecond
)

// paragraphFlushMin is the buffer size above which a paragraph boundary
// ("\n\n") in the pending text triggers an early flush. Paragraph-at-a-
// time reads markedly better on a phone than token jitter, and a
// paragraph break is a natural pause in the answer, so it is worth one
// extra wake. The size floor keeps a burst of short lines (a list, a
// table) from degenerating back into per-chunk frames.
const paragraphFlushMin = 200

// nextFlushDelay returns how long to wait before the next coalesced
// flush.
//
// With grid alignment the deadline is the next absolute instant where
// UnixMilli is a multiple of the period, so independent streams — in
// this process or on another host entirely — converge on the same wake
// windows. Without it, the delay is simply one full period from now
// (per-stream timers; kept as an opt-out for single-bot deployments
// that would rather not have every stream fire simultaneously).
//
// The returned delay is always > 0: landing exactly on a grid instant
// yields a full period, never a zero-length timer that would spin.
func nextFlushDelay(now time.Time, period time.Duration, grid bool) time.Duration {
	if !grid {
		return period
	}
	ms := period.Milliseconds()
	if ms <= 0 {
		// Sub-millisecond periods have no grid to align to; fall back to
		// the plain period so the caller still makes progress.
		return period
	}
	rem := now.UnixMilli() % ms
	return time.Duration(ms-rem) * time.Millisecond
}

// frameStats is a snapshot of one stream's outbound frame accounting.
// Frames-per-turn is the direct proxy for phone radio wakeups, so this
// is the number the whole feature is judged on; BytesOut counts payload
// text bytes (the `text` / `replace_response` body), not SSE framing
// overhead.
type frameStats struct {
	TextFrames    int64
	ReplaceFrames int64
	OtherFrames   int64
	BytesOut      int64
}

// logFrameStats emits the per-turn frame accounting line. One line per
// turn, always on: the before/after ratio of text_frames+replace_frames
// IS the radio-wake ratio, so it must be readable straight out of a
// production log without a debug flag.
func logFrameStats(convID string, st frameStats, dur time.Duration) {
	log.Printf("FRAMESTATS conv=%s text_frames=%d replace_frames=%d bytes_out=%d dur=%s",
		convID, st.TextFrames, st.ReplaceFrames, st.BytesOut, dur.Round(time.Millisecond))
}

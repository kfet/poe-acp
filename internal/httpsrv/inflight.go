package httpsrv

// In-flight stream registry.
//
// The relay's drain is bounded (see internal/supervisor drain.go): when
// the deadline fires, whatever is still streaming is abandoned. That is
// only debuggable if the worker can say WHICH streams it abandoned — the
// production incident left nothing in the log but "State 'stop-sigterm'
// timed out". Every query handler registers itself here for the duration
// of its turn, and the drain path snapshots the registry to log the
// casualties.

import (
	"cmp"
	"slices"
	"sync"
	"time"
)

// DefaultSealTimeout bounds the whole graceful-abandon step on a
// force-cut drain. The process is exiting either way, and each seal is
// itself bounded by the SSE write deadline, so this is only the outer
// cap that stops one wedged writer delaying the force-close.
const DefaultSealTimeout = 3 * time.Second

// AbandonMessage is the user-visible text emitted on an abandoned
// stream. Short and legible: Poe renders it as an error bubble and,
// because the event carries allow_retry, may redrive the same
// message_id against the new worker.
const AbandonMessage = "relay restarted — retrying"

// StreamInfo describes one in-flight query stream at snapshot time.
type StreamInfo struct {
	// ConvID is the Poe conversation_id the stream belongs to.
	ConvID string
	// UserID is the Poe user_id that opened it.
	UserID string
	// MessageID is the Poe message_id of the request being served.
	MessageID string
	// Age is how long the stream had been running at snapshot time.
	Age time.Duration
}

// liveStream is a registry entry.
type liveStream struct {
	convID    string
	userID    string
	messageID string
	started   time.Time
	// seal terminates the stream at the protocol level (error + done).
	// It is nil between registration and the moment the handler has a
	// sink to seal — registration happens first so an abandoned stream
	// is nameable in the log even if it never got that far.
	seal func(text string)
}

// inflight is the concurrent registry of live query streams.
type inflight struct {
	mu   sync.Mutex
	seq  uint64
	live map[uint64]*liveStream
}

func newInflight() *inflight { return &inflight{live: make(map[uint64]*liveStream)} }

// add registers a stream and returns its id plus its release function.
// The release is idempotent-safe to defer directly.
func (i *inflight) add(convID, userID, messageID string) (id uint64, release func()) {
	i.mu.Lock()
	i.seq++
	id = i.seq
	i.live[id] = &liveStream{convID: convID, userID: userID, messageID: messageID, started: time.Now()}
	i.mu.Unlock()
	return id, func() {
		i.mu.Lock()
		delete(i.live, id)
		i.mu.Unlock()
	}
}

// setSeal attaches the protocol-level sealer for an already-registered
// stream. A no-op if the entry has already been released.
func (i *inflight) setSeal(id uint64, seal func(text string)) {
	i.mu.Lock()
	if s, ok := i.live[id]; ok {
		s.seal = seal
	}
	i.mu.Unlock()
}

// sealers snapshots the sealers of every live stream that has one.
func (i *inflight) sealers() []func(string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	out := make([]func(string), 0, len(i.live))
	for _, s := range i.live {
		if s.seal != nil {
			out = append(out, s.seal)
		}
	}
	return out
}

// snapshot returns the live streams, oldest first.
func (i *inflight) snapshot() []StreamInfo {
	now := time.Now()
	i.mu.Lock()
	out := make([]StreamInfo, 0, len(i.live))
	for _, s := range i.live {
		out = append(out, StreamInfo{
			ConvID:    s.convID,
			UserID:    s.userID,
			MessageID: s.messageID,
			Age:       now.Sub(s.started),
		})
	}
	i.mu.Unlock()
	slices.SortFunc(out, func(a, b StreamInfo) int { return cmp.Compare(b.Age, a.Age) })
	return out
}

// InFlight returns the query streams currently being served, oldest
// first. Used by the worker's bounded-drain path to report exactly which
// streams a force-cut drain abandoned.
func (h *Handler) InFlight() []StreamInfo { return h.streams.snapshot() }

// SealInFlight terminates every in-flight stream at the Poe protocol
// level: an `error` event (which carries allow_retry) followed by the
// terminal `done`, flushed. It returns the number of streams it sealed.
//
// It exists for the force-cut drain path: without it, srv.Close() cuts
// the SSE connection mid-stream, Poe sees a truncated response and
// renders a red transport error whose retry is at its discretion. With
// it Poe sees a well-formed terminal error with explicit retry
// permission and may redrive the same message_id against the new
// worker, which resumes the same fir session by conversation cwd — so
// the CHAT stays continuous even though the turn itself is re-run.
//
// Sealing is idempotent with the handler's own finalization backstop:
// both go through orderedWriter, whose `closed` flag makes the second
// `done` a no-op, so no stream can ever emit `done` twice. Each seal
// runs on its own goroutine (streams live on other goroutines and a
// wire write is bounded only by the SSE write deadline) and the whole
// step is capped by timeout — a stalled reader must not wedge the
// drain. timeout <= 0 means DefaultSealTimeout.
func (h *Handler) SealInFlight(text string, timeout time.Duration) int {
	if timeout <= 0 {
		timeout = DefaultSealTimeout
	}
	seals := h.streams.sealers()
	if len(seals) == 0 {
		return 0
	}
	var wg sync.WaitGroup
	for _, seal := range seals {
		wg.Add(1)
		go func() {
			defer wg.Done()
			seal(text)
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
	}
	return len(seals)
}

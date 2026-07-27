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
}

// inflight is the concurrent registry of live query streams.
type inflight struct {
	mu   sync.Mutex
	seq  uint64
	live map[uint64]liveStream
}

func newInflight() *inflight { return &inflight{live: make(map[uint64]liveStream)} }

// add registers a stream and returns its release function. The release
// is idempotent-safe to defer directly.
func (i *inflight) add(convID, userID, messageID string) (release func()) {
	i.mu.Lock()
	i.seq++
	id := i.seq
	i.live[id] = liveStream{convID: convID, userID: userID, messageID: messageID, started: time.Now()}
	i.mu.Unlock()
	return func() {
		i.mu.Lock()
		delete(i.live, id)
		i.mu.Unlock()
	}
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

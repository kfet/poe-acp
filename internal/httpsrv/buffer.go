package httpsrv

import (
	"sync"
	"time"

	"github.com/kfet/poe-acp/internal/router"
)

// recOp identifies a recorded ChunkSink call so a completed turn can be
// replayed verbatim onto a fresh sink when Poe redrives a query whose
// original response was absorbed (see handler.go's gated turn-decouple).
type recOp int

const (
	opText recOp = iota
	opReplace
	opFile
	opError
	opDone
	opFirstChunk
	opSetProviderEmoji
	opSetStatus
)

// recCall is one captured ChunkSink call. Up to four string args cover
// the widest method (File: url, contentType, name, inlineRef).
type recCall struct {
	op             recOp
	s1, s2, s3, s4 string
}

// answerRecorder is a ChunkSink that records every call (for later
// replay) while forwarding to an inner sink. It is goroutine-safe: the
// router drives Text/FirstChunk/SetStatus from the drain goroutine and
// SetProviderEmoji/Done/Error/Replace from the runner goroutine.
type answerRecorder struct {
	inner router.ChunkSink
	mu    sync.Mutex
	calls []recCall
}

func (a *answerRecorder) record(c recCall) {
	a.mu.Lock()
	a.calls = append(a.calls, c)
	a.mu.Unlock()
}

func (a *answerRecorder) Text(s string) error {
	a.record(recCall{op: opText, s1: s})
	return a.inner.Text(s)
}

func (a *answerRecorder) Replace(s string) error {
	a.record(recCall{op: opReplace, s1: s})
	return a.inner.Replace(s)
}

func (a *answerRecorder) File(url, contentType, name, inlineRef string) error {
	a.record(recCall{op: opFile, s1: url, s2: contentType, s3: name, s4: inlineRef})
	return a.inner.File(url, contentType, name, inlineRef)
}

func (a *answerRecorder) Error(text, errorType string) error {
	a.record(recCall{op: opError, s1: text, s2: errorType})
	return a.inner.Error(text, errorType)
}

func (a *answerRecorder) Done() error {
	a.record(recCall{op: opDone})
	return a.inner.Done()
}

func (a *answerRecorder) FirstChunk() {
	a.record(recCall{op: opFirstChunk})
	a.inner.FirstChunk()
}

func (a *answerRecorder) SetProviderEmoji(emoji string) {
	a.record(recCall{op: opSetProviderEmoji, s1: emoji})
	a.inner.SetProviderEmoji(emoji)
}

func (a *answerRecorder) SetStatus(mood, plan string) {
	a.record(recCall{op: opSetStatus, s1: mood, s2: plan})
	a.inner.SetStatus(mood, plan)
}

// ToolActivity is transient liveness (wedge-clock reset + spinner
// label), not user-visible content, so it is forwarded but NOT recorded:
// a replayed answer is a completed turn where liveness is moot.
func (a *answerRecorder) ToolActivity(label string) {
	a.inner.ToolActivity(label)
}

// snapshot returns a copy of the recorded calls, safe to retain after
// the turn ends.
func (a *answerRecorder) snapshot() []recCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]recCall, len(a.calls))
	copy(out, a.calls)
	return out
}

// replay applies a recorded call sequence onto sink, reconstructing the
// exact user-visible stream the original (absorbed) turn produced. IO
// errors are swallowed: a broken redrive connection is no worse than the
// original drop, and there is nothing further to do.
func replay(calls []recCall, sink router.ChunkSink) {
	for _, c := range calls {
		switch c.op {
		case opText:
			_ = sink.Text(c.s1)
		case opReplace:
			_ = sink.Replace(c.s1)
		case opFile:
			_ = sink.File(c.s1, c.s2, c.s3, c.s4)
		case opError:
			_ = sink.Error(c.s1, c.s2)
		case opDone:
			_ = sink.Done()
		case opFirstChunk:
			sink.FirstChunk()
		case opSetProviderEmoji:
			sink.SetProviderEmoji(c.s1)
		case opSetStatus:
			sink.SetStatus(c.s1, c.s2)
		}
	}
}

// defaultAnswerBufferMax bounds the number of buffered answers held at
// once, so a flood of distinct absorbed turns can't grow memory without
// limit. On overflow the entry with the earliest expiry is dropped.
const defaultAnswerBufferMax = 4096

// answerBuffer holds completed-but-undelivered turn outputs keyed by
// conv+message_id, so a Poe redrive of a query whose original response
// was absorbed (client dropped pre-output) is served from the buffer
// instead of re-running the agent. Entries are evicted on take (served
// once) and on TTL expiry; the map is also capped (defaultAnswerBufferMax).
type answerBuffer struct {
	mu         sync.Mutex
	ttl        time.Duration
	maxEntries int
	now        func() time.Time
	m          map[string]bufEntry
}

type bufEntry struct {
	calls  []recCall
	expiry time.Time
}

func newAnswerBuffer(ttl time.Duration) *answerBuffer {
	return &answerBuffer{
		ttl:        ttl,
		maxEntries: defaultAnswerBufferMax,
		now:        time.Now,
		m:          make(map[string]bufEntry),
	}
}

// put stores calls under key with a fresh TTL. Expired entries are swept
// first; if the map is still at capacity after the sweep, the entry with
// the earliest expiry is evicted to make room.
func (b *answerBuffer) put(key string, calls []recCall) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sweepLocked()
	if _, exists := b.m[key]; !exists && len(b.m) >= b.maxEntries {
		b.evictOldestLocked()
	}
	b.m[key] = bufEntry{calls: calls, expiry: b.now().Add(b.ttl)}
}

// take returns and removes the buffered answer for key, if present and
// not expired.
func (b *answerBuffer) take(key string) ([]recCall, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.m[key]
	if !ok {
		return nil, false
	}
	delete(b.m, key)
	if !e.expiry.After(b.now()) {
		return nil, false
	}
	return e.calls, true
}

// sweepLocked removes all expired entries. Caller holds b.mu.
func (b *answerBuffer) sweepLocked() {
	now := b.now()
	for k, e := range b.m {
		if !e.expiry.After(now) {
			delete(b.m, k)
		}
	}
}

// evictOldestLocked drops the entry with the earliest expiry. Caller
// holds b.mu and has ensured the map is non-empty.
func (b *answerBuffer) evictOldestLocked() {
	var oldestKey string
	var oldestExp time.Time
	first := true
	for k, e := range b.m {
		if first || e.expiry.Before(oldestExp) {
			oldestKey, oldestExp, first = k, e.expiry, false
		}
	}
	delete(b.m, oldestKey)
}

// answerKey builds the buffer key for a conv + latest user message_id.
// Returns "" when the message id is empty (un-keyable: never buffered or
// served from buffer).
func answerKey(convID, messageID string) string {
	if messageID == "" {
		return ""
	}
	return convID + "\x00" + messageID
}

package httpsrv

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kfet/poe-acp/internal/router"
	"github.com/kfet/poe-acp/internal/statusline"
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
	opSetModelInfo
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
// SetModelInfo/Done/Error/Replace from the runner goroutine.
type answerRecorder struct {
	inner router.ChunkSink
	mu    sync.Mutex
	calls []recCall
	// token is the router's id for the turn this recorder is bound to,
	// set once via SetTurnToken just before Agent.Prompt. The disconnect
	// watcher passes it to Router.CancelTurn so a late-firing watcher can
	// only ever cancel ITS OWN turn, never the follow-up that replaced it.
	// Zero until the turn actually starts (still queued, or never ran).
	token atomic.Uint64
}

// SetTurnToken implements router.TurnTokener.
func (a *answerRecorder) SetTurnToken(t uint64) { a.token.Store(t) }

// turnToken returns the bound turn id, or 0 if the turn never started.
func (a *answerRecorder) turnToken() uint64 { return a.token.Load() }

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

func (a *answerRecorder) SetModelInfo(emoji, model string) {
	a.record(recCall{op: opSetModelInfo, s1: emoji, s2: model})
	a.inner.SetModelInfo(emoji, model)
}

func (a *answerRecorder) SetStatus(mood, plan string) {
	a.record(recCall{op: opSetStatus, s1: mood, s2: plan})
	a.inner.SetStatus(mood, plan)
}

// ToolActivity is transient liveness (wedge-clock reset + spinner
// label), not user-visible content, so it is forwarded but NOT recorded:
// a replayed answer is a completed turn where liveness is moot. The
// durable per-tool_call body line is a plain Text call and IS recorded.
func (a *answerRecorder) ToolActivity(label string) {
	a.inner.ToolActivity(label)
}

// SetPlan is transient too — the checklist lives only in the keepalive
// frame, which a replayed (already-complete) answer never shows. Forward
// for liveness parity, don't record.
func (a *answerRecorder) SetPlan(entries []statusline.PlanEntry) {
	a.inner.SetPlan(entries)
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
//
// The status FOOTER is deliberately absent from the recording: it is
// produced inside sink.Done, below this recorder, so it was never an
// opText. Replaying opSetModelInfo / opSetStatus and then opDone makes
// the fresh sink regenerate the identical footer from the replayed
// status — recording it as text as well would emit it twice.
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
		case opSetModelInfo:
			sink.SetModelInfo(c.s1, c.s2)
		case opSetStatus:
			sink.SetStatus(c.s1, c.s2)
		}
	}
}

// defaultAnswerBufferMax bounds the number of buffered answers held at
// once, so a flood of distinct absorbed turns can't grow memory without
// limit. On overflow the entry with the earliest expiry is dropped.
const defaultAnswerBufferMax = 4096

// defaultAbsorbPollInterval is how often a redrive that is WAITING on an
// in-flight absorbed turn (possibly owned by a retiring worker
// generation) re-checks for the published answer. Short enough that the
// hand-off is imperceptible next to a turn measured in tens of seconds,
// long enough to be free.
const defaultAbsorbPollInterval = 250 * time.Millisecond

// answerBuffer holds completed-but-undelivered turn outputs keyed by
// conv+message_id, so a Poe redrive of a query whose original response
// was absorbed (client dropped pre-output) is served from the buffer
// instead of re-running the agent. Entries are evicted on take (served
// once) and on TTL expiry; the map is also capped (defaultAnswerBufferMax).
//
// The map is worker-process memory, which is not enough on its own: a
// SIGHUP swap retires the worker holding it while the redrive lands on
// the new generation. `store`, when configured, is the on-disk mirror
// that survives a worker generation AND lets a redrive wait on a turn
// that is still running elsewhere (see answerstore.go and waitPending).
// A nil store is memory-only — exactly the pre-existing behaviour.
type answerBuffer struct {
	mu         sync.Mutex
	ttl        time.Duration
	maxEntries int
	now        func() time.Time
	m          map[string]bufEntry
	store      *answerStore
	// pollInterval is the waitPending poll cadence; a per-instance field
	// so tests can tighten it without touching a shared global.
	pollInterval time.Duration
	// waitTickHook is a test-only seam fired at the top of every
	// waitPending poll cycle, so a test can change the store's state
	// exactly between two polls instead of racing the loop's entry.
	waitTickHook func()
}

type bufEntry struct {
	calls  []recCall
	expiry time.Time
}

func newAnswerBuffer(ttl time.Duration, store *answerStore) *answerBuffer {
	return &answerBuffer{
		ttl:          ttl,
		maxEntries:   defaultAnswerBufferMax,
		now:          time.Now,
		m:            make(map[string]bufEntry),
		store:        store,
		pollInterval: defaultAbsorbPollInterval,
	}
}

// put stores calls under key, in memory and (when configured) on disk.
func (b *answerBuffer) put(key string, calls []recCall) {
	b.putMem(key, calls)
	if b.store != nil {
		b.store.put(key, calls)
	}
}

// putMem stores calls under key with a fresh TTL. Expired entries are
// swept first; if the map is still at capacity after the sweep, the entry
// with the earliest expiry is evicted to make room.
func (b *answerBuffer) putMem(key string, calls []recCall) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sweepLocked()
	if _, exists := b.m[key]; !exists && len(b.m) >= b.maxEntries {
		b.evictOldestLocked()
	}
	b.m[key] = bufEntry{calls: calls, expiry: b.now().Add(b.ttl)}
}

// take returns and removes the buffered answer for key, if present and
// not expired. Memory first, then the cross-generation disk store: an
// answer published by a RETIRING worker is only ever on disk here.
//
// put writes BOTH copies, so a memory hit must also retire the disk one
// — otherwise a later redrive, on this worker or the next generation,
// claims the leftover file and the same answer is served twice. That
// closes the sequential case; two SIMULTANEOUS redrives of one
// message_id, one hitting memory and one claiming disk in the instant
// before the unlink lands, could still each serve once — which requires
// Poe to redrive the same message twice concurrently, and costs a
// duplicate replay rather than a duplicate agent run.
func (b *answerBuffer) take(key string) ([]recCall, bool) {
	if calls, ok := b.takeMem(key); ok {
		if b.store != nil {
			b.store.discard(key)
		}
		return calls, true
	}
	if b.store == nil {
		return nil, false
	}
	return b.store.claim(key)
}

// takeMem is the in-process half of take.
func (b *answerBuffer) takeMem(key string) ([]recCall, bool) {
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

// markPending publishes "an absorbed turn owns this key and is still
// running", at the moment the disconnect watcher latches its absorb
// decision. Without it a redrive arriving mid-turn — the common case,
// since Poe redrives seconds after the drop while the turn runs for
// minutes — would see nothing and re-run from scratch.
//
// It doubles as the marker REFRESH: watchIdle re-marks on every tick for
// as long as the turn is demonstrably alive, which is what decouples the
// marker's lifetime from AnswerTTL without pinning a waiter behind a
// wedged or killed owner. See answerstore.go's package comment.
func (b *answerBuffer) markPending(key string) {
	if key != "" && b.store != nil {
		b.store.markPending(key)
	}
}

// waitOutcome is why waitPending stopped. The three arms mean genuinely
// different things to the caller, and collapsing them is what caused a
// redrive whose own client went away to start a DUPLICATE of a turn
// already running elsewhere.
type waitOutcome int

const (
	// waitMiss: nobody owns this key (no marker, or it expired or was
	// cleared without an answer). Proceed exactly as before the store
	// existed — run the turn.
	waitMiss waitOutcome = iota
	// waitServed: the owner published; the answer is returned.
	waitServed
	// waitAborted: an owner demonstrably holds this turn, but OUR client
	// went away while we waited. Running it again would be a second agent
	// invocation — with real side effects — of a turn already in flight.
	// The owner's answer stays buffered for the next redrive.
	waitAborted
)

// waitPending blocks until the absorbed turn that owns key publishes its
// answer. It returns waitMiss immediately when no live pending marker
// exists, and gives up when the marker is cleared or expires, or when
// this request's own client goes away — every exit is bounded, so a
// redrive can never wedge on it.
//
// The marker IS refreshed while the owning turn is alive (watchIdle), so
// this wait tracks a genuinely long turn instead of timing out at
// AnswerTTL. A wedged owner stops refreshing and the marker expires.
func (b *answerBuffer) waitPending(ctx context.Context, key string) ([]recCall, waitOutcome) {
	if key == "" || b.store == nil || !b.store.pendingLive(key) {
		return nil, waitMiss
	}
	t := time.NewTicker(b.pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, waitAborted
		case <-t.C:
			if b.waitTickHook != nil {
				b.waitTickHook()
			}
			if calls, ok := b.take(key); ok {
				return calls, waitServed
			}
			if !b.store.pendingLive(key) {
				return nil, waitMiss
			}
		}
	}
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

package httpsrv

// On-disk half of the absorbed-answer buffer.
//
// WHY THIS EXISTS
//
// `answerBuffer` alone is process memory. A SIGHUP worker swap retires
// the worker that holds it while a NEW generation starts taking every
// request, so a redrive of an absorbed turn lands on a worker whose map
// is empty and the whole turn re-runs. Worse, in the production incident
// (kopione 2026-08-28) the redrive arrived 45s BEFORE the absorbed turn
// finished — so there was no answer to hand over at any point, in any
// worker. Two things are therefore needed, not one:
//
//  1. a store both generations can see (this file), and
//  2. a PENDING marker published at the absorb latch — not at turn
//     completion — so a redrive that arrives mid-flight knows an answer
//     is coming and waits for it instead of re-prompting.
//
// LAYOUT
//
// One flat directory, `<state>/absorbed/`. Names are
// `<sha256(key)>` plus a suffix; hashing sidesteps any filename encoding
// question for arbitrary Poe conversation/message ids.
//
//	<h>.a            the completed answer (JSON recCall list)
//	<h>.p            pending marker, zero bytes — "a turn owns this key"
//	<h>.a.t<pid>-<n> transient write buffer (tmp+rename)
//	<h>.c<pid>-<n>   transient claim (rename target, see claim)
//
// EXPIRY is mtime + a TTL for every file; nothing carries a timestamp in
// its payload, so a sweep is pure readdir + Info(), needs no reads and no
// decode, and tests drive every expiry branch with os.Chtimes instead of
// sleeping.
//
// The two TTLs are NOT the same quantity and must not be conflated:
//
//   - an ANSWER expires after AnswerTTL — how long a finished answer
//     waits to be redriven.
//   - a PENDING marker expires after IdleWriteTimeout — how long a
//     WAITER should believe the owner is alive. That is the same
//     "maximum tolerable silence" the owner itself uses to declare its
//     own turn wedged, just observed from the other side.
//
// A turn legitimately runs for tens of minutes (hence the 30m
// -swap-drain-deadline), so a marker bounded by AnswerTTL would expire
// mid-flight and drop the waiter back into a re-run on exactly the long
// turns where that costs most. Instead the OWNER republishes its marker
// from watchTurn on every tick (IdleWriteTimeout/4, so four ticks of
// margin) for as long as the turn is alive. Alive is not judged here:
// the loop simply runs until acp-kit's liveness watcher cuts the turn,
// so it is bounded by exactly the condition that declares a wedge. A
// wedged turn therefore stops refreshing and its marker expires; so does
// a killed worker's.
//
// TAKE-ONCE across generations is rename(2): a taker renames `<h>.a` to a
// private claim name. rename is atomic within a directory, so of two
// workers racing exactly one succeeds and the loser gets ENOENT — which
// is indistinguishable from "no answer", the correct fallback. The claim
// file is read and unlinked immediately; if the taker dies first, the
// orphan is TTL-swept like anything else.
//
// A torn or garbage payload is a miss, never fatal: the entry is dropped
// and the caller re-runs the turn, i.e. exactly today's behaviour.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	kitlog "github.com/kfet/acp-kit/log"
)

const (
	// sufAnswer marks a completed, claimable answer. Every file in the
	// directory — this one included — counts against the entry bound and
	// expires on the TTL its suffix selects (see ttlFor).
	sufAnswer = ".a"
	// sufPending marks "an absorbed turn owns this key and is still
	// running". Zero bytes — its existence and mtime are the payload.
	sufPending = ".p"
	// sufTmp prefixes the tmp file of an atomic write.
	sufTmp = ".t"
	// sufClaim prefixes a taker's private rename target.
	sufClaim = ".c"
)

// answerStore is the cross-worker-generation, on-disk backing for
// answerBuffer. A nil *answerStore is never used: callers hold
// `store *answerStore` and check it for nil, which is how "no state dir
// configured" degrades to today's memory-only behaviour.
type answerStore struct {
	dir string
	// warnOnce surfaces the FIRST write failure at WARN. A store whose
	// dir exists but is unwritable (disk full, quota, perms flipped)
	// would otherwise degrade every put and every marker refresh
	// silently at the default log level — and a put that fails is
	// precisely the event that recreates the incident this store exists
	// to prevent. Refreshes fire every IdleWriteTimeout/4, so only the
	// first is loud; the rest stay at debug.
	warnOnce sync.Once
	// ttl bounds a completed answer (AnswerTTL).
	ttl time.Duration
	// pendingTTL bounds a pending marker between refreshes
	// (IdleWriteTimeout). See the package comment above for why these are
	// different quantities.
	pendingTTL time.Duration
	max        int
	// seq disambiguates this process's transient filenames so two
	// concurrent puts (or takes) of the same key never collide.
	seq atomic.Uint64
}

// newAnswerStore prepares dir and sweeps it. An empty dir, or a dir that
// cannot be created, yields nil — the caller then runs memory-only.
// Callers MUST pass already-defaulted durations: a zero pendingTTL would
// make every marker instantly dead and silently disable the wait, with
// nothing in the log to say so.
func newAnswerStore(dir string, ttl, pendingTTL time.Duration) *answerStore {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("WARN absorbed-answer store disabled: mkdir %s: %v", dir, err)
		return nil
	}
	s := &answerStore{dir: dir, ttl: ttl, pendingTTL: pendingTTL, max: defaultAnswerBufferMax}
	s.sweep()
	return s
}

// path returns the on-disk path for key with the given suffix.
func (s *answerStore) path(key, suffix string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(s.dir, hex.EncodeToString(sum[:])+suffix)
}

// put writes the completed answer and clears the pending marker. The
// order matters: the answer becomes visible BEFORE the marker goes away,
// so a waiter polling (claim, then pending) can never observe a window
// where neither exists.
//
// Every put sweeps first. Growth only happens here, so a sweep on every
// put plus one at construction bounds the directory without a background
// goroutine to own, cancel and leak.
func (s *answerStore) put(key string, calls []recCall) {
	s.sweep()
	s.writeAtomic(s.path(key, sufAnswer), encodeCalls(calls))
	s.abandon(key)
}

// markPending publishes "an absorbed turn owns this key and is running",
// at the instant the disconnect watcher latches the absorb decision. A
// redrive landing on ANY worker generation before the turn finishes sees
// this and waits (see answerBuffer.waitPending) instead of re-prompting.
//
// It is also the REFRESH: re-marking rewrites the zero-byte file via
// tmp+rename, which resets mtime and so extends the marker by another
// pendingTTL. Doing it this way rather than with os.Chtimes means a
// marker that was swept, or deleted by an operator poking at the state
// dir, is simply recreated instead of leaving the owner refreshing a file
// that no longer exists.
func (s *answerStore) markPending(key string) {
	s.writeAtomic(s.path(key, sufPending), nil)
}

// abandon clears the pending marker, releasing any waiter. put calls it
// once the answer is visible; nothing else does in production.
func (s *answerStore) abandon(key string) {
	_ = os.Remove(s.path(key, sufPending))
}

// discard drops the on-disk answer without reading it. It exists so a
// redrive served from the OWNER's memory copy also retires the disk copy:
// put writes both, and take-once has to hold across the pair, not just
// within each half.
func (s *answerStore) discard(key string) {
	_ = os.Remove(s.path(key, sufAnswer))
}

// pendingLive reports whether an unexpired pending marker exists — i.e.
// whether some worker generation last claimed this turn alive within
// pendingTTL.
func (s *answerStore) pendingLive(key string) bool {
	fi, err := os.Stat(s.path(key, sufPending))
	return err == nil && s.live(fi.ModTime(), s.pendingTTL)
}

// claim atomically takes the answer for key, if one is present and
// unexpired. The rename IS the claim: exactly one racing taker wins.
func (s *answerStore) claim(key string) ([]recCall, bool) {
	src := s.path(key, sufAnswer)
	dst := fmt.Sprintf("%s%s%d-%d", s.path(key, ""), sufClaim, os.Getpid(), s.seq.Add(1))
	if err := os.Rename(src, dst); err != nil {
		return nil, false // no answer, or another taker won the race
	}
	b, rerr := os.ReadFile(dst)
	fi, serr := os.Stat(dst)
	_ = os.Remove(dst)
	calls, ok := decodeCalls(b)
	if rerr != nil || serr != nil || !s.live(fi.ModTime(), s.ttl) || !ok {
		kitlog.Debugf("absorbed-answer store: discarding %s (read=%v stat=%v decoded=%v)", src, rerr, serr, ok)
		return nil, false
	}
	return calls, true
}

// live reports whether an entry stamped mod is still within ttl.
func (s *answerStore) live(mod time.Time, ttl time.Duration) bool {
	return mod.Add(ttl).After(time.Now())
}

// ttlFor is the bound that applies to a file by name: pending markers get
// pendingTTL, answers and transient tmp/claim orphans get ttl.
func (s *answerStore) ttlFor(name string) time.Duration {
	if strings.HasSuffix(name, sufPending) {
		return s.pendingTTL
	}
	return s.ttl
}

// sweep removes every expired file — answers, pending markers and
// transient tmp/claim orphans from a crashed worker alike — and then
// enforces the entry bound, dropping the oldest first.
//
// The bound counts EVERY file, not just answers: a flood of distinct
// absorbed keys inside one pendingTTL window would otherwise grow the
// directory without limit through markers alone. Evicting a live marker
// is safe — its owner republishes it on the next watchTurn tick, and the
// worst case in the meantime is one waiter falling back to a re-run.
//
// Two generations may sweep and write here concurrently. Every operation
// is a single unlink or rename, so the only interleaving that matters is
// a sweep judging a marker expired in the instant before its owner
// republishes it; the republish simply recreates the file, and the cost
// is at most one waiter's re-run, never corruption.
func (s *answerStore) sweep() {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		kitlog.Debugf("absorbed-answer store: readdir %s: %v", s.dir, err)
		return
	}
	type entry struct {
		name string
		mod  time.Time
	}
	live := make([]entry, 0, len(ents))
	for _, e := range ents {
		// An Info() error means the entry vanished under us (another
		// generation swept it); treat it exactly like expired.
		fi, ierr := e.Info()
		if ierr != nil || !s.live(fi.ModTime(), s.ttlFor(e.Name())) {
			_ = os.Remove(filepath.Join(s.dir, e.Name()))
			continue
		}
		live = append(live, entry{e.Name(), fi.ModTime()})
	}
	if len(live) <= s.max {
		return
	}
	slices.SortFunc(live, func(a, b entry) int { return a.mod.Compare(b.mod) })
	for _, e := range live[:len(live)-s.max] {
		_ = os.Remove(filepath.Join(s.dir, e.name))
	}
}

// writeAtomic writes b to path via tmp+rename, so a reader (possibly in
// another worker generation) never observes a partial file. Failures are
// logged and swallowed: a store that cannot write degrades to a re-run,
// which is the pre-existing behaviour, not an outage.
func (s *answerStore) writeAtomic(path string, b []byte) {
	tmp := fmt.Sprintf("%s%s%d-%d", path, sufTmp, os.Getpid(), s.seq.Add(1))
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		s.warnWrite("write", tmp, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		s.warnWrite("rename", path, err)
		_ = os.Remove(tmp)
	}
}

// warnWrite reports a store write failure: loudly the first time in this
// process, at debug thereafter. See answerStore.warnOnce.
func (s *answerStore) warnWrite(op, path string, err error) {
	s.warnOnce.Do(func() {
		log.Printf("WARN absorbed-answer store degraded: %s %s: %v — absorbed turns will re-run on redrive", op, path, err)
	})
	kitlog.Debugf("absorbed-answer store: %s %s: %v", op, path, err)
}

// wireCall is the on-disk form of a recCall. recCall's fields are
// unexported (they must stay that way — replay is the only consumer), so
// the wire shape is spelled out here rather than tagging the live type.
type wireCall struct {
	Op int    `json:"op"`
	S1 string `json:"s1,omitempty"`
	S2 string `json:"s2,omitempty"`
	S3 string `json:"s3,omitempty"`
	S4 string `json:"s4,omitempty"`
}

// encodeCalls renders a recorded turn as the answer file's payload.
func encodeCalls(calls []recCall) []byte {
	w := make([]wireCall, len(calls))
	for i, c := range calls {
		w[i] = wireCall{Op: int(c.op), S1: c.s1, S2: c.s2, S3: c.s3, S4: c.s4}
	}
	return mustMarshalCalls(w)
}

// decodeCalls parses an answer payload. A torn or foreign file is a
// clean miss (ok=false), never a fatal error.
func decodeCalls(b []byte) ([]recCall, bool) {
	var w []wireCall
	if err := json.Unmarshal(b, &w); err != nil {
		return nil, false
	}
	calls := make([]recCall, len(w))
	for i, c := range w {
		calls[i] = recCall{op: recOp(c.Op), s1: c.S1, s2: c.S2, s3: c.S3, s4: c.S4}
	}
	return calls, true
}

package httpsrv

// Unit coverage for the on-disk absorbed-answer store: bound, sweep, torn
// payloads, and the IO failure paths that must degrade to a miss rather
// than take the relay down.

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newStore(t *testing.T) *answerStore {
	t.Helper()
	s := newAnswerStore(filepath.Join(t.TempDir(), "absorbed"), time.Minute, time.Minute)
	if s == nil {
		t.Fatal("newAnswerStore returned nil for a usable dir")
	}
	return s
}

// A state dir that cannot be created disables the store rather than
// failing the relay: the caller then runs memory-only.
func TestAnswerStore_UnusableDirDisablesStore(t *testing.T) {
	f := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s := newAnswerStore(filepath.Join(f, "absorbed"), time.Minute, time.Minute); s != nil {
		t.Fatal("store survived an uncreatable dir")
	}
	if s := newAnswerStore("", time.Minute, time.Minute); s != nil {
		t.Fatal("empty dir must disable the store")
	}
}

// Round-trip: every recorded op survives encode/decode and replays
// verbatim, including the four-string File call.
func TestAnswerStore_RoundTripsEveryOp(t *testing.T) {
	s := newStore(t)
	want := []recCall{
		{op: opFirstChunk},
		{op: opSetProviderEmoji, s1: "🦊"},
		{op: opSetStatus, s1: "steady", s2: "plan"},
		{op: opText, s1: "hello"},
		{op: opReplace, s1: "hello world"},
		{op: opFile, s1: "u", s2: "ct", s3: "n", s4: "ref"},
		{op: opError, s1: "boom", s2: "user_caused_error"},
		{op: opDone},
	}
	s.put("k", want)
	got, ok := s.claim("k")
	if !ok {
		t.Fatal("claim missed a freshly put answer")
	}
	if len(got) != len(want) {
		t.Fatalf("round trip len=%d want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call %d = %+v want %+v", i, got[i], want[i])
		}
	}
	// Take-once: the second claim finds nothing.
	if _, ok := s.claim("k"); ok {
		t.Fatal("answer claimed twice")
	}
}

// A torn or foreign payload is a clean miss — the entry is dropped and
// the caller re-runs the turn. It must never be fatal.
func TestAnswerStore_TornFileIsIgnored(t *testing.T) {
	s := newStore(t)
	s.put("k", []recCall{{op: opText, s1: "hi"}})
	if err := os.WriteFile(s.path("k", sufAnswer), []byte(`[{"op":0,"s1":"tr`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.claim("k"); ok {
		t.Fatal("torn answer file was served")
	}
	if _, err := os.Stat(s.path("k", sufAnswer)); !os.IsNotExist(err) {
		t.Fatal("torn answer file was not removed")
	}
	if _, ok := decodeCalls([]byte("not json")); ok {
		t.Fatal("decodeCalls accepted garbage")
	}
}

// The sweep drops expired entries of every shape — answers, pending
// markers, and transient claim/tmp orphans from a crashed worker.
func TestAnswerStore_SweepDropsExpiredAndOrphans(t *testing.T) {
	s := newStore(t)
	s.put("stale", []recCall{{op: opText, s1: "old"}})
	s.markPending("stalepend")
	orphan := filepath.Join(s.dir, "deadbeef.c999-1")
	if err := os.WriteFile(orphan, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{s.path("stale", sufAnswer), s.path("stalepend", sufPending), orphan} {
		backdate(t, p, 2*time.Minute)
	}
	// A live entry must survive the same sweep.
	s.put("fresh", []recCall{{op: opText, s1: "new"}})

	for _, p := range []string{s.path("stale", sufAnswer), s.path("stalepend", sufPending), orphan} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("expired entry survived the sweep: %s", p)
		}
	}
	if _, ok := s.claim("fresh"); !ok {
		t.Fatal("sweep dropped a live entry")
	}
}

// The directory is bounded like the in-memory map: on overflow the
// oldest answers go first.
func TestAnswerStore_EnforcesEntryBound(t *testing.T) {
	s := newStore(t)
	s.max = 2
	for _, k := range []string{"a", "b", "c"} {
		s.put(k, []recCall{{op: opText, s1: k}})
	}
	// Age them distinctly so eviction order is deterministic, then force
	// a sweep via one more put.
	backdate(t, s.path("a", sufAnswer), 30*time.Second)
	backdate(t, s.path("b", sufAnswer), 20*time.Second)
	backdate(t, s.path("c", sufAnswer), 10*time.Second)
	s.max = 1
	s.put("d", []recCall{{op: opText, s1: "d"}})

	if _, ok := s.claim("a"); ok {
		t.Fatal("oldest entry survived the bound")
	}
	if _, ok := s.claim("b"); ok {
		t.Fatal("second-oldest entry survived the bound")
	}
	if _, ok := s.claim("c"); !ok {
		t.Fatal("newest surviving entry was evicted")
	}
}

// A sweep whose directory has vanished logs and returns; it must not
// panic or wedge the caller.
func TestAnswerStore_SweepSurvivesMissingDir(t *testing.T) {
	s := newStore(t)
	if err := os.RemoveAll(s.dir); err != nil {
		t.Fatal(err)
	}
	s.sweep()
	// And a put into the vanished dir degrades to a miss.
	s.put("k", []recCall{{op: opText, s1: "x"}})
	if _, ok := s.claim("k"); ok {
		t.Fatal("claim served an answer that could not be written")
	}
}

// A rename that cannot land (target occupied by a directory) cleans up
// its tmp file instead of leaking it.
func TestAnswerStore_FailedRenameCleansTmp(t *testing.T) {
	s := newStore(t)
	if err := os.MkdirAll(s.path("k", sufAnswer), 0o700); err != nil {
		t.Fatal(err)
	}
	s.put("k", []recCall{{op: opText, s1: "x"}})
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if !e.IsDir() {
			t.Fatalf("tmp file leaked after a failed rename: %s", e.Name())
		}
	}
}

// pendingLive is false for a key that was never marked.
func TestAnswerStore_PendingLiveFalseWhenAbsent(t *testing.T) {
	s := newStore(t)
	if s.pendingLive("nope") {
		t.Fatal("pendingLive true for an unmarked key")
	}
}

// put writes the answer to memory AND disk, so take-once has to hold
// across the pair: a redrive served from the owner's memory copy must
// retire the disk copy too, or the next redrive — here or on the next
// worker generation — claims the leftover and serves the same answer a
// second time.
func TestAnswerStore_MemoryHitRetiresTheDiskCopy(t *testing.T) {
	store := newStore(t)
	b := newAnswerBuffer(time.Minute, store)
	key := "conv\x00msg"
	b.put(key, []recCall{{op: opText, s1: "once"}})

	if _, ok := b.take(key); !ok {
		t.Fatal("first take missed")
	}
	// A second generation, sharing the dir, must find nothing.
	other := newAnswerBuffer(time.Minute, store)
	if _, ok := other.take(key); ok {
		t.Fatal("answer served twice: the disk copy outlived the memory hit")
	}
}

// The entry bound counts EVERY file, not just answers. A flood of
// distinct absorbed keys inside one pendingTTL window would otherwise
// grow the directory without limit through pending markers alone.
func TestAnswerStore_BoundCountsPendingMarkersToo(t *testing.T) {
	s := newStore(t)
	s.max = 3
	for _, k := range []string{"a", "b", "c", "d", "e", "f"} {
		s.markPending(k)
	}
	s.sweep()
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) > s.max {
		t.Fatalf("directory holds %d entries, bound is %d", len(ents), s.max)
	}
}

// A store whose dir exists but has become unwritable degrades silently at
// debug level for every put and every marker refresh — so the FIRST
// failure is surfaced at WARN, once, naming the consequence.
func TestAnswerStore_FirstWriteFailureWarnsOnce(t *testing.T) {
	s := newStore(t)
	if err := os.RemoveAll(s.dir); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	s.put("k", []recCall{{op: opText, s1: "x"}})
	s.markPending("k")
	s.markPending("k")

	if n := strings.Count(buf.String(), "absorbed-answer store degraded"); n != 1 {
		t.Fatalf("degraded WARN emitted %d times, want exactly 1:\n%s", n, buf.String())
	}
}

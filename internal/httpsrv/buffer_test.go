package httpsrv

import (
	"testing"
	"time"
)

// recordSink is a minimal ChunkSink that records replayed calls so the
// replay() helper can be verified independently of the SSE layer.
type recordSink struct {
	texts    []string
	replaces []string
	files    [][4]string
	suggests []string
	errs     [][2]string
	done     int
	first    int
	emojis   []string
	statuses [][2]string
}

func (r *recordSink) Text(s string) error    { r.texts = append(r.texts, s); return nil }
func (r *recordSink) Replace(s string) error { r.replaces = append(r.replaces, s); return nil }
func (r *recordSink) File(u, c, n, i string) error {
	r.files = append(r.files, [4]string{u, c, n, i})
	return nil
}
func (r *recordSink) SuggestedReply(t string) error {
	r.suggests = append(r.suggests, t)
	return nil
}
func (r *recordSink) Error(t, e string) error { r.errs = append(r.errs, [2]string{t, e}); return nil }
func (r *recordSink) Done() error             { r.done++; return nil }
func (r *recordSink) FirstChunk()             { r.first++ }
func (r *recordSink) SetProviderEmoji(e string) {
	r.emojis = append(r.emojis, e)
}
func (r *recordSink) SetStatus(m, p string) { r.statuses = append(r.statuses, [2]string{m, p}) }

func TestAnswerRecorderAndReplay(t *testing.T) {
	inner := &recordSink{}
	rec := &answerRecorder{inner: inner}
	rec.SetProviderEmoji("🤖")
	rec.SetStatus("calm", "plan")
	rec.FirstChunk()
	if err := rec.Text("hello "); err != nil {
		t.Fatal(err)
	}
	if err := rec.Replace("repl"); err != nil {
		t.Fatal(err)
	}
	if err := rec.File("u", "c", "n", "i"); err != nil {
		t.Fatal(err)
	}
	if err := rec.Error("boom", "user_caused_error"); err != nil {
		t.Fatal(err)
	}
	if err := rec.Done(); err != nil {
		t.Fatal(err)
	}

	// Inner sink saw every forwarded call.
	if len(inner.texts) != 1 || inner.texts[0] != "hello " {
		t.Fatalf("inner texts=%v", inner.texts)
	}
	if inner.done != 1 || inner.first != 1 {
		t.Fatalf("inner done=%d first=%d", inner.done, inner.first)
	}

	// Replay the snapshot onto a fresh sink → identical sequence.
	out := &recordSink{}
	replay(rec.snapshot(), out)
	if len(out.texts) != 1 || out.texts[0] != "hello " {
		t.Fatalf("replay texts=%v", out.texts)
	}
	if len(out.replaces) != 1 || out.replaces[0] != "repl" {
		t.Fatalf("replay replaces=%v", out.replaces)
	}
	if len(out.files) != 1 || out.files[0] != [4]string{"u", "c", "n", "i"} {
		t.Fatalf("replay files=%v", out.files)
	}
	if len(out.errs) != 1 || out.errs[0] != [2]string{"boom", "user_caused_error"} {
		t.Fatalf("replay errs=%v", out.errs)
	}
	if out.done != 1 || out.first != 1 {
		t.Fatalf("replay done=%d first=%d", out.done, out.first)
	}
	if len(out.emojis) != 1 || out.emojis[0] != "🤖" {
		t.Fatalf("replay emojis=%v", out.emojis)
	}
	if len(out.statuses) != 1 || out.statuses[0] != [2]string{"calm", "plan"} {
		t.Fatalf("replay statuses=%v", out.statuses)
	}
}

func TestAnswerKey(t *testing.T) {
	if got := answerKey("c1", ""); got != "" {
		t.Fatalf("empty message id must yield empty key, got %q", got)
	}
	if got := answerKey("c1", "m1"); got != "c1\x00m1" {
		t.Fatalf("key=%q", got)
	}
}

func TestAnswerBuffer_PutTakeEvictsOnServe(t *testing.T) {
	b := newAnswerBuffer(time.Minute)
	calls := []recCall{{op: opText, s1: "x"}}
	b.put("k", calls)
	got, ok := b.take("k")
	if !ok || len(got) != 1 || got[0].s1 != "x" {
		t.Fatalf("take=%v ok=%v", got, ok)
	}
	// Evicted on serve.
	if _, ok := b.take("k"); ok {
		t.Fatal("entry must be evicted after take")
	}
}

func TestAnswerBuffer_TakeMiss(t *testing.T) {
	b := newAnswerBuffer(time.Minute)
	if _, ok := b.take("nope"); ok {
		t.Fatal("miss must return ok=false")
	}
}

func TestAnswerBuffer_TTLExpiry(t *testing.T) {
	now := time.Unix(1000, 0)
	b := newAnswerBuffer(time.Minute)
	b.now = func() time.Time { return now }
	b.put("k", []recCall{{op: opDone}})
	// Advance past the TTL: take must report expired (and the put sweep too).
	now = now.Add(2 * time.Minute)
	if _, ok := b.take("k"); ok {
		t.Fatal("expired entry must not be served")
	}
}

func TestAnswerBuffer_SweepOnPut(t *testing.T) {
	now := time.Unix(0, 0)
	b := newAnswerBuffer(time.Minute)
	b.now = func() time.Time { return now }
	b.put("old", []recCall{{op: opDone}})
	now = now.Add(2 * time.Minute)
	b.put("new", []recCall{{op: opDone}})
	// "old" swept during the put of "new".
	if len(b.m) != 1 {
		t.Fatalf("expected sweep to drop expired entry, map=%d", len(b.m))
	}
	if _, ok := b.m["new"]; !ok {
		t.Fatal("new entry missing")
	}
}

func TestAnswerBuffer_CapEvictsOldest(t *testing.T) {
	base := time.Unix(0, 0)
	b := newAnswerBuffer(time.Hour)
	b.maxEntries = 2
	cur := base
	b.now = func() time.Time { return cur }
	cur = base.Add(1 * time.Second)
	b.put("a", []recCall{{op: opDone}}) // expiry base+1s+1h
	cur = base.Add(2 * time.Second)
	b.put("b", []recCall{{op: opDone}}) // expiry base+2s+1h
	cur = base.Add(3 * time.Second)
	b.put("c", []recCall{{op: opDone}}) // overflow → evict oldest ("a")
	if len(b.m) != 2 {
		t.Fatalf("map size=%d want 2", len(b.m))
	}
	if _, ok := b.m["a"]; ok {
		t.Fatal("oldest entry 'a' should have been evicted")
	}
	if _, ok := b.m["c"]; !ok {
		t.Fatal("newest entry 'c' missing")
	}
}

func TestAnswerBuffer_PutOverwriteKeepsCap(t *testing.T) {
	b := newAnswerBuffer(time.Hour)
	b.maxEntries = 1
	b.put("k", []recCall{{op: opText, s1: "1"}})
	b.put("k", []recCall{{op: opText, s1: "2"}}) // same key: overwrite, no evict
	got, ok := b.take("k")
	if !ok || got[0].s1 != "2" {
		t.Fatalf("overwrite failed: %v ok=%v", got, ok)
	}
}

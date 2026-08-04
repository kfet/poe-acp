package httpsrv

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kfet/poe-acp/internal/poeproto"
)

// renderBody replays a parsed SSE event sequence the way a Poe client
// renders it: `text` appends to the body, `replace_response` overwrites
// it. The result is exactly what the user ends up seeing, which is the
// invariant coalescing must not disturb.
func renderBody(events []sseEventRec) string {
	var body string
	for _, e := range events {
		switch e.event {
		case "text":
			body += e.text
		case "replace_response":
			body = e.text
		}
	}
	return body
}

// newTestSink builds a sink over a fresh recorder, with meta already
// emitted, and returns both.
func newTestSink(t *testing.T, opts sinkOpts) (*sink, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	w, err := poeproto.NewSSEWriter(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Meta(); err != nil {
		t.Fatal(err)
	}
	return newSinkOpts(w, opts), rec
}

// exhaustEagerWindow pushes the writer past the eager first-flush window
// (see eagerFlushChunks) without any wall-clock waiting, by backdating
// the turn start and consuming the chunk budget. After this, buffering
// is governed purely by the grid tick and the paragraph rule.
func exhaustEagerWindow(o *orderedWriter) {
	o.mu.Lock()
	o.turnStart = time.Now().Add(-2 * eagerFlushWindow)
	o.chunks = eagerFlushChunks + 1
	o.mu.Unlock()
}

// TestNextFlushDelay pins the shared wall-clock grid. Independent
// streams — in this process or on another host — must converge on the
// SAME absolute flush instants, otherwise four bots at 3s each still
// wake the modem every 750ms and cDRX never engages.
func TestNextFlushDelay(t *testing.T) {
	const period = 3 * time.Second
	// A spread of arrival instants inside one period must all resolve to
	// the SAME absolute deadline.
	base := time.UnixMilli((1_700_000_000_000 / period.Milliseconds()) * period.Milliseconds())
	if base.UnixMilli()%period.Milliseconds() != 0 {
		t.Fatalf("test base %d is not grid-aligned", base.UnixMilli())
	}
	want := base.Add(period)
	for _, off := range []time.Duration{1 * time.Millisecond, 500 * time.Millisecond, 2999 * time.Millisecond} {
		now := base.Add(off)
		got := now.Add(nextFlushDelay(now, period, true))
		if !got.Equal(want) {
			t.Fatalf("off=%s: grid deadline %s, want %s", off, got, want)
		}
	}
	// Exactly on a grid instant yields a full period, never a zero timer.
	if d := nextFlushDelay(base, period, true); d != period {
		t.Fatalf("on-grid delay = %s, want %s", d, period)
	}
	// Grid off: plain per-stream period from now.
	if d := nextFlushDelay(base.Add(700*time.Millisecond), period, false); d != period {
		t.Fatalf("ungridded delay = %s, want %s", d, period)
	}
	// Sub-millisecond period has no grid to align to.
	if d := nextFlushDelay(base, 500*time.Microsecond, true); d != 500*time.Microsecond {
		t.Fatalf("sub-ms grid delay = %s, want 500µs", d)
	}
}

// TestCoalesce_FinalTextIdentical is the core safety property: for ANY
// coalesce interval, the turn's final visible text is byte-identical to
// the un-coalesced stream. Only the frame COUNT may differ.
func TestCoalesce_FinalTextIdentical(t *testing.T) {
	chunks := make([]string, 0, 300)
	for i := range 300 {
		chunks = append(chunks, fmt.Sprintf("chunk-%03d ", i))
		if i%17 == 0 {
			chunks = append(chunks, "\n\n")
		}
	}
	var want string

	for _, tc := range []struct {
		name string
		opts sinkOpts
	}{
		{"off", sinkOpts{}},
		{"coalesce-grid", sinkOpts{coalesce: 20 * time.Millisecond, grid: true}},
		{"coalesce-nogrid", sinkOpts{coalesce: 20 * time.Millisecond}},
	} {
		s, rec := newTestSink(t, tc.opts)
		for _, c := range chunks {
			if err := s.Text(c); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Done(); err != nil {
			t.Fatal(err)
		}
		s.stop()
		<-s.coExited
		got := renderBody(parseSSE(t, rec.Body.String()))
		if tc.name == "off" {
			want = got
			continue
		}
		if got != want {
			t.Fatalf("%s: final body differs from un-coalesced stream\n got %q\nwant %q", tc.name, got, want)
		}
		frames := s.o.snapshotStats().TextFrames
		t.Logf("%s: text_frames=%d", tc.name, frames)
	}
}

// TestCoalesce_EagerFirstFlush guards the cold-start contract: Poe drops
// a new-conversation bot connection that sees no content event during
// the initial gap, so the opening chunks of a turn must reach the wire
// immediately even with coalescing on.
func TestCoalesce_EagerFirstFlush(t *testing.T) {
	s, rec := newTestSink(t, sinkOpts{coalesce: time.Hour, grid: false})
	defer s.stop()

	if err := s.Text("first"); err != nil {
		t.Fatal(err)
	}
	events := parseSSE(t, rec.Body.String())
	if body := renderBody(events); body != "first" {
		t.Fatalf("first chunk was buffered (body=%q); the cold-start starvation window would be open", body)
	}

	// Still inside the eager window: subsequent chunks also stream 1:1.
	for i := range eagerFlushChunks - 1 {
		if err := s.Text(fmt.Sprintf(" %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.o.snapshotStats().TextFrames; got != int64(eagerFlushChunks) {
		t.Fatalf("eager window emitted %d text frames, want %d", got, eagerFlushChunks)
	}

	// Past the budget, buffering takes over (interval is an hour, so
	// nothing can flush on its own).
	exhaustEagerWindow(s.o)
	if err := s.Text(" buffered"); err != nil {
		t.Fatal(err)
	}
	if got := s.o.snapshotStats().TextFrames; got != int64(eagerFlushChunks) {
		t.Fatalf("post-eager chunk was written immediately (%d frames); want it buffered", got)
	}
	// The buffered tail must still land in the final body.
	if err := s.Done(); err != nil {
		t.Fatal(err)
	}
	if body := renderBody(parseSSE(t, rec.Body.String())); !strings.HasSuffix(body, " buffered") {
		t.Fatalf("buffered tail lost on Done: %q", body)
	}
}

// TestCoalesce_EagerWindowExpiresOnTime covers the time half of the
// "first ~30 chunks OR 1.5s, whichever comes first" rule: a slow trickle
// that never reaches the chunk budget still leaves the eager window.
func TestCoalesce_EagerWindowExpiresOnTime(t *testing.T) {
	s, _ := newTestSink(t, sinkOpts{coalesce: time.Hour})
	defer s.stop()
	if err := s.Text("a"); err != nil {
		t.Fatal(err)
	}
	// Backdate only the clock, leaving the chunk budget untouched.
	s.o.mu.Lock()
	s.o.turnStart = time.Now().Add(-2 * eagerFlushWindow)
	s.o.mu.Unlock()
	if err := s.Text("b"); err != nil {
		t.Fatal(err)
	}
	if got := s.o.snapshotStats().TextFrames; got != 1 {
		t.Fatalf("text frames = %d, want 1 (second chunk should be buffered once the eager window expired)", got)
	}
}

// TestCoalesce_ParagraphFlush pins the readability rule: a non-trivial
// buffer holding a paragraph boundary is worth one extra wake.
func TestCoalesce_ParagraphFlush(t *testing.T) {
	s, rec := newTestSink(t, sinkOpts{coalesce: time.Hour})
	defer s.stop()
	exhaustEagerWindow(s.o)

	// A paragraph boundary in a trivial buffer does NOT flush — below
	// the size floor, so a burst of short lines can't degenerate back
	// into per-chunk frames.
	if err := s.Text("tiny\n\n"); err != nil {
		t.Fatal(err)
	}
	if got := s.o.snapshotStats().TextFrames; got != 0 {
		t.Fatalf("sub-threshold paragraph flushed (%d frames)", got)
	}
	// Crossing the floor with a boundary present flushes.
	if err := s.Text(strings.Repeat("x", paragraphFlushMin)); err != nil {
		t.Fatal(err)
	}
	if got := s.o.snapshotStats().TextFrames; got != 1 {
		t.Fatalf("paragraph boundary did not flush (%d frames)", got)
	}
	if body := renderBody(parseSSE(t, rec.Body.String())); !strings.HasPrefix(body, "tiny\n\n") {
		t.Fatalf("flushed body %q lost its head", body)
	}

	// A large buffer with NO paragraph boundary keeps waiting for the
	// grid tick.
	s2, _ := newTestSink(t, sinkOpts{coalesce: time.Hour})
	defer s2.stop()
	exhaustEagerWindow(s2.o)
	if err := s2.Text(strings.Repeat("y", 4*paragraphFlushMin)); err != nil {
		t.Fatal(err)
	}
	if got := s2.o.snapshotStats().TextFrames; got != 0 {
		t.Fatalf("boundary-free buffer flushed early (%d frames)", got)
	}
}

// TestCoalesce_OrderingSafety is requirement #4: a buffered `text` may
// never be overtaken. Every other stream write must flush the buffer
// first, synchronously.
func TestCoalesce_OrderingSafety(t *testing.T) {
	cases := []struct {
		name string
		// fire performs the ordering-forcing event.
		fire func(t *testing.T, s *sink)
		// want is the event that must directly follow the flushed text.
		want string
	}{
		{"replace", func(t *testing.T, s *sink) {
			if err := s.Replace("REPLACED"); err != nil {
				t.Fatal(err)
			}
		}, "replace_response"},
		{"file", func(t *testing.T, s *sink) {
			if err := s.File("u", "text/plain", "n", ""); err != nil {
				t.Fatal(err)
			}
		}, "file"},
		{"error", func(t *testing.T, s *sink) {
			if err := s.Error("boom", ""); err != nil {
				t.Fatal(err)
			}
		}, "error"},
		{"done", func(t *testing.T, s *sink) {
			if err := s.Done(); err != nil {
				t.Fatal(err)
			}
		}, "done"},
		{"heartbeat", func(_ *testing.T, s *sink) {
			var tick int
			s.emitSpinnerFrame(&tick)
		}, "replace_response"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// stall=0 so the heartbeat case counts as stalled.
			s, rec := newTestSink(t, sinkOpts{coalesce: time.Hour})
			defer s.stop()
			exhaustEagerWindow(s.o)
			if err := s.Text("BUFFERED"); err != nil {
				t.Fatal(err)
			}
			if got := s.o.snapshotStats().TextFrames; got != 0 {
				t.Fatalf("precondition: text was not buffered (%d frames)", got)
			}
			tc.fire(t, s)

			events := parseSSE(t, rec.Body.String())
			// events[0] is meta; the buffered text must be next, and the
			// forcing event immediately after it.
			if len(events) < 3 {
				t.Fatalf("want meta+text+%s, got %d events: %s", tc.want, len(events), rec.Body.String())
			}
			if events[1].event != "text" || events[1].text != "BUFFERED" {
				t.Fatalf("buffered text was overtaken: events[1]=%+v\n%s", events[1], rec.Body.String())
			}
			if events[2].event != tc.want {
				t.Fatalf("events[2] = %q, want %q\n%s", events[2].event, tc.want, rec.Body.String())
			}
		})
	}
}

// TestCoalesce_GridFlush drives the real flusher goroutine and proves a
// buffered chunk lands without any other event forcing it.
func TestCoalesce_GridFlush(t *testing.T) {
	flushed := make(chan struct{}, 8)
	s, rec := newTestSink(t, sinkOpts{
		coalesce:  10 * time.Millisecond,
		grid:      true,
		flushHook: func() { flushed <- struct{}{} },
	})
	defer s.stop()
	exhaustEagerWindow(s.o)
	if err := s.Text("tick-me"); err != nil {
		t.Fatal(err)
	}
	// Wait for a flush cycle that actually emitted the buffered text.
	deadline := time.After(5 * time.Second)
	for s.o.snapshotStats().TextFrames == 0 {
		select {
		case <-flushed:
		case <-deadline:
			t.Fatal("grid flusher never drained the buffer")
		}
	}
	if body := renderBody(parseSSE(t, rec.Body.String())); body != "tick-me" {
		t.Fatalf("body = %q, want %q", body, "tick-me")
	}
}

// TestCoalesce_ClosedStreamDropsBuffer covers the sealed-stream guards:
// once `done` is out, buffering and flushing are both no-ops.
func TestCoalesce_ClosedStreamDropsBuffer(t *testing.T) {
	s, _ := newTestSink(t, sinkOpts{coalesce: time.Hour})
	defer s.stop()
	if err := s.Done(); err != nil {
		t.Fatal(err)
	}
	if err := s.Text("after-done"); err != nil {
		t.Fatal(err)
	}
	if err := s.o.flush(); err != nil {
		t.Fatal(err)
	}
	if got := s.o.snapshotStats().TextFrames; got != 0 {
		t.Fatalf("post-done write reached the wire (%d text frames)", got)
	}
}

// TestHeartbeatDedupe covers requirement #5: an identical keepalive
// payload is not re-sent, because the client would render exactly what
// it is already rendering. With a static spinner that collapses the
// 1.5s keepalive to genuine state changes plus the liveness floor.
func TestHeartbeatDedupe(t *testing.T) {
	s, _ := newTestSink(t, sinkOpts{spinnerStatic: true})
	defer s.stop()
	var tick int

	s.emitSpinnerFrame(&tick)
	first := s.o.snapshotStats().ReplaceFrames
	if first != 1 {
		t.Fatalf("first keepalive frames = %d, want 1", first)
	}
	// Identical payload, repeatedly: suppressed.
	for range 10 {
		s.emitSpinnerFrame(&tick)
	}
	if got := s.o.snapshotStats().ReplaceFrames; got != 1 {
		t.Fatalf("identical keepalives emitted %d frames, want 1", got)
	}
	// A genuine state change breaks the dedupe immediately.
	s.SetStatus("focused", "digging")
	s.emitSpinnerFrame(&tick)
	if got := s.o.snapshotStats().ReplaceFrames; got != 2 {
		t.Fatalf("state change did not emit a frame (%d)", got)
	}
	// Liveness floor: after it lapses an identical frame goes out anyway,
	// because the heartbeat is also Poe's content-starvation keepalive.
	s.o.mu.Lock()
	s.o.lastHBAt = time.Now().Add(-2 * s.o.hbFloor)
	s.o.mu.Unlock()
	s.emitSpinnerFrame(&tick)
	if got := s.o.snapshotStats().ReplaceFrames; got != 3 {
		t.Fatalf("liveness floor did not re-emit (%d frames)", got)
	}
}

// TestHeartbeatDedupe_NotSuppressedAfterStrip: if a user write stripped
// the spinner off screen, the next identical frame MUST be re-sent —
// otherwise the status line silently disappears for the rest of the turn.
func TestHeartbeatDedupe_NotSuppressedAfterStrip(t *testing.T) {
	s, _ := newTestSink(t, sinkOpts{spinnerStatic: true})
	defer s.stop()
	var tick int
	s.emitSpinnerFrame(&tick)
	if err := s.Text("hi"); err != nil { // strips the spinner
		t.Fatal(err)
	}
	// Same rendered line as before, but no longer on screen.
	s.emitSpinnerFrame(&tick)
	// 1 cold-start frame + 1 strip + 1 re-arm.
	if got := s.o.snapshotStats().ReplaceFrames; got != 3 {
		t.Fatalf("replace frames = %d, want 3 (spinner must be re-armed after a strip)", got)
	}
}

// TestSpinnerDots pins the animate/static split.
func TestSpinnerDots(t *testing.T) {
	animated := &sink{}
	if got := []string{animated.dots(1), animated.dots(2), animated.dots(3), animated.dots(4)}; got[0] != "." || got[1] != ".." || got[2] != "..." || got[3] != "." {
		t.Fatalf("animated dots = %v", got)
	}
	static := &sink{spinnerStatic: true}
	if got := static.dots(1); got != staticSpinnerDots {
		t.Fatalf("static dots = %q, want %q", got, staticSpinnerDots)
	}
	if static.dots(2) != static.dots(7) {
		t.Fatal("static dots must not vary with the tick counter")
	}
}

// TestHBDedupeFloor pins the derivation of the liveness floor from the
// operator's own declared drop-tolerance margin.
func TestHBDedupeFloor(t *testing.T) {
	if got := hbDedupeFloor(8 * time.Second); got != 16*time.Second {
		t.Fatalf("floor(8s) = %s, want 16s", got)
	}
	if got := hbDedupeFloor(time.Second); got != hbDedupeFloorMin {
		t.Fatalf("floor(1s) = %s, want the %s minimum", got, hbDedupeFloorMin)
	}
}

// TestFrameStats covers the per-turn accounting and its log line.
func TestFrameStats(t *testing.T) {
	s, _ := newTestSink(t, sinkOpts{})
	defer s.stop()
	if err := s.Text(""); err != nil { // dropped by the writer; must not count
		t.Fatal(err)
	}
	if err := s.Text("hello"); err != nil {
		t.Fatal(err)
	}
	if err := s.Replace("hi"); err != nil {
		t.Fatal(err)
	}
	if err := s.File("u", "text/plain", "n", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Error("bad", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Done(); err != nil {
		t.Fatal(err)
	}
	st := s.o.snapshotStats()
	if st.TextFrames != 1 || st.ReplaceFrames != 1 || st.OtherFrames != 3 {
		t.Fatalf("stats = %+v, want text=1 replace=1 other=3", st)
	}
	if st.BytesOut != int64(len("hello")+len("hi")+len("bad")) {
		t.Fatalf("bytes_out = %d", st.BytesOut)
	}
	logFrameStats("c-1", st, 1500*time.Millisecond)
}

// TestCoalesce_ConcurrentWritesAreOrdered runs the grid flusher against
// a hot producer under -race, then checks the rendered body is exactly
// the concatenation of what was written.
func TestCoalesce_ConcurrentWritesAreOrdered(t *testing.T) {
	s, rec := newTestSink(t, sinkOpts{coalesce: time.Millisecond, grid: true})
	var want strings.Builder
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 500 {
			c := fmt.Sprintf("%d,", i)
			want.WriteString(c)
			if err := s.Text(c); err != nil {
				return
			}
		}
	}()
	wg.Wait()
	if err := s.Done(); err != nil {
		t.Fatal(err)
	}
	s.stop()
	<-s.coExited
	if got := renderBody(parseSSE(t, rec.Body.String())); got != want.String() {
		t.Fatalf("body mismatch under concurrency:\n got %q\nwant %q", got, want.String())
	}
}

// syntheticChunks / syntheticBatches / syntheticSpan describe the
// synthetic long-answer workload: 2000 chunks spread over a 60s answer,
// arriving in bursts the way a real agent streams them. The timeline is
// compressed 100× (60s → 600ms) so it can run in the unit suite; every
// interval in the measurement — chunk spacing, heartbeat, coalescing
// period — is scaled by the same factor, so the frame RATIO is exactly
// what the uncompressed run would produce.
const (
	syntheticChunks     = 2000
	syntheticBatches    = 100
	syntheticSpan       = 600 * time.Millisecond // 60s / 100
	syntheticHeartbeat  = 15 * time.Millisecond  // 1500ms / 100
	syntheticCoalesceMs = 30 * time.Millisecond  // 3000ms / 100
)

// TestCoalesce_SyntheticLongAnswer is the measurement: it streams the
// same long answer through an un-coalesced and a coalesced sink and
// reports frames-per-turn for each. Frames-per-turn is the phone's
// radio-wake count, so the ratio printed here IS the radio-wake ratio.
func TestCoalesce_SyntheticLongAnswer(t *testing.T) {
	run := func(opts sinkOpts) (frameStats, string) {
		opts.heartbeat = syntheticHeartbeat
		opts.stall = 8 * time.Millisecond
		s, rec := newTestSink(t, opts)
		perBatch := syntheticChunks / syntheticBatches
		tick := time.NewTicker(syntheticSpan / syntheticBatches)
		defer tick.Stop()
		n := 0
		for range syntheticBatches {
			<-tick.C
			for range perBatch {
				if err := s.Text(fmt.Sprintf("tok%04d ", n)); err != nil {
					t.Fatal(err)
				}
				n++
			}
		}
		if err := s.Done(); err != nil {
			t.Fatal(err)
		}
		s.stop()
		<-s.coExited
		<-s.hbExited
		return s.o.snapshotStats(), renderBody(parseSSE(t, rec.Body.String()))
	}

	before, bodyBefore := run(sinkOpts{})
	after, bodyAfter := run(sinkOpts{
		coalesce:      syntheticCoalesceMs,
		grid:          true,
		spinnerStatic: true,
	})

	if bodyBefore != bodyAfter {
		t.Fatal("coalesced answer text differs from the un-coalesced one")
	}
	beforeTotal := before.TextFrames + before.ReplaceFrames + before.OtherFrames
	afterTotal := after.TextFrames + after.ReplaceFrames + after.OtherFrames
	t.Logf("BEFORE (coalesce off): text=%d replace=%d other=%d total=%d bytes=%d",
		before.TextFrames, before.ReplaceFrames, before.OtherFrames, beforeTotal, before.BytesOut)
	t.Logf("AFTER  (coalesce 3s grid + static spinner): text=%d replace=%d other=%d total=%d bytes=%d",
		after.TextFrames, after.ReplaceFrames, after.OtherFrames, afterTotal, after.BytesOut)
	t.Logf("frame ratio: %.1f× fewer wire frames (%d → %d)",
		float64(beforeTotal)/float64(afterTotal), beforeTotal, afterTotal)

	// Generous bound: the point is an order of magnitude, and CI timing
	// jitter must not make the assertion flaky.
	if afterTotal*10 > beforeTotal {
		t.Fatalf("coalescing cut frames only from %d to %d; want at least a 10× reduction", beforeTotal, afterTotal)
	}
}

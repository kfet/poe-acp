package httpsrv

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/poe-acp/internal/poeproto"
	"github.com/kfet/poe-acp/internal/router"
)

// blockingResp is an http.ResponseWriter whose Write blocks (once armed)
// until a gate is released, modelling a client that has stopped reading
// (TCP backpressure). It optionally honours a write deadline set via
// http.ResponseController — so the SSE write-deadline recovery path is
// exercisable — by returning os.ErrDeadlineExceeded when the deadline
// elapses before the gate opens.
type blockingResp struct {
	hdr       http.Header
	mu        sync.Mutex
	buf       bytes.Buffer
	armed     bool
	dl        time.Time
	gate      chan struct{}
	entered   chan struct{}
	enterOnce sync.Once
}

func newBlockingResp() *blockingResp {
	return &blockingResp{
		hdr:     http.Header{},
		gate:    make(chan struct{}),
		entered: make(chan struct{}),
	}
}

func (r *blockingResp) Header() http.Header { return r.hdr }
func (r *blockingResp) WriteHeader(int)     {}
func (r *blockingResp) Flush()              {}

// arm makes all subsequent Writes block until release (or the write
// deadline elapses).
func (r *blockingResp) arm() {
	r.mu.Lock()
	r.armed = true
	r.mu.Unlock()
}

// release unblocks any blocked writes. Call at most once.
func (r *blockingResp) release() { close(r.gate) }

// SetWriteDeadline satisfies http.ResponseController's optional
// interface so the SSE write-deadline path is honoured in tests.
func (r *blockingResp) SetWriteDeadline(t time.Time) error {
	r.mu.Lock()
	r.dl = t
	r.mu.Unlock()
	return nil
}
func (r *blockingResp) SetReadDeadline(time.Time) error { return nil }

func (r *blockingResp) Write(b []byte) (int, error) {
	r.mu.Lock()
	armed := r.armed
	dl := r.dl
	r.mu.Unlock()
	if armed {
		r.enterOnce.Do(func() { close(r.entered) })
		var timer <-chan time.Time
		if !dl.IsZero() {
			d := time.Until(dl)
			if d <= 0 {
				return 0, os.ErrDeadlineExceeded
			}
			tm := time.NewTimer(d)
			defer tm.Stop()
			timer = tm.C
		}
		select {
		case <-r.gate:
		case <-timer:
			return 0, os.ErrDeadlineExceeded
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(b)
}

// TestOrderedWriter_StateReadsNotWedgedByBlockedWrite is the FAIL-FIRST
// reproduction of the production finalization hang. A heartbeat spinner
// frame is mid-write to a client that has stopped reading (blocked
// Write). On the buggy code orderedWriter.hbFrame held o.mu ACROSS that
// blocking write, so every state read (isClosed/hasOutput) and the
// terminal userDone that also acquire o.mu wedged forever — the turn
// never finalized and the Stop button never cleared. The fix keeps o.mu
// critical sections to state only (the wire write runs under a separate
// writeMu), so these reads never wedge behind a blocked wire write.
func TestOrderedWriter_StateReadsNotWedgedByBlockedWrite(t *testing.T) {
	br := newBlockingResp()
	w, err := poeproto.NewSSEWriter(br)
	if err != nil {
		t.Fatal(err)
	}
	o := &orderedWriter{w: w}
	br.arm()

	// Heartbeat frame write blocks inside o.w.Replace (client backpressure).
	go func() { _, _ = o.hbFrame("> _Thinking._") }()
	select {
	case <-br.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat write never entered")
	}

	// State reads must not be wedged behind the in-flight blocked write.
	done := make(chan struct{})
	go func() {
		_ = o.isClosed()
		_ = o.hasOutput()
		o.mu.Lock()
		o.mu.Unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("state reads wedged behind a blocked SSE write — o.mu held across the write")
	}
	br.release()
}

// TestOrderedWriter_DoneSealsWhenWriteDeadlineFires proves the SSE
// write-deadline recovery path: a heartbeat frame is blocked writing to
// a dead reader, then the terminal userDone is issued. Even though the
// heartbeat holds writeMu on a blocked write AND userDone's own Done
// write would also block, the write deadline aborts both, so userDone
// returns bounded and STILL seals the stream (closed=true). This is the
// regression for "write-deadline path doesn't drop the terminal done's
// sealing".
func TestOrderedWriter_DoneSealsWhenWriteDeadlineFires(t *testing.T) {
	br := newBlockingResp()
	w, err := poeproto.NewSSEWriter(br)
	if err != nil {
		t.Fatal(err)
	}
	w.SetWriteTimeout(100 * time.Millisecond)
	o := &orderedWriter{w: w}
	br.arm() // gate never released: only the deadline can free a write

	hbReturned := make(chan struct{})
	go func() { _, _ = o.hbFrame("> _Thinking._"); close(hbReturned) }()
	select {
	case <-br.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat write never entered")
	}

	res := make(chan error, 1)
	go func() { res <- o.userDone() }()
	select {
	case <-res:
	case <-time.After(3 * time.Second):
		t.Fatal("userDone wedged despite the SSE write deadline")
	}
	if !o.isClosed() {
		t.Fatal("userDone must seal the stream even when its wire write deadline-fails")
	}
	select {
	case <-hbReturned:
	case <-time.After(3 * time.Second):
		t.Fatal("blocked heartbeat frame never returned via the write deadline")
	}
}

// blockFinalizeAgent emits one user-visible chunk, signals, then waits
// for the test to release it before ending the turn — so the test can
// arm the blocking writer at a deterministic point (after first output,
// before finalization) and prove the terminal done + Prompt return are
// not wedged by a blocked-writer heartbeat.
type blockFinalizeAgent struct {
	*fakeAgent
	chunked chan struct{}
	finish  chan struct{}
}

func (a *blockFinalizeAgent) Prompt(_ context.Context, sid acp.SessionId, _ []acp.ContentBlock) (acp.StopReason, error) {
	a.fakeAgent.mu.Lock()
	sink := a.fakeAgent.sinks[sid]
	a.fakeAgent.mu.Unlock()
	_ = sink.OnUpdate(context.Background(), acp.SessionNotification{
		SessionId: sid,
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("partial")},
		},
	})
	close(a.chunked)
	<-a.finish
	return acp.StopReasonEndTurn, nil
}

// TestHandler_FinalizesUnderBlockedWriterHeartbeat is the end-to-end
// proof of the fix: a turn produces output, then the client stops
// reading (writer armed to block) while the mid-turn keepalive spinner
// is re-arming, then the turn ends. handleQuery must still drive the
// endTurn ack, emit the terminal done, and RETURN — bounded by the SSE
// write deadline — so the HTTP response closes and the Stop button
// clears. Before the fix this wedged forever (heartbeat holding the lock
// on a blocked write).
func TestHandler_FinalizesUnderBlockedWriterHeartbeat(t *testing.T) {
	a := &blockFinalizeAgent{
		fakeAgent: &fakeAgent{},
		chunked:   make(chan struct{}),
		finish:    make(chan struct{}),
	}
	rtr, err := router.New(router.Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	// Fast heartbeat + tiny stall so the spinner re-arms aggressively;
	// short write deadline so a blocked write fails quickly.
	h := New(Config{
		Router:            rtr,
		HeartbeatInterval: 2 * time.Millisecond,
		StallThreshold:    time.Millisecond,
		SSEWriteTimeout:   150 * time.Millisecond,
	})

	body := mustJSON(map[string]any{
		"type": "query", "conversation_id": "c-fin",
		"query": []map[string]any{{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body))
	br := newBlockingResp()

	served := make(chan struct{})
	go func() {
		defer close(served)
		h.ServeHTTP(br, req)
	}()

	// First chunk has landed → arm the writer so every subsequent write
	// (re-armed spinner + terminal done) blocks on the dead reader.
	select {
	case <-a.chunked:
	case <-time.After(3 * time.Second):
		t.Fatal("agent never emitted first chunk")
	}
	br.arm()
	// Give the heartbeat a moment to enter a blocked spinner write, then
	// end the turn: finalization must not wedge behind it.
	select {
	case <-br.entered:
	case <-time.After(2 * time.Second):
		// Heartbeat may not have ticked into a stall yet; proceed anyway.
	}
	close(a.finish)

	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("handleQuery never returned — turn finalization wedged behind a blocked-writer heartbeat")
	}
}

// TestOrderedWriter_FileStripsSpinner covers userFile's spinner-strip
// branch: a `file` event arriving while a keepalive spinner frame is on
// screen must re-render the accumulated body (strip) before advertising
// the attachment, so the file chip isn't rendered below a frozen status
// line.
func TestOrderedWriter_FileStripsSpinner(t *testing.T) {
	rec := httptest.NewRecorder()
	w, err := poeproto.NewSSEWriter(rec)
	if err != nil {
		t.Fatal(err)
	}
	_ = w.Meta()
	o := &orderedWriter{w: w}
	// Some accumulated content, then a visible spinner frame.
	if err := o.userText("answer"); err != nil {
		t.Fatal(err)
	}
	if open, _ := o.hbFrame("> _Thinking._"); !open {
		t.Fatal("precondition: spinner frame must go on the wire")
	}
	if !o.spinnerVisible {
		t.Fatal("precondition: spinnerVisible must be true")
	}
	if err := o.userFile("https://poe/x", "text/plain", "f.txt", ""); err != nil {
		t.Fatal(err)
	}
	if o.spinnerVisible {
		t.Fatal("userFile must strip the visible spinner")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: file") {
		t.Fatalf("missing file event: %q", body)
	}
	// The strip re-rendered the accumulated answer before the file event.
	if !strings.Contains(body, "\"text\":\"answer\"") {
		t.Fatalf("strip did not re-render accumulated content: %q", body)
	}
}

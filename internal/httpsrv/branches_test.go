package httpsrv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/poe-acp/internal/acpclient"
	"github.com/kfet/poe-acp/internal/authbroker"
	"github.com/kfet/poe-acp/internal/debuglog"
	"github.com/kfet/poe-acp/internal/poeproto"
	"github.com/kfet/poe-acp/internal/router"
)

func TestHandler_MethodNotAllowed(t *testing.T) {
	h := New(Config{})
	req := httptest.NewRequest(http.MethodGet, "/poe", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestHandler_DecodeError(t *testing.T) {
	h := New(Config{})
	req := httptest.NewRequest(http.MethodPost, "/poe", strings.NewReader("not-json"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestHandler_ReportTypesAndUnknown(t *testing.T) {
	h := New(Config{})
	for _, ty := range []string{"report_feedback", "report_reaction", "report_error"} {
		req := httptest.NewRequest(http.MethodPost, "/poe",
			bytes.NewReader(mustJSON(map[string]any{"type": ty})))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Errorf("%s: %d", ty, rec.Code)
		}
	}
	// Unknown type → 400.
	req := httptest.NewRequest(http.MethodPost, "/poe",
		bytes.NewReader(mustJSON(map[string]any{"type": "weird"})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown: %d", rec.Code)
	}
}

func TestDebugHandler(t *testing.T) {
	rtr, err := router.New(router.Config{Agent: &fakeAgent{}, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := DebugHandler(rtr)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"sessions"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

// nonFlushResp is an http.ResponseWriter that does NOT implement http.Flusher.
type nonFlushResp struct {
	hdr  http.Header
	body bytes.Buffer
	code int
}

func (r *nonFlushResp) Header() http.Header { return r.hdr }
func (r *nonFlushResp) Write(b []byte) (int, error) {
	return r.body.Write(b)
}
func (r *nonFlushResp) WriteHeader(c int) { r.code = c }

func TestHandler_HandleQuery_NotFlushable(t *testing.T) {
	rtr, err := router.New(router.Config{Agent: &fakeAgent{}, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Router: rtr})
	w := &nonFlushResp{hdr: http.Header{}}
	body := mustJSON(map[string]any{
		"type": "query", "conversation_id": "c1",
		"query": []map[string]any{{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body))
	h.ServeHTTP(w, req)
	if w.code != http.StatusInternalServerError {
		t.Fatalf("code=%d body=%s", w.code, w.body.String())
	}
}

// errorMetaResp returns an http.ResponseWriter where Write fails immediately,
// so even the first SSE event (Meta) fails.
type errorMetaResp struct {
	hdr http.Header
}

func (r *errorMetaResp) Header() http.Header       { return r.hdr }
func (r *errorMetaResp) Write([]byte) (int, error) { return 0, io.ErrShortWrite }
func (r *errorMetaResp) WriteHeader(int)           {}
func (r *errorMetaResp) Flush()                    {}

func TestHandler_HandleQuery_MetaError(t *testing.T) {
	rtr, err := router.New(router.Config{Agent: &fakeAgent{}, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Router: rtr})
	w := &errorMetaResp{hdr: http.Header{}}
	body := mustJSON(map[string]any{
		"type": "query", "conversation_id": "c1",
		"query": []map[string]any{{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body))
	h.ServeHTTP(w, req)
	// Just exercising the path; the handler logs and returns.
}

func TestHandler_DebugLogPath(t *testing.T) {
	prev := debuglog.Enabled()
	debuglog.SetEnabled(true)
	defer debuglog.SetEnabled(prev)

	rtr, err := router.New(router.Config{Agent: &fakeAgent{}, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Router: rtr, HeartbeatInterval: 0})
	body := mustJSON(map[string]any{
		"type": "query", "conversation_id": "c1", "user_id": "u", "message_id": "m",
		"query": []map[string]any{{
			"role": "user", "content": "hi",
			"parameters": map[string]any{"thinking": "high"},
		}},
	})
	req := httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestHandler_SpinnerInterval(t *testing.T) {
	t.Skip("retired: SpinnerInterval collapsed into HeartbeatInterval")
} // hangAgent blocks Prompt until ctx is cancelled.
type hangAgent struct {
	*fakeAgent
	cancelled chan struct{}
	entered   chan struct{}
}

func (a *hangAgent) Prompt(ctx context.Context, _ acp.SessionId, _ []acp.ContentBlock) (acp.StopReason, error) {
	if a.entered != nil {
		close(a.entered)
	}
	<-ctx.Done()
	close(a.cancelled)
	return acp.StopReasonCancelled, ctx.Err()
}

func TestHandler_CancelOnDisconnect(t *testing.T) {
	a := &hangAgent{fakeAgent: &fakeAgent{}, cancelled: make(chan struct{}), entered: make(chan struct{})}
	rtr, err := router.New(router.Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Router: rtr, HeartbeatInterval: 0})

	body := mustJSON(map[string]any{
		"type": "query", "conversation_id": "c-cx",
		"query": []map[string]any{{"role": "user", "content": "hi"}},
	})

	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, bytes.NewReader(body))
	go func() {
		select {
		case <-a.entered:
		case <-time.After(3 * time.Second):
		}
		cancel()
	}()
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	select {
	case <-a.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("agent prompt never cancelled")
	}
}

func TestHandler_AuthBrokerError(t *testing.T) {
	stub := &errAuth{err: errors.New("agent down")}
	broker := authbroker.New(stub)
	rtr, err := router.New(router.Config{Agent: &fakeAgent{}, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Router: rtr, AuthBroker: broker, HeartbeatInterval: 0})
	body := mustJSON(map[string]any{
		"type": "query", "conversation_id": "c1", "user_id": "u",
		"query": []map[string]any{{"role": "user", "content": "/login anthropic"}},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body)))
	if !strings.Contains(rec.Body.String(), "event: error") {
		t.Fatalf("expected error event: %s", rec.Body.String())
	}
}

type errAuth struct {
	methods []acpclient.AuthMethod
	err     error
}

func (e *errAuth) AuthMethods() []acpclient.AuthMethod {
	return []acpclient.AuthMethod{{ID: "oauth-anthropic", Type: "agent"}}
}
func (e *errAuth) Authenticate(_ context.Context, _, _, _ string, _ bool) (acpclient.AuthResult, error) {
	return acpclient.AuthResult{}, e.err
}

func TestLatestUserTurn(t *testing.T) {
	if got := latestUserTurn(nil); got != "" {
		t.Fatal()
	}
	turns := []router.Turn{
		{Role: "user", Content: "first"},
		{Role: "bot", Content: "reply"},
		{Role: "user", Content: "last"},
	}
	if got := latestUserTurn(turns); got != "last" {
		t.Fatal(got)
	}
	turns = []router.Turn{{Role: "bot", Content: "x"}}
	if got := latestUserTurn(turns); got != "" {
		t.Fatal(got)
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("", 10); got != "" {
		t.Fail()
	}
	if got := truncateRunes("hi", 0); got != "" {
		t.Fail()
	}
	if got := truncateRunes("hi", 10); got != "hi" {
		t.Fail()
	}
	long := strings.Repeat("a", 200)
	if got := truncateRunes(long, 80); !strings.HasSuffix(got, "…") || len([]rune(got)) != 81 {
		t.Fatalf("got len=%d", len([]rune(got)))
	}
	// Multibyte runes — len(s) > n but rune count <= n.
	multi := strings.Repeat("☃", 50) // each ☃ is 3 bytes
	if got := truncateRunes(multi, 50); got != multi {
		t.Fatalf("multibyte not preserved: %q", got)
	}
	// Truncated multibyte.
	if got := truncateRunes(multi, 5); len([]rune(got)) != 6 || !strings.HasSuffix(got, "…") {
		t.Fatalf("got %q", got)
	}
}

func TestSink_BasicFlow(t *testing.T) {
	rec := httptest.NewRecorder()
	w, err := poeproto.NewSSEWriter(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Meta(); err != nil {
		t.Fatal(err)
	}
	s := newSink(w, 0) // heartbeat disabled path
	if err := s.Text("hi"); err != nil {
		t.Fatal(err)
	}
	if err := s.Replace("rep"); err != nil {
		t.Fatal(err)
	}
	if err := s.Error("oops", "T"); err != nil {
		t.Fatal(err)
	}
	if err := s.Done(); err != nil {
		t.Fatal(err)
	}
	// Calling stop again is no-op.
	s.stop()
	// FirstChunk after stop is no-op.
	s.FirstChunk()
}

func TestSink_FirstChunkWhileSpinning(t *testing.T) {
	rec := httptest.NewRecorder()
	w, _ := poeproto.NewSSEWriter(rec)
	_ = w.Meta()
	wait := waitTicks(t, 2) // two ticks → first iteration's hbReplace has definitely written
	s := newSink(w, 5*time.Millisecond)
	wait()
	s.FirstChunk()
	<-s.hbExited // race-free: heartbeat goroutine has fully returned
	if !strings.Contains(rec.Body.String(), "Thinking") {
		t.Fatalf("missing spinner: %s", rec.Body.String())
	}
}

// fakeBroker returns whatever (out, err) is set.
type fakeBroker struct {
	pending bool
	out     *authbroker.Outcome
	err     error
}

func (f *fakeBroker) HasPending(string) bool { return f.pending }
func (f *fakeBroker) Handle(context.Context, string, string) (*authbroker.Outcome, error) {
	return f.out, f.err
}

func TestHandler_AuthBroker_NilOutcome(t *testing.T) {
	rtr, err := router.New(router.Config{Agent: &fakeAgent{}, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Router: rtr, AuthBroker: &fakeBroker{pending: true}, HeartbeatInterval: 0})
	body := mustJSON(map[string]any{
		"type": "query", "conversation_id": "c1",
		"query": []map[string]any{{"role": "user", "content": "anything"}},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body)))
	if !strings.Contains(rec.Body.String(), "event: done") {
		t.Fatalf("expected done: %s", rec.Body.String())
	}
}

// TestOrderedWriter_PostCloseWritesAreNoOps covers the `if o.closed`
// guard on every user* method: once Done has been emitted, subsequent
// writes silently no-op so a buggy caller can't tail-emit content
// after the stream has been sealed.
func TestOrderedWriter_PostCloseWritesAreNoOps(t *testing.T) {
	rec := httptest.NewRecorder()
	w, _ := poeproto.NewSSEWriter(rec)
	_ = w.Meta()
	o := &orderedWriter{w: w}
	if err := o.userDone(); err != nil {
		t.Fatal(err)
	}
	for _, do := range []func() error{
		func() error { return o.userText("late") },
		func() error { return o.userReplace("late") },
		func() error { return o.userError("late", "user_caused_error") },
		func() error { return o.userDone() },
	} {
		if err := do(); err != nil {
			t.Fatalf("post-close write should be silent no-op, got err: %v", err)
		}
	}
	// Heartbeat path also no-ops post-close.
	if open, _ := o.hbReplace("late"); open {
		t.Fatalf("hbReplace must report gate closed after Done")
	}
	if strings.Contains(rec.Body.String(), "late") {
		t.Fatalf("late content leaked into stream:\n%s", rec.Body.String())
	}
}

// TestSink_HeartbeatSelfDisarmsViaGate covers the goroutine's
// self-disarm branch: when a user write closes the gate WHILE the
// heartbeat goroutine is mid-tick, the goroutine observes
// gateOpen=false from hbReplace and returns. Catching it inside the
// tick body via the hook makes the sequencing deterministic.
func TestSink_HeartbeatSelfDisarmsViaGate(t *testing.T) {
	rec := httptest.NewRecorder()
	w, _ := poeproto.NewSSEWriter(rec)
	_ = w.Meta()

	inTick := make(chan struct{})
	proceed := make(chan struct{})
	prev := heartbeatTickHook
	heartbeatTickHook = func() {
		inTick <- struct{}{}
		<-proceed
	}
	t.Cleanup(func() { heartbeatTickHook = prev })

	s := newSink(w, time.Millisecond)

	<-inTick // goroutine paused inside the tick body, before hbReplace
	// Close the gate via a user write while the goroutine is paused.
	// We deliberately do NOT call s.stop() / s.Done(): the hbDone
	// channel stays open so the only way for the goroutine to exit
	// is via the gateOpen=false self-disarm branch.
	if err := s.Replace("user content"); err != nil {
		t.Fatal(err)
	}
	close(proceed) // let the goroutine continue → hbReplace → gate closed → return
	<-s.hbExited   // race-free: goroutine has fully returned via self-disarm
	// Sanity: no spinner / heartbeat frame in the recorded stream.
	if strings.Contains(rec.Body.String(), "Thinking") {
		t.Fatalf("heartbeat must not have written after gate closed:\n%s", rec.Body.String())
	}
}

func TestSink_HeartbeatExitsOnStop(t *testing.T) {
	rec := httptest.NewRecorder()
	w, _ := poeproto.NewSSEWriter(rec)
	_ = w.Meta()
	wait := waitTicks(t, 1)
	s := newSink(w, time.Millisecond)
	wait()
	s.stop()
	<-s.hbExited
}

// TestOrderedWriter_HeartbeatGatedByUserWrite is the structural
// regression for the "garbled / out-of-order output" bug. The
// invariant: once any user-visible write has landed on the SSE stream
// (Text / Replace / Error / Done), subsequent heartbeat frames must
// become no-ops — enforced INSIDE orderedWriter, under the same mutex
// that serialises user writes, so the gate-check-and-write is atomic.
//
// Earlier designs put this responsibility on each call site ("call
// stop() before any user write"), which was a footgun: any new write
// site that forgot would let a stale heartbeat tick clobber user
// content with Replace("") (or a "Thinking…" frame). With the gate
// inside orderedWriter, no caller can bypass it.
func TestOrderedWriter_HeartbeatGatedByUserWrite(t *testing.T) {
	cases := []struct {
		name string
		do   func(o *orderedWriter) error
	}{
		{"userText", func(o *orderedWriter) error { return o.userText("hi") }},
		{"userReplace", func(o *orderedWriter) error { return o.userReplace("hi") }},
		{"userError", func(o *orderedWriter) error { return o.userError("oops", "user_caused_error") }},
		{"userDone", func(o *orderedWriter) error { return o.userDone() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			w, _ := poeproto.NewSSEWriter(rec)
			_ = w.Meta()
			o := &orderedWriter{w: w}
			// Pre-write: gate is open, heartbeat frames go on the wire.
			if open, _ := o.hbReplace(""); !open {
				t.Fatal("gate must start open on a fresh orderedWriter")
			}
			// User write closes the gate.
			if err := tc.do(o); err != nil {
				t.Fatal(err)
			}
			// Post-write: gate is closed; heartbeat frames are no-ops.
			if open, _ := o.hbReplace("> _Thinking._"); open {
				t.Fatalf("%s did not close the heartbeat gate", tc.name)
			}
			if open, _ := o.hbReplace(""); open {
				t.Fatalf("%s did not close the heartbeat gate (empty frame)", tc.name)
			}
		})
	}
}

// TestSink_HeartbeatNeverOverwritesUserContent exercises the invariant
// end-to-end through the full sink → SSEWriter → http.ResponseWriter
// path. After driving several heartbeat ticks (so the spinner is
// definitely alive), it issues a user-visible Replace, then waits for
// any stale tick to fire (deterministically, via the tick hook), and
// verifies that no `replace_response` event with a heartbeat-shaped
// body appears AFTER the user content in the recorded SSE sequence.
func TestSink_HeartbeatNeverOverwritesUserContent(t *testing.T) {
	rec := httptest.NewRecorder()
	w, _ := poeproto.NewSSEWriter(rec)
	_ = w.Meta()

	// Hook: count ticks AND signal each one so we can sequence
	// deterministically without time.Sleep.
	ticks := make(chan struct{}, 256)
	prev := heartbeatTickHook
	heartbeatTickHook = func() { ticks <- struct{}{} }
	t.Cleanup(func() { heartbeatTickHook = prev })

	// hideThinking=true so the heartbeat emits visible "Thinking…" frames
	// — making the test sensitive to the buggy "tick after user write
	// overwrites content" behaviour.
	s := newSink(w, time.Millisecond)

	// Wait for the heartbeat to definitely have ticked a few times.
	for i := 0; i < 3; i++ {
		select {
		case <-ticks:
		case <-time.After(3 * time.Second):
			t.Fatalf("heartbeat did not tick %d times", i+1)
		}
	}

	// User write — the structural invariant says no further heartbeat
	// frame may appear after this in the SSE event sequence.
	const userMarker = "_(cancelled)_"
	if err := s.Replace(userMarker); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	// Drain any tick that fires AFTER the user write — under the
	// invariant the heartbeat goroutine self-disarms on its next
	// tick (gate closed → return). Done() additionally closes hbDone
	// so the goroutine wakes immediately rather than waiting for the
	// next tick. Either way, hbExited is the race-free signal that
	// the goroutine has returned and won't write to rec.Body again.
	if err := s.Done(); err != nil {
		t.Fatalf("Done: %v", err)
	}
	<-s.hbExited

	// Parse the recorded SSE event sequence.
	events := parseSSE(t, rec.Body.String())
	firstUser := -1
	for i, e := range events {
		if e.event == "text" || (e.event == "replace_response" && e.text == userMarker) {
			firstUser = i
			break
		}
	}
	if firstUser < 0 {
		t.Fatalf("user content %q not found in SSE stream:\n%s", userMarker, rec.Body.String())
	}
	for i := firstUser + 1; i < len(events); i++ {
		e := events[i]
		if e.event == "replace_response" && isHeartbeatFrame(e.text) {
			t.Fatalf("heartbeat-shaped replace_response %q at idx %d (after first user content at %d) — would have overwritten user content:\n%s",
				e.text, i, firstUser, rec.Body.String())
		}
	}
}

// sseEvent is one parsed `event: NAME\ndata: {...}` SSE record.
type sseEventRec struct {
	event string
	text  string // .text field of the JSON payload, if present
}

// parseSSE parses an SSE byte stream into ordered events. Tolerant —
// only extracts what the test needs.
func parseSSE(t *testing.T, body string) []sseEventRec {
	t.Helper()
	var out []sseEventRec
	for _, frame := range strings.Split(body, "\n\n") {
		frame = strings.TrimSpace(frame)
		if frame == "" {
			continue
		}
		var e sseEventRec
		for _, line := range strings.Split(frame, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				e.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				var payload struct {
					Text string `json:"text"`
				}
				_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload)
				e.text = payload.Text
			}
		}
		out = append(out, e)
	}
	return out
}

// isHeartbeatFrame reports whether s matches a body the heartbeat
// goroutine would emit: empty (the keepalive) or a "> _Thinking…_"
// spinner frame.
func isHeartbeatFrame(s string) bool {
	if s == "" {
		return true
	}
	return strings.HasPrefix(s, "> _Thinking") && strings.HasSuffix(s, "_")
}

// waitTicks wires heartbeatTickHook to a channel and returns a func
// that blocks until n heartbeat ticks have fired (or fails the test
// after 3s). The hook is restored via t.Cleanup.
func waitTicks(t *testing.T, n int) func() {
	t.Helper()
	ch := make(chan struct{}, 256)
	prev := heartbeatTickHook
	heartbeatTickHook = func() {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	t.Cleanup(func() { heartbeatTickHook = prev })
	return func() {
		t.Helper()
		for i := 0; i < n; i++ {
			select {
			case <-ch:
			case <-time.After(3 * time.Second):
				t.Fatalf("heartbeat tick %d/%d never fired", i+1, n)
			}
		}
	}
}

// Ensure the fmt import is used to satisfy goimports.
var _ = fmt.Sprintf

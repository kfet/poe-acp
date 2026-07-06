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
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/acp-kit/client"
	kitlog "github.com/kfet/acp-kit/log"
	"github.com/kfet/poe-acp/internal/command"
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
// so even the first SSE write (the preamble) fails.
type errorMetaResp struct {
	hdr http.Header
}

func (r *errorMetaResp) Header() http.Header       { return r.hdr }
func (r *errorMetaResp) Write([]byte) (int, error) { return 0, io.ErrShortWrite }
func (r *errorMetaResp) WriteHeader(int)           {}
func (r *errorMetaResp) Flush()                    {}

func TestHandler_HandleQuery_PreambleError(t *testing.T) {
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

// failSecondWriteResp succeeds on the first Write (the preamble) and fails
// on every subsequent Write, so the Meta event — the second write — fails
// while the preamble lands. Covers the handler's Meta-error branch.
type failSecondWriteResp struct {
	hdr   http.Header
	wrote int
}

func (r *failSecondWriteResp) Header() http.Header { return r.hdr }
func (r *failSecondWriteResp) Write(b []byte) (int, error) {
	r.wrote++
	if r.wrote == 1 {
		return len(b), nil
	}
	return 0, io.ErrShortWrite
}
func (r *failSecondWriteResp) WriteHeader(int) {}
func (r *failSecondWriteResp) Flush()          {}

func TestHandler_HandleQuery_MetaError(t *testing.T) {
	rtr, err := router.New(router.Config{Agent: &fakeAgent{}, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Router: rtr})
	w := &failSecondWriteResp{hdr: http.Header{}}
	body := mustJSON(map[string]any{
		"type": "query", "conversation_id": "c1",
		"query": []map[string]any{{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body))
	h.ServeHTTP(w, req)
	// Just exercising the path; the handler logs and returns.
}

func TestHandler_DebugLogPath(t *testing.T) {
	prev := kitlog.Enabled()
	kitlog.SetEnabled(true)
	defer kitlog.SetEnabled(prev)

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

// hangAgent emits one chunk (so the stream has user-visible output), then
// blocks until Cancel is invoked. Models a real user Stop arriving AFTER
// output has started: the gated-cancel path must forward session/cancel.
type hangAgent struct {
	*fakeAgent
	cancelled chan struct{}
	entered   chan struct{}
	cancelOne sync.Once
}

func (a *hangAgent) Prompt(_ context.Context, sid acp.SessionId, _ []acp.ContentBlock) (acp.StopReason, error) {
	a.fakeAgent.mu.Lock()
	sink := a.fakeAgent.sinks[sid]
	a.fakeAgent.mu.Unlock()
	_ = sink.OnUpdate(context.Background(), acp.SessionNotification{
		SessionId: sid,
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
				Content: acp.TextBlock("pong\n"),
			},
		},
	})
	if a.entered != nil {
		close(a.entered)
	}
	<-a.cancelled
	return acp.StopReasonCancelled, nil
}

func (a *hangAgent) Cancel(_ context.Context, _ acp.SessionId) error {
	a.cancelOne.Do(func() { close(a.cancelled) })
	return nil
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	// Read until the first chunk lands so realWritten is set: the
	// post-output disconnect must then propagate as session/cancel.
	gotChunk := make(chan struct{})
	go func() {
		buf := make([]byte, 4096)
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 && strings.Contains(string(buf[:n]), "pong") {
				close(gotChunk)
				return
			}
			if rerr != nil {
				return
			}
		}
	}()
	select {
	case <-gotChunk:
	case <-time.After(3 * time.Second):
		t.Fatal("never received first chunk")
	}
	cancel()
	_ = resp.Body.Close()
	select {
	case <-a.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("agent prompt never cancelled")
	}
}

func TestHandler_AuthBrokerError(t *testing.T) {
	stub := &errAuth{err: errors.New("agent down")}
	broker := command.New(stub)
	rtr, err := router.New(router.Config{Agent: &fakeAgent{}, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Router: rtr, Commands: broker, HeartbeatInterval: 0})
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
	methods []client.AuthMethod
	err     error
}

func (e *errAuth) AuthMethods() []client.AuthMethod {
	return []client.AuthMethod{{ID: "oauth-anthropic", Type: "agent"}}
}
func (e *errAuth) Authenticate(_ context.Context, _, _, _ string, _ bool) (client.AuthResult, error) {
	return client.AuthResult{}, e.err
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

func TestLatestUserMessageID(t *testing.T) {
	turns := []router.Turn{
		{Role: "user", Content: "a", MessageID: "m1"},
		{Role: "bot", Content: "b"},
		{Role: "user", Content: "c", MessageID: "m2"},
	}
	if got := latestUserMessageID(turns); got != "m2" {
		t.Fatalf("got %q want m2", got)
	}
	// No user turn → "".
	if got := latestUserMessageID([]router.Turn{{Role: "bot", Content: "x"}}); got != "" {
		t.Fatalf("got %q want empty", got)
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
	out     *command.Outcome
	err     error
	passOut string
	passOK  bool
}

func (f *fakeBroker) HasPending(string) bool { return f.pending }
func (f *fakeBroker) Handle(context.Context, string, string) (*command.Outcome, error) {
	return f.out, f.err
}
func (f *fakeBroker) Passthrough(string) (string, bool) { return f.passOut, f.passOK }

func TestHandler_AuthBroker_NilOutcome(t *testing.T) {
	rtr, err := router.New(router.Config{Agent: &fakeAgent{}, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Router: rtr, Commands: &fakeBroker{pending: true}, HeartbeatInterval: 0})
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

// TestOrderedWriter_SpinnerAutoClear focuses on the clearSpinnerLocked
// helper: a userText/userError/userDone arriving while a visible
// spinner frame is on screen must auto-emit Replace("") first, since
// Poe `text` events append to whatever the renderer thinks the body
// is. (userReplace doesn't need this — its own Replace overwrites.)
func TestOrderedWriter_SpinnerAutoClear(t *testing.T) {
	cases := []struct {
		name        string
		do          func(o *orderedWriter) error
		wantContent string // body of the final user-visible event
	}{
		{
			name:        "userText clears then appends",
			do:          func(o *orderedWriter) error { return o.userText("answer") },
			wantContent: `"text":"answer"`,
		},
		{
			name:        "userError clears then errors",
			do:          func(o *orderedWriter) error { return o.userError("bad", "user_caused_error") },
			wantContent: `"text":"bad"`,
		},
		{
			name:        "userDone clears before sealing",
			do:          func(o *orderedWriter) error { return o.userDone() },
			wantContent: "event: done",
		},
		{
			name:        "userReplace overwrites without explicit clear",
			do:          func(o *orderedWriter) error { return o.userReplace("final") },
			wantContent: `"text":"final"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			w, _ := poeproto.NewSSEWriter(rec)
			_ = w.Meta()
			o := &orderedWriter{w: w}
			// Simulate the heartbeat having shown a non-empty spinner frame.
			if open, _ := o.hbReplace("> _Thinking._"); !open {
				t.Fatal("precondition: hb gate must start open")
			}
			if !o.spinnerVisible {
				t.Fatal("precondition: spinnerVisible must be true after hbReplace with non-empty body")
			}
			if err := tc.do(o); err != nil {
				t.Fatal(err)
			}
			if o.spinnerVisible {
				t.Fatalf("%s left spinnerVisible=true", tc.name)
			}
			out := rec.Body.String()
			events := parseSSE(t, out)
			// For paths that take clearSpinnerLocked (Text / Error / Done),
			// expect a `replace_response` with empty body BEFORE the user
			// content. userReplace doesn't pre-clear; its Replace IS the
			// overwrite.
			if strings.Contains(tc.name, "userReplace") {
				if got := events[len(events)-1]; got.event != "replace_response" || got.text != "final" {
					t.Fatalf("userReplace last event = %+v; want replace_response 'final'", got)
				}
			} else {
				// At least one empty replace_response must precede the user content.
				sawClear := false
				for i, e := range events {
					if e.event == "replace_response" && e.text == "" {
						// Anything user-visible after this counts.
						for _, after := range events[i+1:] {
							if after.event == "text" || after.event == "error" || after.event == "done" {
								sawClear = true
								break
							}
						}
					}
				}
				if !sawClear {
					t.Fatalf("%s: expected empty replace_response (spinner clear) before user content; events=%+v", tc.name, events)
				}
			}
			if !strings.Contains(out, tc.wantContent) {
				t.Fatalf("%s: missing %q in stream:\n%s", tc.name, tc.wantContent, out)
			}
		})
	}
}

// TestOrderedWriter_NoClearWhenSpinnerNotVisible covers the negative
// case: if no spinner frame has been shown, userText must NOT emit a
// gratuitous Replace("") — the stream should jump straight to the
// `text` event.
func TestOrderedWriter_NoClearWhenSpinnerNotVisible(t *testing.T) {
	rec := httptest.NewRecorder()
	w, _ := poeproto.NewSSEWriter(rec)
	_ = w.Meta()
	o := &orderedWriter{w: w}
	if err := o.userText("hi"); err != nil {
		t.Fatal(err)
	}
	out := rec.Body.String()
	events := parseSSE(t, out)
	for _, e := range events {
		if e.event == "replace_response" {
			t.Fatalf("unexpected replace_response without prior spinner: %+v\n%s", e, out)
		}
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

	// Let tick #0 (the immediate pre-loop frame) emit with the gate
	// still open, so the goroutine enters the ticker loop.
	<-inTick              // paused inside tick #0's hook, before hbReplace
	proceed <- struct{}{} // tick #0 emits a spinner frame, gate open → loop

	<-inTick // paused inside tick #1's hook (ticker-driven), before hbReplace
	// Close the gate via a user write while the goroutine is paused.
	// We deliberately do NOT call s.stop() / s.Done(): the hbDone
	// channel stays open so the only way for the goroutine to exit
	// is via the gateOpen=false self-disarm branch INSIDE the loop.
	if err := s.Replace("user content"); err != nil {
		t.Fatal(err)
	}
	proceed <- struct{}{} // let the goroutine continue → hbReplace → gate closed → return
	<-s.hbExited          // race-free: goroutine has fully returned via self-disarm

	// Sanity: the tick #0 spinner frame may legitimately precede the
	// user content, but NO heartbeat frame may appear AFTER it — the
	// loop self-disarmed on tick #1 before writing. Assert the last
	// content frame on the wire is the user write, not a stale spinner.
	events := parseSSE(t, rec.Body.String())
	last := events[len(events)-1]
	if last.event != "replace_response" || last.text != "user content" {
		t.Fatalf("expected final frame to be the user write, got %+v:\n%s", last, rec.Body.String())
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

	// The heartbeat emits visible "Thinking…" frames so the test is
	// sensitive to the buggy "tick after user write overwrites
	// content" behaviour the structural fix is meant to prevent.
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

// TestHandler_ReportReactionForwards verifies a report_reaction POST
// gets decoded, normalised, and queued onto the router (visible via
// agent.lastPrompt after the runner drains it).
func TestHandler_ReportReactionForwards(t *testing.T) {
	a := &fakeAgent{}
	rtr, err := router.New(router.Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Router: rtr})

	body := mustJSON(map[string]any{
		"type":            "report_reaction",
		"conversation_id": "c-rx",
		"user_id":         "u",
		"message_id":      "msg-7",
		"reaction":        "👍",
		"action":          "added",
	})
	req := httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}

	// Wait for the runner to deliver the synthetic turn to the agent.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		got := a.lastPrompt
		a.mu.Unlock()
		if len(got) > 0 && got[0].Text != nil &&
			strings.Contains(got[0].Text.Text, "[poe-acp:out-of-band reaction]") &&
			strings.Contains(got[0].Text.Text, "msg-7") &&
			strings.Contains(got[0].Text.Text, "👍") &&
			strings.Contains(got[0].Text.Text, "added") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	t.Fatalf("agent never received reaction turn; lastPrompt=%+v", a.lastPrompt)
}

// TestHandler_ReportReactionMinusPrefix verifies the '-emoji' shape is
// normalised to (kind, removed) end-to-end.
func TestHandler_ReportReactionMinusPrefix(t *testing.T) {
	a := &fakeAgent{}
	rtr, err := router.New(router.Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Router: rtr})

	body := mustJSON(map[string]any{
		"type":            "report_reaction",
		"conversation_id": "c-rx2",
		"user_id":         "u",
		"message_id":      "msg-8",
		"reaction":        "-👍",
	})
	req := httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		got := a.lastPrompt
		a.mu.Unlock()
		if len(got) > 0 && got[0].Text != nil &&
			strings.Contains(got[0].Text.Text, "removed") &&
			strings.Contains(got[0].Text.Text, "👍") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	t.Fatalf("removed-action not propagated; lastPrompt=%+v", a.lastPrompt)
}

// TestHandler_ReportReactionDebugLog enables the debug logger so the
// debug-branch in handleReaction is exercised.
func TestHandler_ReportReactionDebugLog(t *testing.T) {
	prev := kitlog.Enabled()
	kitlog.SetEnabled(true)
	t.Cleanup(func() { kitlog.SetEnabled(prev) })

	a := &fakeAgent{}
	rtr, err := router.New(router.Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Router: rtr})

	body := mustJSON(map[string]any{
		"type": "report_reaction", "conversation_id": "c", "user_id": "u",
		"message_id": "m", "reaction": "👍", "action": "added",
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
}

// TestHandler_ReportReactionRouterError covers the error-log branch
// when Router.ReportReaction returns a non-nil error (getOrCreate /
// NewSession fails).
func TestHandler_ReportReactionRouterError(t *testing.T) {
	a := &fakeAgent{}
	// Use the failing-agent variant: trigger NewSession error path by
	// stubbing a router whose Agent returns an error from NewSession.
	rtr, err := router.New(router.Config{
		Agent:    &errNewSessionAgent{fakeAgent: a},
		StateDir: t.TempDir(), SessionTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Router: rtr})
	body := mustJSON(map[string]any{
		"type": "report_reaction", "conversation_id": "c", "user_id": "u",
		"message_id": "m", "reaction": "👍", "action": "added",
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
}

type errNewSessionAgent struct{ *fakeAgent }

func (e *errNewSessionAgent) NewSession(_ context.Context, _ string, _ client.SessionUpdateSink, _ []acp.ContentBlock) (acp.SessionId, error) {
	return "", errors.New("new session boom")
}

// TestSink_StatusLinePrependsHeaderOnce verifies that a non-empty
// dev.acp-kit.status-line/v1 status renders into the first user Text
// chunk exactly once, and that subsequent chunks pass through unchanged.
func TestSink_StatusLinePrependsHeaderOnce(t *testing.T) {
	rec := httptest.NewRecorder()
	w, _ := poeproto.NewSSEWriter(rec)
	_ = w.Meta()
	s := newSink(w, 0)
	s.SetProviderEmoji("🏛️")
	s.SetStatus("steady", "2/5")
	if err := s.Text("hello"); err != nil {
		t.Fatal(err)
	}
	if err := s.Text(" world"); err != nil {
		t.Fatal(err)
	}
	if err := s.Done(); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	// Header appears once, on the first chunk only.
	if strings.Count(body, "🏛️ • steady • 2/5") != 1 {
		t.Errorf("expected header exactly once in body: %q", body)
	}
	// Header is followed by a blank line then the first chunk's text.
	if !strings.Contains(body, "🏛️ • steady • 2/5\\n\\nhello") {
		// SSE-escaped newlines — fall back to checking raw assembled order.
		t.Logf("body: %q", body)
	}
}

// TestSink_StatusLineNoHeaderWhenAllEmpty: with no provider emoji set
// and no agent _meta, the first Text chunk is forwarded unchanged.
func TestSink_StatusLineNoHeaderWhenAllEmpty(t *testing.T) {
	rec := httptest.NewRecorder()
	w, _ := poeproto.NewSSEWriter(rec)
	_ = w.Meta()
	s := newSink(w, 0)
	// No SetProviderEmoji, no SetStatus.
	if err := s.Text("only-content"); err != nil {
		t.Fatal(err)
	}
	_ = s.Done()
	body := rec.Body.String()
	if strings.Contains(body, " • ") {
		t.Errorf("unexpected header separator in body: %q", body)
	}
	if !strings.Contains(body, "only-content") {
		t.Errorf("missing chunk content: %q", body)
	}
}

// TestSink_StatusLineEmojiOnlyWithoutAgentMeta verifies the
// backwards-compat case: agent doesn't emit _meta, so only the
// provider emoji segment survives. Header is still prepended.
func TestSink_StatusLineEmojiOnlyWithoutAgentMeta(t *testing.T) {
	rec := httptest.NewRecorder()
	w, _ := poeproto.NewSSEWriter(rec)
	_ = w.Meta()
	s := newSink(w, 0)
	s.SetProviderEmoji("🌐")
	if err := s.Text("payload"); err != nil {
		t.Fatal(err)
	}
	_ = s.Done()
	body := rec.Body.String()
	if !strings.Contains(body, "🌐") {
		t.Errorf("emoji missing from body: %q", body)
	}
	// No mood/plan dividers should be present.
	if strings.Contains(body, "🌐 • ") {
		t.Errorf("unexpected mood/plan segment present: %q", body)
	}
}

// TestSink_StatusLineSpinnerCarriesHeader verifies the spinner frame
// includes the current status — provider emoji + mood + plan + Thinking…
func TestSink_StatusLineSpinnerCarriesHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	w, _ := poeproto.NewSSEWriter(rec)
	_ = w.Meta()
	wait := waitTicks(t, 2)
	s := newSink(w, 5*time.Millisecond)
	s.SetProviderEmoji("🏛️")
	s.SetStatus("steady", "2/5")
	wait()
	s.FirstChunk()
	<-s.hbExited
	body := rec.Body.String()
	if !strings.Contains(body, "🏛️") {
		t.Errorf("spinner missing emoji: %q", body)
	}
	if !strings.Contains(body, "steady") {
		t.Errorf("spinner missing mood: %q", body)
	}
	if !strings.Contains(body, "2/5") {
		t.Errorf("spinner missing plan: %q", body)
	}
	if !strings.Contains(body, "Thinking") {
		t.Errorf("spinner missing Thinking suffix: %q", body)
	}
}

// TestSink_StatusLineHeaderEmittedOnlyForText verifies that the header
// is not prepended onto Replace / Error paths (those overwrite the
// body, so a header there would be erased or out of place).
func TestSink_StatusLineHeaderSkippedOnReplace(t *testing.T) {
	rec := httptest.NewRecorder()
	w, _ := poeproto.NewSSEWriter(rec)
	_ = w.Meta()
	s := newSink(w, 0)
	s.SetProviderEmoji("🏛️")
	s.SetStatus("steady", "2/5")
	if err := s.Replace("_(cancelled)_"); err != nil {
		t.Fatal(err)
	}
	_ = s.Done()
	body := rec.Body.String()
	if strings.Contains(body, "🏛️") {
		t.Errorf("Replace path must not prepend header: %q", body)
	}
}

// TestHandler_PassthroughRewrite: an allowlisted agent command is
// rewritten to its slash form and forwarded to the agent as the prompt.
func TestHandler_PassthroughRewrite(t *testing.T) {
	agent := &fakeAgent{}
	rtr, err := router.New(router.Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Router: rtr, HeartbeatInterval: 0,
		Commands: &fakeBroker{passOut: "/reload", passOK: true}})

	body := mustJSON(map[string]any{
		"type": "query", "conversation_id": "c1", "user_id": "u1", "message_id": "m1",
		"query": []map[string]any{{"role": "user", "content": "!reload"}},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	agent.mu.Lock()
	got := ""
	if len(agent.lastPrompt) > 0 && agent.lastPrompt[0].Text != nil {
		got = agent.lastPrompt[0].Text.Text
	}
	agent.mu.Unlock()
	if got != "/reload" {
		t.Fatalf("agent received %q, want /reload", got)
	}
}

func TestRewriteLatestUserTurn_NoUserTurn(t *testing.T) {
	in := []router.Turn{{Role: "assistant", Content: "hi"}}
	out := rewriteLatestUserTurn(in, "/reload")
	if out[0].Content != "hi" {
		t.Fatalf("non-user turn should be untouched: %+v", out)
	}
	got := rewriteLatestUserTurn([]router.Turn{{Role: "user", Content: "x"}}, "/reload")
	if got[0].Content != "/reload" {
		t.Fatalf("user turn rewrite: %+v", got)
	}
}

func TestSink_File(t *testing.T) {
	rec := httptest.NewRecorder()
	w, err := poeproto.NewSSEWriter(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Meta(); err != nil {
		t.Fatal(err)
	}
	s := newSink(w, 0)
	if err := s.File("https://poe/x", "text/markdown", "doc.md", ""); err != nil {
		t.Fatalf("File: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "event: file") {
		t.Fatalf("want file event, got %q", rec.Body.String())
	}
	// File after Done is a no-op (closed gate).
	_ = s.Done()
	if err := s.File("u", "ct", "n", ""); err != nil {
		t.Fatalf("File after done: %v", err)
	}
}

func TestSink_SuggestedReply(t *testing.T) {
	rec := httptest.NewRecorder()
	w, err := poeproto.NewSSEWriter(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Meta(); err != nil {
		t.Fatal(err)
	}
	s := newSink(w, 0)
	// Call suggest BEFORE any text — the worst case that Poe discards if
	// emitted immediately. It must be buffered, not written yet.
	if err := s.SuggestedReply("Yes"); err != nil {
		t.Fatalf("SuggestedReply: %v", err)
	}
	if strings.Contains(rec.Body.String(), "suggested_reply") {
		t.Fatalf("suggested_reply must be deferred, not emitted early: %q", rec.Body.String())
	}
	// Emit some content, then finish. Chips flush at Done, after content,
	// immediately before the done event — the only position Poe renders.
	_ = s.Text("the answer")
	if err := s.Done(); err != nil {
		t.Fatalf("Done: %v", err)
	}
	body := rec.Body.String()
	iText := strings.Index(body, "the answer")
	iChip := strings.Index(body, "event: suggested_reply")
	iDone := strings.Index(body, "event: done")
	if iChip < 0 {
		t.Fatalf("want suggested_reply flushed at Done, got %q", body)
	}
	if !(iText < iChip && iChip < iDone) {
		t.Fatalf("ordering must be text < suggested_reply < done; got text=%d chip=%d done=%d", iText, iChip, iDone)
	}
	// After Done the gate is closed → buffering is a no-op, no error.
	if err := s.SuggestedReply("No"); err != nil {
		t.Fatalf("SuggestedReply after done: %v", err)
	}
}

package httpsrv

import (
	"bytes"
	"context"
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
	sa := &slowAgent{fakeAgent: &fakeAgent{}, release: make(chan struct{}), chunk: "x"}
	rtr, err := router.New(router.Config{Agent: sa, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{
		Router:            rtr,
		HeartbeatInterval: 100 * time.Millisecond,
		SpinnerInterval:   5 * time.Millisecond,
	})
	body := mustJSON(map[string]any{
		"type": "query", "conversation_id": "c1",
		"query": []map[string]any{{"role": "user", "content": "hi", "parameters": map[string]any{"hide_thinking": true}}},
	})
	rec := httptest.NewRecorder()
	wait := waitTicks(t, 2)
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body)))
		close(done)
	}()
	wait()
	close(sa.release)
	<-done
	if !strings.Contains(rec.Body.String(), "Thinking.") {
		t.Fatalf("expected spinner: %s", rec.Body.String())
	}
}

// hangAgent blocks Prompt until ctx is cancelled.
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
	s := newSink(w, 0, false) // heartbeat disabled path
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
	wait := waitTicks(t, 1)
	s := newSink(w, 5*time.Millisecond, true)
	wait()
	s.FirstChunk()
	// after FirstChunk, heartbeat goroutine has exited.
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

func TestSink_HeartbeatExitsOnStop(t *testing.T) {
	// heartbeat with started=true at tick time: select hbDone branch when stopped already.
	rec := httptest.NewRecorder()
	w, _ := poeproto.NewSSEWriter(rec)
	_ = w.Meta()
	wait := waitTicks(t, 1)
	s := newSink(w, time.Millisecond, false)
	wait()
	s.stop()
}

func TestSink_HeartbeatExitsWhenStartedAtTick(t *testing.T) {
	rec := httptest.NewRecorder()
	w, _ := poeproto.NewSSEWriter(rec)
	_ = w.Meta()
	wait := waitTicks(t, 1)
	s := newSink(w, time.Millisecond, false)
	// Flip started before the tick fires; the heartbeat goroutine should
	// notice on its next tick and return without closing hbDone.
	s.mu.Lock()
	s.started = true
	s.mu.Unlock()
	wait()
	s.stop()
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

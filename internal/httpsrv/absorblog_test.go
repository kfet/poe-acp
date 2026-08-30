package httpsrv

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kfet/poe-acp/internal/router"
)

// syncBuf is a race-safe log sink: the stdlib logger writes from the
// disconnect-watch goroutine while the test goroutine reads.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The absorb path must be legible at DEFAULT verbosity (kitlog debug off,
// which is production's setting). Every line asserted here was previously
// either debug-only or absent, which is the bug: an absorbed turn and its
// redrive were invisible in production logs.
func TestAbsorbPathLogsAtDefaultVerbosity(t *testing.T) {
	var out syncBuf
	origOut := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(origOut)

	a := &absorbAgent{fakeAgent: &fakeAgent{}, entered: make(chan struct{}), release: make(chan struct{})}
	rtr, err := router.New(router.Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Router: rtr, HeartbeatInterval: 0})

	body := mustJSON(map[string]any{
		"type": "query", "conversation_id": "c1", "user_id": "u", "message_id": "req1",
		"query": []map[string]any{{"role": "user", "content": "hi", "message_id": "m1"}},
	})

	decided := make(chan struct{})
	h.absorbDecidedHook = func() { close(decided) }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body)).WithContext(ctx)
		h.ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()
	<-a.entered
	cancel()
	<-decided // the latch line is written before the hook fires
	close(a.release)
	<-done // the completion line is written before ServeHTTP returns

	got := out.String()
	// RECV carries BOTH ids: msg is the per-query id (unique per request),
	// umsg is the buffer key (stable across a redrive).
	for _, want := range []string{
		"RECV conv=c1 msg=req1 umsg=m1 bytes=",
		"WARN absorbed pre-output client drop: conv=c1 umsg=m1 elapsed=",
		"absorbed turn complete: conv=c1 umsg=m1 duration=",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q at default verbosity in log:\n%s", want, got)
		}
	}

	// The redrive: same umsg, different per-query msg — the pair is now
	// correlatable from the log alone.
	redrive := mustJSON(map[string]any{
		"type": "query", "conversation_id": "c1", "user_id": "u", "message_id": "req2",
		"query": []map[string]any{{"role": "user", "content": "hi", "message_id": "m1"}},
	})
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(redrive)))

	got = out.String()
	for _, want := range []string{
		"RECV conv=c1 msg=req2 umsg=m1 bytes=",
		"redrive served from buffer: conv=c1 msg=req2 umsg=m1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q at default verbosity in log:\n%s", want, got)
		}
	}
}

// An absorbed turn with no user message_id has no buffer key: nothing is
// buffered and no completion line follows. The latch line must say so,
// otherwise the missing companion line reads as a lost answer.
func TestAbsorbLatchLogsUnbufferableDrop(t *testing.T) {
	var out syncBuf
	origOut := log.Writer()
	log.SetOutput(&out)
	defer log.SetOutput(origOut)

	a := &absorbAgent{fakeAgent: &fakeAgent{}, entered: make(chan struct{}), release: make(chan struct{})}
	rtr, err := router.New(router.Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{Router: rtr, HeartbeatInterval: 0})

	// No message_id on the user turn → answerKey is empty.
	body := mustJSON(map[string]any{
		"type": "query", "conversation_id": "c2", "user_id": "u", "message_id": "req1",
		"query": []map[string]any{{"role": "user", "content": "hi"}},
	})

	decided := make(chan struct{})
	h.absorbDecidedHook = func() { close(decided) }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/poe", bytes.NewReader(body)).WithContext(ctx)
		h.ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()
	<-a.entered
	cancel()
	<-decided
	close(a.release)
	<-done

	got := out.String()
	if !strings.Contains(got, "answer NOT buffered") {
		t.Fatalf("latch line must state the answer is unbufferable:\n%s", got)
	}
	if strings.Contains(got, "absorbed turn complete") {
		t.Fatalf("no answer was buffered, so no completion line is due:\n%s", got)
	}
}

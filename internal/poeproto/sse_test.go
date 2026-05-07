package poeproto

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kfet/poe-acp/internal/debuglog"
)

func TestLatestUserText(t *testing.T) {
	r := &Request{Query: []Message{
		{Role: "user", Content: "first"},
		{Role: "bot", Content: "reply"},
		{Role: "user", Content: "last"},
	}}
	if got := r.LatestUserText(); got != "last" {
		t.Fatalf("got %q", got)
	}
	r2 := &Request{Query: []Message{{Role: "bot", Content: "x"}}}
	if got := r2.LatestUserText(); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestLatestParameters_None(t *testing.T) {
	r := &Request{Query: []Message{{Role: "bot"}}}
	if r.LatestParameters() != nil {
		t.Fatal("expected nil")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestDecode_Errors(t *testing.T) {
	if _, err := Decode(strings.NewReader("not json")); err == nil {
		t.Fatal("expected json error")
	}
	if _, err := Decode(strings.NewReader(`{"query":[]}`)); err == nil {
		t.Fatal("expected missing-type error")
	}
}

func TestDecode_DebugPath(t *testing.T) {
	prev := debuglog.Enabled()
	debuglog.SetEnabled(true)
	defer debuglog.SetEnabled(prev)

	body := `{"type":"query","query":[{"role":"user","content":"hi"}]}`
	r, err := Decode(strings.NewReader(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if r.Type != "query" {
		t.Fatalf("type=%q", r.Type)
	}

	// Trigger read error in debug branch.
	if _, err := Decode(errReader{}); err == nil {
		t.Fatal("expected read error")
	}
}

// nonFlusher writer to exercise NewSSEWriter error.
type nonFlusher struct{ http.ResponseWriter }

func (nonFlusher) Header() http.Header         { return http.Header{} }
func (nonFlusher) Write(b []byte) (int, error) { return len(b), nil }
func (nonFlusher) WriteHeader(int)             {}

func TestNewSSEWriter_NotFlushable(t *testing.T) {
	if _, err := NewSSEWriter(nonFlusher{}); err == nil {
		t.Fatal("expected error")
	}
}

func TestSSEWriter_Lifecycle(t *testing.T) {
	rec := httptest.NewRecorder()
	s, err := NewSSEWriter(rec)
	if err != nil {
		t.Fatalf("NewSSEWriter: %v", err)
	}
	if rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("ct=%q", rec.Header().Get("Content-Type"))
	}
	if err := s.Meta(); err != nil {
		t.Fatal(err)
	}
	if err := s.Text(""); err != nil { // skip path
		t.Fatal(err)
	}
	if err := s.Text("hi"); err != nil {
		t.Fatal(err)
	}
	if err := s.Replace("done"); err != nil {
		t.Fatal(err)
	}
	if err := s.Error("oops", "InternalError"); err != nil {
		t.Fatal(err)
	}
	if err := s.Error("oops2", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Done(); err != nil {
		t.Fatal(err)
	}
	// After Done, further writes should fail.
	if err := s.Text("more"); err == nil {
		t.Fatal("expected closed error")
	}

	body := rec.Body.String()
	for _, want := range []string{
		"event: meta",
		"event: text\ndata: {\"text\":\"hi\"}",
		"event: replace_response",
		"event: error",
		"\"error_type\":\"InternalError\"",
		"event: done",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in body:\n%s", want, body)
		}
	}
}

// errFlushWriter returns a write error.
type errFlushWriter struct {
	httptest.ResponseRecorder
}

func (e *errFlushWriter) Write(b []byte) (int, error) { return 0, io.ErrShortWrite }
func (e *errFlushWriter) Flush()                      {}

func TestSSEWriter_EventWriteError(t *testing.T) {
	w := &errFlushWriter{}
	s := &SSEWriter{w: w, f: w}
	if err := s.Meta(); err == nil {
		t.Fatal("expected write error")
	}
}

func TestSSEWriter_EventMarshalError(t *testing.T) {
	rec := httptest.NewRecorder()
	s, _ := NewSSEWriter(rec)
	// channels can't be marshalled.
	if err := s.event("x", make(chan int)); err == nil {
		t.Fatal("expected marshal error")
	}
}

func TestSSEWriter_ConcurrentSafe(t *testing.T) {
	rec := httptest.NewRecorder()
	s, _ := NewSSEWriter(rec)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = s.Text("x") }()
	}
	wg.Wait()
}

func TestBearerAuth(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })

	// secret empty => always 401.
	h := BearerAuth("", next)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 401 || called {
		t.Fatalf("empty secret: code=%d called=%v", rec.Code, called)
	}

	h = BearerAuth("s3cret", next)

	// no header.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 401 {
		t.Fatalf("missing: %d", rec.Code)
	}

	// wrong prefix.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Basic xyz")
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("wrong prefix: %d", rec.Code)
	}

	// wrong secret.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer nope")
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("wrong secret: %d", rec.Code)
	}

	// good.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	h.ServeHTTP(rec, req)
	if !called {
		t.Fatal("expected call")
	}
}

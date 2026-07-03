package poeproto

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	kitlog "github.com/kfet/acp-kit/log"
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
	prev := kitlog.Enabled()
	kitlog.SetEnabled(true)
	defer kitlog.SetEnabled(prev)

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

	// Regression: bodies larger than 16 KiB must still decode under
	// debug logging — earlier the body was truncated by io.LimitReader
	// before being handed to the JSON decoder, causing 400s on any
	// real Poe query carrying attachment parsed_content.
	bigContent := strings.Repeat("x", 64*1024)
	big := `{"type":"query","query":[{"role":"user","content":"` + bigContent + `"}]}`
	r2, err := Decode(strings.NewReader(big))
	if err != nil {
		t.Fatalf("decode large body: %v", err)
	}
	if got := r2.LatestUserText(); len(got) != len(bigContent) {
		t.Fatalf("content len=%d want=%d", len(got), len(bigContent))
	}
}

func TestCapWriter(t *testing.T) {
	// Under-fill: buffer grows, never truncates.
	c := &capWriter{max: 10}
	n, err := c.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("write 5: n=%d err=%v", n, err)
	}
	if string(c.buf) != "hello" || c.truncated {
		t.Fatalf("under: buf=%q truncated=%v", c.buf, c.truncated)
	}

	// Straddle the boundary: keep first 5 more bytes, mark truncated.
	n, err = c.Write([]byte("world!!!extra"))
	if err != nil || n != 13 {
		t.Fatalf("straddle: n=%d err=%v", n, err)
	}
	if string(c.buf) != "helloworld" || !c.truncated {
		t.Fatalf("straddle: buf=%q truncated=%v", c.buf, c.truncated)
	}

	// Post-full: nothing added, still truncated, n still reports input len.
	n, err = c.Write([]byte("more"))
	if err != nil || n != 4 {
		t.Fatalf("post: n=%d err=%v", n, err)
	}
	if string(c.buf) != "helloworld" {
		t.Fatalf("post: buf=%q", c.buf)
	}

	// Empty write at full does not flip anything weird.
	n, err = c.Write(nil)
	if err != nil || n != 0 {
		t.Fatalf("empty: n=%d err=%v", n, err)
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

func TestSSEWriter_Preamble(t *testing.T) {
	rec := httptest.NewRecorder()
	s, err := NewSSEWriter(rec)
	if err != nil {
		t.Fatalf("NewSSEWriter: %v", err)
	}
	// X-Accel-Buffering: no must be set to defeat proxy buffering.
	if got := rec.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("X-Accel-Buffering=%q, want no", got)
	}
	if err := s.Preamble(); err != nil {
		t.Fatalf("Preamble: %v", err)
	}
	body := rec.Body.String()
	// Must be a comment frame: ": " + padding + "\n\n".
	if !strings.HasPrefix(body, ": ") {
		t.Fatalf("preamble must start with comment marker, got %q", body[:min(8, len(body))])
	}
	if !strings.HasSuffix(body, "\n\n") {
		t.Fatalf("preamble must end with blank line, got %q", body)
	}
	if len(body) < preamblePadding {
		t.Fatalf("preamble too short: %d bytes, want >= %d", len(body), preamblePadding)
	}
	// Meta after preamble still works and appends a valid event.
	if err := s.Meta(); err != nil {
		t.Fatalf("Meta after preamble: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "event: meta") {
		t.Fatalf("meta event missing after preamble")
	}
}

func TestSSEWriter_PreambleAfterClose(t *testing.T) {
	rec := httptest.NewRecorder()
	s, _ := NewSSEWriter(rec)
	if err := s.Done(); err != nil {
		t.Fatalf("Done: %v", err)
	}
	if err := s.Preamble(); err == nil {
		t.Fatal("expected closed error from Preamble after Done")
	}
}

func TestSSEWriter_PreambleWriteError(t *testing.T) {
	w := &errFlushWriter{}
	s := &SSEWriter{w: w, f: w}
	if err := s.Preamble(); err == nil {
		t.Fatal("expected write error from Preamble")
	}
}

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

func TestSSEWriter_File(t *testing.T) {
	rec := httptest.NewRecorder()
	s, err := NewSSEWriter(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.File("https://poe/x", "text/markdown", "doc.md", "ref123"); err != nil {
		t.Fatalf("File: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: file") || !strings.Contains(body, "https://poe/x") ||
		!strings.Contains(body, "ref123") || !strings.Contains(body, "doc.md") {
		t.Fatalf("file event missing fields: %q", body)
	}

	// Non-inline: inline_ref MUST serialize as null (not ""), else Poe
	// renders a "[]: url" link-reference instead of an attachment chip.
	rec2 := httptest.NewRecorder()
	s2, err := NewSSEWriter(rec2)
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.File("https://poe/y", "text/plain", "f.txt", ""); err != nil {
		t.Fatalf("File (non-inline): %v", err)
	}
	b2 := rec2.Body.String()
	if !strings.Contains(b2, "\"inline_ref\":null") {
		t.Fatalf("non-inline file event must send inline_ref:null, got %q", b2)
	}
	if strings.Contains(b2, "\"inline_ref\":\"\"") {
		t.Fatalf("non-inline file event sent empty-string inline_ref (bug): %q", b2)
	}
}

func TestSSEWriter_SuggestedReply(t *testing.T) {
	rec := httptest.NewRecorder()
	s, err := NewSSEWriter(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SuggestedReply("Yes"); err != nil {
		t.Fatalf("SuggestedReply: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: suggested_reply") || !strings.Contains(body, `"text":"Yes"`) {
		t.Fatalf("suggested_reply event missing fields: %q", body)
	}
}

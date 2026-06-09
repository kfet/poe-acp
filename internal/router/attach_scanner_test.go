package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kfet/poe-acp/internal/poeupload"
)

// scanSink records Text and File calls in order.
type scanSink struct {
	mu    sync.Mutex
	texts []string
	files []capturedFile
}

func (s *scanSink) Text(t string) error {
	s.mu.Lock()
	s.texts = append(s.texts, t)
	s.mu.Unlock()
	return nil
}
func (s *scanSink) File(url, ct, name, ref string) error {
	s.mu.Lock()
	s.files = append(s.files, capturedFile{url, ct, name, ref})
	s.mu.Unlock()
	return nil
}
func (s *scanSink) Replace(string) error       { return nil }
func (s *scanSink) Error(string, string) error { return nil }
func (s *scanSink) Done() error                { return nil }
func (s *scanSink) FirstChunk()                {}
func (s *scanSink) SetProviderEmoji(string)    {}
func (s *scanSink) SetStatus(string, string)   {}
func (s *scanSink) joined() string             { return strings.Join(s.texts, "") }

func newTestScanner(t *testing.T, sink ChunkSink, cwd string) (*attachScanner, *int) {
	t.Helper()
	var uploads int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uploads++
		_, _ = io.WriteString(w, `{"attachment_url":"https://poe/att","mime_type":"text/plain"}`)
	}))
	t.Cleanup(srv.Close)
	up := poeupload.New("k", srv.URL, srv.Client())
	return &attachScanner{up: up, sink: sink, cwd: cwd}, &uploads
}

func TestScanner_PlainTextStreamsThrough(t *testing.T) {
	sink := &scanSink{}
	sc, _ := newTestScanner(t, sink, "")
	sc.Feed("hello ")
	sc.Feed("world")
	sc.Flush()
	if got := sink.joined(); got != "hello world" {
		t.Fatalf("got %q", got)
	}
	if len(sink.files) != 0 {
		t.Fatalf("unexpected files: %v", sink.files)
	}
}

func TestScanner_DirectiveUploadsAndEmitsFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "doc.md")
	if err := os.WriteFile(fp, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	sink := &scanSink{}
	sc, n := newTestScanner(t, sink, dir)
	sc.Feed("before\n")
	sc.Feed(`<!--poe-attach path="doc.md" name="My Doc"-->` + "\n")
	sc.Feed("after")
	sc.Flush()

	if *n != 1 {
		t.Fatalf("uploads = %d", *n)
	}
	if len(sink.files) != 1 {
		t.Fatalf("files = %v", sink.files)
	}
	f := sink.files[0]
	if f.url != "https://poe/att" || f.name != "My Doc" || f.ref != "" {
		t.Fatalf("file = %+v", f)
	}
	if got := sink.joined(); got != "before\nafter" {
		t.Fatalf("text = %q (directive should be stripped)", got)
	}
}

func TestScanner_InlineEmitsMarkdownRef(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "c.png"), []byte("x"), 0o644)
	sink := &scanSink{}
	sc, _ := newTestScanner(t, sink, dir)
	sc.Feed(`<!--poe-attach path="c.png" name="Chart" inline-->` + "\n")
	sc.Flush()
	if len(sink.files) != 1 || sink.files[0].ref == "" {
		t.Fatalf("want inline ref set, files=%+v", sink.files)
	}
	ref := sink.files[0].ref
	if !strings.Contains(sink.joined(), "![Chart]["+ref+"]") {
		t.Fatalf("want inline markdown ref, got %q", sink.joined())
	}
}

func TestScanner_DirectiveSplitAcrossChunks(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0o644)
	sink := &scanSink{}
	sc, n := newTestScanner(t, sink, dir)
	// Feed the directive one fragment at a time.
	for _, frag := range []string{"<!--", "poe-", `attach path="f`, `.txt"`, "-->", "\n"} {
		sc.Feed(frag)
	}
	sc.Flush()
	if *n != 1 || len(sink.files) != 1 {
		t.Fatalf("uploads=%d files=%v", *n, sink.files)
	}
	if strings.Contains(sink.joined(), "poe-attach") {
		t.Fatalf("directive leaked into text: %q", sink.joined())
	}
}

func TestScanner_NonDirectiveCommentPassesThrough(t *testing.T) {
	sink := &scanSink{}
	sc, n := newTestScanner(t, sink, "")
	sc.Feed("<!-- just a comment -->\n")
	sc.Flush()
	if *n != 0 {
		t.Fatalf("no upload expected, got %d", *n)
	}
	if got := sink.joined(); got != "<!-- just a comment -->\n" {
		t.Fatalf("comment should pass through, got %q", got)
	}
}

func TestScanner_MissingPathPassesThrough(t *testing.T) {
	sink := &scanSink{}
	sc, n := newTestScanner(t, sink, "")
	sc.Feed(`<!--poe-attach name="x"-->` + "\n")
	sc.Flush()
	if *n != 0 || len(sink.files) != 0 {
		t.Fatalf("expected no-op, uploads=%d files=%v", *n, sink.files)
	}
	if !strings.Contains(sink.joined(), "poe-attach") {
		t.Fatalf("malformed directive should pass through as text, got %q", sink.joined())
	}
}

func TestScanner_UploadErrorSurfacesNote(t *testing.T) {
	sink := &scanSink{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0o644)
	sc := &attachScanner{up: poeupload.New("k", srv.URL, srv.Client()), sink: sink, cwd: dir}
	sc.Feed(`<!--poe-attach path="f.txt"-->` + "\n")
	sc.Flush()
	if len(sink.files) != 0 {
		t.Fatalf("no file event on error, got %v", sink.files)
	}
	if !strings.Contains(sink.joined(), "attachment failed") {
		t.Fatalf("want failure note, got %q", sink.joined())
	}
}

func TestScanner_DirectiveAtFlushNoNewline(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0o644)
	sink := &scanSink{}
	sc, n := newTestScanner(t, sink, dir)
	sc.Feed(`<!--poe-attach path="f.txt"-->`) // no trailing newline
	sc.Flush()
	if *n != 1 || len(sink.files) != 1 {
		t.Fatalf("uploads=%d files=%v", *n, sink.files)
	}
}

func TestNewAttachScanner_NilWhenDisabled(t *testing.T) {
	r := &Router{}
	if sc := r.newAttachScanner(&scanSink{}, ""); sc != nil {
		t.Fatal("want nil scanner when uploader unset")
	}
	r.uploader = poeupload.New("k", "", nil)
	if sc := r.newAttachScanner(&scanSink{}, "/tmp"); sc == nil {
		t.Fatal("want scanner when uploader set")
	}
}

var _ = context.Background

func newUploaderForURL(srv *httptest.Server) *poeupload.Uploader {
	return poeupload.New("k", srv.URL, srv.Client())
}

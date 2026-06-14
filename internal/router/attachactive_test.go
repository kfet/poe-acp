package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

func mkRouterWithUploader(t *testing.T, uploadOK bool) *Router {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !uploadOK {
			w.WriteHeader(500)
			return
		}
		_, _ = io.WriteString(w, `{"attachment_url":"https://poe/x","mime_type":"text/markdown"}`)
	}))
	t.Cleanup(srv.Close)
	agent := newFakeAgent(func(_ context.Context, _ *fakeAgent, _ acp.SessionId, _ string) (acp.StopReason, error) {
		return acp.StopReasonEndTurn, nil
	})
	r, err := New(Config{
		Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour,
		AccessKey: "k", UploadEndpoint: srv.URL, HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAttachActive_NoUploader(t *testing.T) {
	r := &Router{active: map[string]activeTurn{}}
	if err := r.AttachActive("c", "/p", "", false); err == nil {
		t.Fatal("want error when uploader nil")
	}
}

func TestAttachActive_NoActiveTurn(t *testing.T) {
	r := mkRouterWithUploader(t, true)
	if err := r.AttachActive("nope", "/p", "", false); err == nil {
		t.Fatal("want error when no active turn")
	}
}

func TestAttachActive_MissingPath(t *testing.T) {
	r := mkRouterWithUploader(t, true)
	r.setActiveTurn("c", &captureSink{}, t.TempDir())
	if err := r.AttachActive("c", "", "", false); err == nil {
		t.Fatal("want error for empty path")
	}
}

func TestAttachActive_UploadError(t *testing.T) {
	r := mkRouterWithUploader(t, false) // server 500s
	dir := t.TempDir()
	fp := filepath.Join(dir, "f.txt")
	os.WriteFile(fp, []byte("x"), 0o644)
	r.setActiveTurn("c", &captureSink{}, dir)
	if err := r.AttachActive("c", "f.txt", "", false); err == nil {
		t.Fatal("want upload error")
	}
}

func TestAttachActive_Success_RelativePath(t *testing.T) {
	r := mkRouterWithUploader(t, true)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "doc.md"), []byte("hi"), 0o644)
	sink := &captureSink{}
	r.setActiveTurn("c", sink, dir)
	if err := r.AttachActive("c", "doc.md", "", false); err != nil {
		t.Fatalf("AttachActive: %v", err)
	}
	if len(sink.files) != 1 || sink.files[0].url != "https://poe/x" {
		t.Fatalf("files = %+v", sink.files)
	}
	// name defaulted to basename
	if sink.files[0].name != "doc.md" {
		t.Errorf("name = %q", sink.files[0].name)
	}
}

func TestAttachActive_Inline_AbsolutePath(t *testing.T) {
	r := mkRouterWithUploader(t, true)
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.png")
	os.WriteFile(fp, []byte("x"), 0o644)
	sink := &captureSink{}
	r.setActiveTurn("c", sink, dir)
	if err := r.AttachActive("c", fp, "Chart", true); err != nil {
		t.Fatalf("AttachActive: %v", err)
	}
	if len(sink.files) != 1 || sink.files[0].ref == "" || sink.files[0].name != "Chart" {
		t.Fatalf("inline file = %+v", sink.files)
	}
	if got := sink.text.String(); !contains(got, "![Chart][") {
		t.Fatalf("want inline markdown ref, got %q", got)
	}
	r.clearActiveTurn("c")
	if _, ok := r.active["c"]; ok {
		t.Fatal("active not cleared")
	}
}

func TestAttachActive_SinkFileError(t *testing.T) {
	r := mkRouterWithUploader(t, true)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0o644)
	r.setActiveTurn("c", errFileSink2{&captureSink{}}, dir)
	if err := r.AttachActive("c", "f.txt", "n", false); err == nil {
		t.Fatal("want error when sink.File fails")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}())
}

type errFileSink2 struct{ *captureSink }

func (errFileSink2) File(string, string, string, string) error { return io.ErrClosedPipe }

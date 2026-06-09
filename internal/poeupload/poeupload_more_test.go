package poeupload

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom read") }

func TestUploadFile_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"attachment_url":"https://poe/f","mime_type":"text/markdown"}`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	fp := filepath.Join(dir, "doc.md")
	if err := os.WriteFile(fp, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := New("k", srv.URL, srv.Client()).UploadFile(context.Background(), fp)
	if err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if res.URL != "https://poe/f" || res.Name != "doc.md" {
		t.Fatalf("res=%+v", res)
	}
}

func TestUploadReader_CopyError(t *testing.T) {
	_, err := New("k", "http://x", nil).UploadReader(context.Background(), "f", errReader{})
	if err == nil || !strings.Contains(err.Error(), "copy") {
		t.Fatalf("want copy error, got %v", err)
	}
}

func TestUploadReader_BadEndpoint(t *testing.T) {
	_, err := New("k", "://bad-url", nil).UploadReader(context.Background(), "f", strings.NewReader("x"))
	if err == nil {
		t.Fatal("want request build error")
	}
}

func TestUploadReader_DoError(t *testing.T) {
	// Unreachable port → client.Do fails.
	_, err := New("k", "http://127.0.0.1:1/up", &http.Client{}).
		UploadReader(context.Background(), "f", strings.NewReader("x"))
	if err == nil || !strings.Contains(err.Error(), "do") {
		t.Fatalf("want do error, got %v", err)
	}
}

func TestUploadReader_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()
	_, err := New("k", srv.URL, srv.Client()).UploadReader(context.Background(), "f", strings.NewReader("x"))
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("want decode error, got %v", err)
	}
}

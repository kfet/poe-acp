package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// errFileSink errors on File to exercise the scanner's File-error branch.
type errFileSink struct{ scanSink }

func (s *errFileSink) File(string, string, string, string) error {
	return io.ErrClosedPipe
}

func TestScanner_FileEventErrorConsumesDirective(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "f.txt")
	os.WriteFile(fp, []byte("x"), 0o644)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"attachment_url":"u","mime_type":"text/plain"}`)
	}))
	defer srv.Close()
	sink := &errFileSink{}
	sc := newScannerFor(srv, sink, dir)
	sc.Feed(`<!--poe-attach path="` + fp + `"-->` + "\n")
	sc.Flush()
	// File errored but directive is still consumed (not leaked as text).
	if strings.Contains(sink.joined(), "poe-attach") {
		t.Fatalf("directive leaked: %q", sink.joined())
	}
}

func newScannerFor(srv *httptest.Server, sink ChunkSink, cwd string) *attachScanner {
	r := &Router{}
	r.uploader = newUploaderForURL(srv)
	return r.newAttachScanner(sink, cwd)
}

// TestRouter_AttachmentEndToEnd drives a full turn through Prompt with
// the uploader enabled, exercising the message-scanner path, the
// thought-interrupt flush, and the end-of-turn flush.
func TestRouter_AttachmentEndToEnd(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "deliver.md")
	os.WriteFile(fp, []byte("payload"), 0o644)

	var uploads int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uploads++
		_, _ = io.WriteString(w, `{"attachment_url":"https://poe/x","mime_type":"text/markdown"}`)
	}))
	defer srv.Close()

	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		// Thought first, then a message carrying a directive, then trailing text.
		a.emitUpdate(sid, acp.SessionUpdate{
			AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{Content: acp.TextBlock("pondering")},
		})
		a.emit(sid, "Here is your file.\n")
		a.emit(sid, `<!--poe-attach path="`+fp+`" name="Deliverable"-->`+"\n")
		a.emit(sid, "Done.")
		return acp.StopReasonEndTurn, nil
	})

	r, err := New(Config{
		Agent:                agent,
		StateDir:             dir,
		SessionTTL:           time.Hour,
		AccessKey:            "test-key",
		UploadEndpoint:       srv.URL,
		HTTPClient:           srv.Client(),
		SystemPromptProvider: func() string { return "SYS" },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sink := &captureSink{}
	if err := r.Prompt(context.Background(), "conv-x", "u1",
		[]Turn{{Role: "user", Content: "give me the file"}}, Options{}, sink); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if uploads != 1 {
		t.Fatalf("uploads=%d want 1", uploads)
	}
	if len(sink.files) != 1 || sink.files[0].name != "Deliverable" || sink.files[0].url != "https://poe/x" {
		t.Fatalf("files=%+v", sink.files)
	}
	got := sink.text.String()
	if strings.Contains(got, "poe-attach") {
		t.Fatalf("directive leaked into text: %q", got)
	}
	if !strings.Contains(got, "Here is your file.") || !strings.Contains(got, "Done.") {
		t.Fatalf("surrounding text missing: %q", got)
	}
}

func TestRouter_New_NoUploaderWhenNoKey(t *testing.T) {
	agent := newFakeAgent(func(_ context.Context, _ *fakeAgent, _ acp.SessionId, _ string) (acp.StopReason, error) {
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if r.uploader != nil {
		t.Fatal("uploader should be nil without AccessKey")
	}
}

func TestDiscardSink_File(t *testing.T) {
	if err := (discardSink{convID: "c"}).File("u", "ct", "n", ""); err != nil {
		t.Fatalf("discardSink.File: %v", err)
	}
}

func (s *errFileSink) SuggestedReply(string) error { return nil }

package router

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/poe-acp/internal/acpclient"
	"github.com/kfet/poe-acp/internal/debuglog"
)

func TestNew_ConfigErrors(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected nil-Agent error")
	}
	if _, err := New(Config{Agent: &fakeAgent{}}); err == nil {
		t.Fatal("expected empty-StateDir error")
	}
}

func TestNew_TTLDefaultsAndClamp(t *testing.T) {
	dir := t.TempDir()
	// SessionTTL=0 → default. AttachmentTTL=0 → default.
	r, err := New(Config{Agent: &fakeAgent{}, StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	_ = r
	// AttachmentTTL < SessionTTL → clamped (just exercises the branch).
	if _, err := New(Config{Agent: &fakeAgent{}, StateDir: dir,
		SessionTTL: time.Hour, AttachmentTTL: time.Minute}); err != nil {
		t.Fatal(err)
	}
}

func TestNew_MkdirError(t *testing.T) {
	// Use a path under a non-directory.
	f := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := New(Config{Agent: &fakeAgent{}, StateDir: filepath.Join(f, "child")})
	if err == nil {
		t.Fatal("expected mkdir error")
	}
}

func TestRouter_PromptEmptyUserMessage(t *testing.T) {
	r, _ := New(Config{Agent: &fakeAgent{}, StateDir: t.TempDir()})
	sink := &captureSink{}
	err := r.Prompt(context.Background(), "", "u", []Turn{{Role: "bot", Content: "x"}}, Options{}, sink)
	if err == nil {
		t.Fatal("expected error")
	}
	if sink.errText == "" || !sink.done {
		t.Fatalf("sink: %+v", sink)
	}
}

func TestRouter_PromptGetOrCreateError(t *testing.T) {
	a := newFakeAgent(func(context.Context, *fakeAgent, acp.SessionId, string) (acp.StopReason, error) {
		return acp.StopReasonEndTurn, nil
	})
	a.newSessErr = errors.New("boom")
	r, _ := New(Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour})
	sink := &captureSink{}
	err := r.Prompt(context.Background(), "c1", "u", []Turn{{Role: "user", Content: "hi"}}, Options{}, sink)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(sink.errText, "boom") {
		t.Fatalf("sink err=%q", sink.errText)
	}
}

func TestRouter_ApplyOptions_SetModelError(t *testing.T) {
	a := newFakeAgent(func(_ context.Context, fa *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		fa.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	a.setModelErr = errors.New("set-model fail")
	r, _ := New(Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour})
	sink := &captureSink{}
	err := r.Prompt(context.Background(), "c1", "u",
		[]Turn{{Role: "user", Content: "hi"}},
		Options{Model: "foo"}, sink)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sink.text.String(), "option not applied") {
		t.Fatalf("expected notice in text=%q", sink.text.String())
	}
}

func TestRouter_ApplyOptions_SetConfigSuppressed(t *testing.T) {
	a := newFakeAgent(func(_ context.Context, fa *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		fa.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	a.setConfigErr = errors.New("not supported")
	r, _ := New(Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour})
	sink := &captureSink{}
	err := r.Prompt(context.Background(), "c1", "u",
		[]Turn{{Role: "user", Content: "hi"}},
		Options{Thinking: "high"}, sink)
	if err != nil {
		t.Fatal(err)
	}
	// Suppressed: text should NOT contain "option not applied".
	if strings.Contains(sink.text.String(), "option not applied") {
		t.Fatalf("expected suppressed: %q", sink.text.String())
	}
}

func TestRouter_StopReasons_All(t *testing.T) {
	for _, sr := range []acp.StopReason{
		acp.StopReasonMaxTokens,
		acp.StopReasonMaxTurnRequests,
		acp.StopReasonRefusal,
		acp.StopReasonCancelled,
	} {
		sr := sr
		t.Run(string(sr), func(t *testing.T) {
			a := newFakeAgent(func(_ context.Context, fa *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
				fa.emit(sid, "x")
				return sr, nil
			})
			r, _ := New(Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour})
			sink := &captureSink{}
			if err := r.Prompt(context.Background(), "c-"+string(sr), "u",
				[]Turn{{Role: "user", Content: "hi"}}, Options{}, sink); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRouter_PromptError(t *testing.T) {
	a := newFakeAgent(func(context.Context, *fakeAgent, acp.SessionId, string) (acp.StopReason, error) {
		return "", errors.New("agent boom")
	})
	r, _ := New(Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour})
	sink := &captureSink{}
	err := r.Prompt(context.Background(), "c1", "u",
		[]Turn{{Role: "user", Content: "hi"}}, Options{}, sink)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(sink.errText, "agent boom") {
		t.Fatalf("sink: %q", sink.errText)
	}
}

func TestParseOptions_InvalidThinking(t *testing.T) {
	got := ParseOptions(map[string]any{"thinking": "bogus"}, Options{Thinking: "low"})
	if got.Thinking != "low" {
		t.Fatalf("expected default kept, got %q", got.Thinking)
	}
	got = ParseOptions(map[string]any{"model": 123, "hide_thinking": "yes"}, Options{Model: "x"})
	if got.Model != "x" {
		t.Fatalf("model overridden: %q", got.Model)
	}
}

func TestRouter_HTTPClientDefault(t *testing.T) {
	r, _ := New(Config{Agent: &fakeAgent{}, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if r.httpClient() != http.DefaultClient {
		t.Fatal("expected default")
	}
	hc := &http.Client{}
	r2, _ := New(Config{Agent: &fakeAgent{}, StateDir: t.TempDir(), SessionTTL: time.Hour, HTTPClient: hc})
	if r2.httpClient() != hc {
		t.Fatal("expected custom client")
	}
}

func TestNameHelpers(t *testing.T) {
	// preferredName fallbacks
	a := Attachment{URL: "https://x", ContentType: "image/png"}
	for _, n := range []string{"", ".", ".."} {
		a.Name = n
		if got := preferredName(a); !strings.HasPrefix(got, "attachment-") {
			t.Errorf("preferredName(%q)=%q", n, got)
		}
	}
	// fallbackName uses bin when no extension known.
	if got := fallbackName(Attachment{URL: "https://x", ContentType: "application/x-unknown-mime-foo"}); !strings.HasSuffix(got, ".bin") {
		t.Errorf("fallbackName: %q", got)
	}
	// capName preserves extension.
	long := strings.Repeat("a", 250) + ".png"
	got := capName(long, 50)
	if len(got) != 50 || !strings.HasSuffix(got, ".png") {
		t.Errorf("capName: %d %q", len(got), got)
	}
	// capName returns name as-is when within limit.
	if got := capName("short.png", 50); got != "short.png" {
		t.Fatalf("capName short: %q", got)
	}
	// capName when ext >= max.
	if got := capName("aa.toolong", 5); got != "aa.to" {
		t.Fatalf("capName ext>=max: %q", got)
	}
	// uniqueName.
	used := map[string]struct{}{"foo.png": {}}
	if got := uniqueName("foo.png", used); got != "foo-2.png" {
		t.Fatalf("uniqueName: %q", got)
	}
	used["foo-2.png"] = struct{}{}
	if got := uniqueName("foo.png", used); got != "foo-3.png" {
		t.Fatalf("uniqueName 3: %q", got)
	}
}

func TestResourceLinkHelpers(t *testing.T) {
	// resourceLinkBlockHTTPS with empty name uses URL.
	got := resourceLinkBlockHTTPS(Attachment{URL: "https://x/y.png", ContentType: "image/png"})
	if got.ResourceLink == nil || got.ResourceLink.Name != "https://x/y.png" {
		t.Fatalf("got %+v", got.ResourceLink)
	}
	// textResourceBlock with mime.
	tr := textResourceBlock("file:///a", "hello", "text/plain")
	if tr.Resource == nil || tr.Resource.Resource.TextResourceContents == nil ||
		tr.Resource.Resource.TextResourceContents.MimeType == nil ||
		*tr.Resource.Resource.TextResourceContents.MimeType != "text/plain" {
		t.Fatalf("textResourceBlock: %+v", tr)
	}
	// textResourceBlock without mime.
	tr2 := textResourceBlock("file:///a", "hello", "")
	if tr2.Resource.Resource.TextResourceContents.MimeType != nil {
		t.Fatalf("expected nil mime")
	}
	// fileResourceLinkBlock with empty contentType: no mime set.
	fb := fileResourceLinkBlock("a.txt", "/tmp/a.txt", "")
	if fb.ResourceLink.MimeType != nil {
		t.Fatalf("expected no mime")
	}
}

func TestLatestUserTurnRefAndFlatten(t *testing.T) {
	if _, ok := latestUserTurnRef(nil); ok {
		t.Fatal()
	}
	if _, ok := latestUserTurnRef([]Turn{{Role: "bot"}}); ok {
		t.Fatal()
	}
	got := flattenTranscript([]Turn{
		{Role: "user", Content: "u"},
		{Role: "bot", Content: "b"},
		{Role: "system", Content: "s"},
		{Role: "weird", Content: "w"},
	})
	for _, want := range []string{"User:", "Assistant:", "System:", "weird:"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
}

func TestRouter_Cancel(t *testing.T) {
	a := newFakeAgent(func(context.Context, *fakeAgent, acp.SessionId, string) (acp.StopReason, error) {
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour})
	// No session yet: returns nil.
	if err := r.Cancel(context.Background(), "c-x"); err != nil {
		t.Fatal(err)
	}
	// Create one then cancel.
	if err := r.Prompt(context.Background(), "c1", "u",
		[]Turn{{Role: "user", Content: "hi"}}, Options{}, &captureSink{}); err != nil {
		t.Fatal(err)
	}
	if err := r.Cancel(context.Background(), "c1"); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&a.cancelCalls) != 1 {
		t.Fatal("expected cancel call")
	}
}

func TestRouter_GetOrCreate_MkdirError(t *testing.T) {
	// Make StateDir/convs unreadable by replacing convs/ with a file.
	dir := t.TempDir()
	r, _ := New(Config{Agent: &fakeAgent{}, StateDir: dir, SessionTTL: time.Hour})
	// Replace convs/c1 with a file so MkdirAll(c1) fails (a file with that name exists).
	if err := os.RemoveAll(filepath.Join(dir, "convs")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "convs"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sink := &captureSink{}
	err := r.Prompt(context.Background(), "c1", "u",
		[]Turn{{Role: "user", Content: "hi"}}, Options{}, sink)
	if err == nil {
		t.Fatal("expected mkdir error")
	}
}

func TestRouter_RunGC(t *testing.T) {
	a := newFakeAgent(func(_ context.Context, fa *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		fa.emit(sid, "x")
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Nanosecond, AttachmentTTL: time.Nanosecond})
	_ = r.Prompt(context.Background(), "c1", "u",
		[]Turn{{Role: "user", Content: "hi"}}, Options{}, &captureSink{})
	if r.Len() != 1 {
		t.Fatalf("pre-gc len=%d", r.Len())
	}
	ticked := make(chan struct{}, 4)
	prev := runGCTickHook
	runGCTickHook = func() {
		select {
		case ticked <- struct{}{}:
		default:
		}
	}
	defer func() { runGCTickHook = prev }()
	stop := r.RunGC(context.Background(), time.Millisecond)
	select {
	case <-ticked:
	case <-time.After(3 * time.Second):
		stop()
		t.Fatal("RunGC tick never fired")
	}
	stop()
	if r.Len() != 0 {
		t.Fatalf("expected eviction, len=%d", r.Len())
	}
}

func TestRouter_Debug(t *testing.T) {
	a := newFakeAgent(func(_ context.Context, fa *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		fa.emit(sid, "x")
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour})
	for _, c := range []string{"b", "a"} {
		if err := r.Prompt(context.Background(), c, "u",
			[]Turn{{Role: "user", Content: "hi"}}, Options{}, &captureSink{}); err != nil {
			t.Fatal(err)
		}
	}
	d := r.Debug()
	if len(d) != 2 || d[0].ConvID != "a" || d[1].ConvID != "b" {
		t.Fatalf("Debug: %+v", d)
	}
}

func TestRouter_DefaultConvID(t *testing.T) {
	a := newFakeAgent(func(_ context.Context, fa *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		fa.emit(sid, "x")
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err := r.Prompt(context.Background(), "", "u",
		[]Turn{{Role: "user", Content: "hi"}}, Options{}, &captureSink{}); err != nil {
		t.Fatal(err)
	}
	if r.Len() != 1 {
		t.Fatalf("len=%d", r.Len())
	}
}

func TestDrainProcessChunk_ToolCallSuppressed(t *testing.T) {
	td := &turnDef{sink: &captureSink{}}
	first := false
	mode := chunkNone
	// Update with neither AgentMessageChunk nor AgentThoughtChunk → return.
	drainProcessChunk(acp.SessionNotification{Update: acp.SessionUpdate{}}, td, &first, &mode)
	if first {
		t.Fatal("expected first to remain false")
	}
}

func TestDrainProcessChunk_HiddenThought(t *testing.T) {
	td := &turnDef{sink: &captureSink{}, hideThinking: true}
	first := false
	mode := chunkNone
	drainProcessChunk(acp.SessionNotification{Update: acp.SessionUpdate{
		AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{Content: acp.TextBlock("hidden")},
	}}, td, &first, &mode)
	if first {
		t.Fatal("hidden thought should not start the turn")
	}
}

func TestDrainProcessChunk_ThoughtFormatting(t *testing.T) {
	cs := &captureSink{}
	td := &turnDef{sink: cs}
	first := false
	mode := chunkNone

	// First chunk thought.
	drainProcessChunk(acp.SessionNotification{Update: acp.SessionUpdate{
		AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{Content: acp.TextBlock("a\nb")},
	}}, td, &first, &mode)
	// Then a message chunk.
	drainProcessChunk(acp.SessionNotification{Update: acp.SessionUpdate{
		AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("M")},
	}}, td, &first, &mode)
	// Then thought again.
	drainProcessChunk(acp.SessionNotification{Update: acp.SessionUpdate{
		AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{Content: acp.TextBlock("T")},
	}}, td, &first, &mode)
	got := cs.text.String()
	if !strings.Contains(got, "Thinking…") || !strings.Contains(got, "M") {
		t.Fatalf("got %q", got)
	}
}

// --- Attachment download paths ---

func TestRouter_AttachmentDownloadFlow(t *testing.T) {
	// Real tiny image.
	pngBytes := []byte{
		0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A,
		0, 0, 0, 13, 'I', 'H', 'D', 'R',
		0, 0, 0, 1, 0, 0, 0, 1, 8, 2, 0, 0, 0, 0x90, 0x77, 0x53, 0xDE,
		0, 0, 0, 0, 'I', 'E', 'N', 'D', 0xAE, 0x42, 0x60, 0x82,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/big":
			// Force chunked encoding so Content-Length isn't set.
			w.Header().Set("Content-Type", "image/png")
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			w.Write(bytes.Repeat([]byte("a"), 1024))
		case "/declared-too-big":
			w.Header().Set("Content-Length", "1000000")
			w.WriteHeader(200)
			w.Write([]byte("x"))
		case "/404":
			w.WriteHeader(404)
		case "/img":
			w.Header().Set("Content-Type", "image/png")
			w.Write(pngBytes)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	a := newFakeAgent(func(_ context.Context, fa *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		fa.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{
		Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour,
		HTTPClient:          srv.Client(),
		MaxAttachmentBytes:  100,
		MaxInlineImageBytes: 1024,
	})

	cases := []struct {
		name string
		atts []Attachment
		want int // # of resource-link / image / resource blocks expected
	}{
		{"small image", []Attachment{{URL: srv.URL + "/img", ContentType: "image/png", Name: "a.png"}}, 2},
		{"oversize", []Attachment{{URL: srv.URL + "/big", ContentType: "image/png", Name: "big.png"}}, 1},
		{"declared too big", []Attachment{{URL: srv.URL + "/declared-too-big", ContentType: "image/png", Name: "d.png"}}, 1},
		{"http 404", []Attachment{{URL: srv.URL + "/404", ContentType: "image/png", Name: "x.png"}}, 1},
		{"hostile name", []Attachment{{URL: srv.URL + "/img", ContentType: "image/png", Name: "../hostile.png"}}, 2},
	}
	for i, tc := range cases {
		conv := fmt.Sprintf("c-%d", i)
		err := r.Prompt(context.Background(), conv, "u",
			[]Turn{{Role: "user", Content: "hi", MessageID: "m1", Attachments: tc.atts}},
			Options{}, &captureSink{})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		// Check non-text blocks were emitted.
		a.mu.Lock()
		var nonText int
		for _, b := range a.lastPromptBlocks {
			if b.Text == nil {
				nonText++
			}
		}
		a.mu.Unlock()
		if nonText < 1 {
			t.Errorf("%s: expected ≥1 non-text block", tc.name)
		}
	}
}

func TestRouter_DownloadAttachment_BadURL(t *testing.T) {
	a := newFakeAgent(func(_ context.Context, fa *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		fa.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour})
	// A URL that doesn't connect.
	err := r.Prompt(context.Background(), "c1", "u",
		[]Turn{{Role: "user", Content: "hi", MessageID: "m1", Attachments: []Attachment{
			{URL: "http://127.0.0.1:1/never", ContentType: "image/png", Name: "x.png"},
		}}}, Options{}, &captureSink{})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRouter_DownloadAttachment_BadRequest(t *testing.T) {
	a := newFakeAgent(func(_ context.Context, fa *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		fa.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour})
	// Invalid URL → http.NewRequestWithContext fails.
	err := r.Prompt(context.Background(), "c1", "u",
		[]Turn{{Role: "user", Content: "hi", MessageID: "m1", Attachments: []Attachment{
			{URL: "ht tp://bad", ContentType: "image/png", Name: "x.png"},
		}}}, Options{}, &captureSink{})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRouter_OpenMessageDir_FailsBase(t *testing.T) {
	// MkdirAll on attachment base fails when parent is a regular file.
	dir := t.TempDir()
	cwd := filepath.Join(dir, "cwd")
	if err := os.WriteFile(cwd, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openMessageDir(cwd, "m1"); err == nil {
		t.Fatal("expected error")
	}
}

func TestRouter_OpenMessageDir_HostileMsgID(t *testing.T) {
	cwd := t.TempDir()
	// MsgID with traversal: os.Root.Mkdir rejects.
	if _, err := openMessageDir(cwd, "../escape"); err == nil {
		t.Fatal("expected error")
	}
}

func TestRouter_OpenMessageDir_FailsOpenRoot(t *testing.T) {
	// Make attBase a file so OpenRoot fails.
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, attachmentDirName), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := openMessageDir(cwd, "m1"); err == nil {
		t.Fatal("expected OpenRoot error")
	}
}

func TestRouter_SweepAttachments(t *testing.T) {
	a := newFakeAgent(func(_ context.Context, fa *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		fa.emit(sid, "x")
		return acp.StopReasonEndTurn, nil
	})
	dir := t.TempDir()
	// Tweak Now to control time.
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	r, _ := New(Config{
		Agent: a, StateDir: dir, SessionTTL: time.Hour, AttachmentTTL: time.Hour,
		Now: func() time.Time { return time.Unix(0, clock.Load()) },
	})

	// Set up <stateDir>/convs/c1/.poe-attachments/m1/file.png with old mtime.
	convDir := filepath.Join(dir, "convs", "c1")
	attDir := filepath.Join(convDir, attachmentDirName, "m1")
	if err := os.MkdirAll(attDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(attDir, "old.png")
	if err := os.WriteFile(old, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	// Also make a stray file in convs/ (non-dir entry).
	if err := os.WriteFile(filepath.Join(dir, "convs", "stray"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Conv dir with no attachments.
	if err := os.MkdirAll(filepath.Join(dir, "convs", "c2"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Empty att dir for c1: stray non-dir entry inside .poe-attachments
	if err := os.WriteFile(filepath.Join(convDir, attachmentDirName, "stray"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Empty msg dir.
	if err := os.MkdirAll(filepath.Join(convDir, attachmentDirName, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	r.sweepAttachmentsOnce()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old file still exists: %v", err)
	}
}

func TestRouter_SweepAttachments_NoStateDir(t *testing.T) {
	a := newFakeAgent(func(context.Context, *fakeAgent, acp.SessionId, string) (acp.StopReason, error) {
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{Agent: a, StateDir: filepath.Join(t.TempDir(), "missing"), SessionTTL: time.Hour, AttachmentTTL: time.Hour})
	// convs root doesn't exist — silently skipped.
	r.sweepAttachmentsOnce()
}

func TestRouter_SweepAttachments_TTLZero(t *testing.T) {
	a := newFakeAgent(func(context.Context, *fakeAgent, acp.SessionId, string) (acp.StopReason, error) {
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour, AttachmentTTL: time.Hour})
	// Reach in: set TTL to 0 to take early-return path.
	r.cfg.AttachmentTTL = 0
	r.sweepAttachmentsOnce()
}

func TestRouter_SweepAttachments_ReadDirError(t *testing.T) {
	a := newFakeAgent(func(context.Context, *fakeAgent, acp.SessionId, string) (acp.StopReason, error) {
		return acp.StopReasonEndTurn, nil
	})
	dir := t.TempDir()
	// Make convs a file: ReadDir errors with non-NotExist.
	if err := os.WriteFile(filepath.Join(dir, "convs"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r := &Router{cfg: Config{
		Agent: a, StateDir: dir, SessionTTL: time.Hour, AttachmentTTL: time.Hour,
		Now: time.Now,
	}, sessions: map[string]*sessionState{}}
	// Enable debug to exercise the debuglog branch.
	prev := debuglog.Enabled()
	debuglog.SetEnabled(true)
	defer debuglog.SetEnabled(prev)
	r.sweepAttachmentsOnce()
}

// errReader returns an error after the headers but during body read,
// triggering io.Copy failure → cerr branch.
type errReadResp struct{ http.Handler }

func TestRouter_DownloadAttachment_BodyReadError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(200)
		// Hijack to close mid-stream.
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer srv.Close()
	a := newFakeAgent(func(_ context.Context, fa *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		fa.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour, HTTPClient: srv.Client()})
	_ = r.Prompt(context.Background(), "c1", "u",
		[]Turn{{Role: "user", Content: "hi", MessageID: "m1", Attachments: []Attachment{
			{URL: srv.URL, ContentType: "image/png", Name: "x.png"},
		}}}, Options{}, &captureSink{})
}

func TestRouter_ListSessionsErrorLogged(t *testing.T) {
	a := newFakeAgent(func(_ context.Context, fa *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		fa.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	a.caps = acpclient.Caps{ListSessions: true, ResumeSession: true}
	a.listErr = errors.New("list-fail")
	r, _ := New(Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err := r.Prompt(context.Background(), "c1", "u",
		[]Turn{{Role: "user", Content: "hi"}}, Options{}, &captureSink{}); err != nil {
		t.Fatal(err)
	}
}

func TestSweepAttachments_DebugLogPaths(t *testing.T) {
	a := newFakeAgent(func(context.Context, *fakeAgent, acp.SessionId, string) (acp.StopReason, error) {
		return acp.StopReasonEndTurn, nil
	})
	prev := debuglog.Enabled()
	debuglog.SetEnabled(true)
	defer debuglog.SetEnabled(prev)

	dir := t.TempDir()
	r, _ := New(Config{Agent: a, StateDir: dir, SessionTTL: time.Hour, AttachmentTTL: time.Hour})
	convDir := filepath.Join(dir, "convs", "c1", attachmentDirName, "m1")
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// File whose os.Remove will fail: make the parent dir read-only AFTER
	// writing the file, so chmod restoration in cleanup needs care.
	old := filepath.Join(convDir, "old.png")
	if err := os.WriteFile(old, []byte("d"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldT := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, oldT, oldT); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(convDir, 0o500); err != nil { // r-x: deny removal
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(convDir, 0o755) })

	r.sweepAttachmentsOnce()
}

// drainStop branch in OnUpdate: send to closed drainStop without consumer.
func TestSessionState_OnUpdateAfterStop(t *testing.T) {
	s := &sessionState{
		chunkCh:   make(chan chunkMsg), // unbuffered
		drainStop: make(chan struct{}),
	}
	close(s.drainStop)
	// Drain branch should be selected immediately.
	if err := s.OnUpdate(context.Background(), acp.SessionNotification{}); err != nil {
		t.Fatal(err)
	}
}

// Make sure unused imports are touched.
var _ = io.EOF

func swap[T any](dst *T, v T) func() {
	old := *dst
	*dst = v
	return func() { *dst = old }
}

func TestSweepAttachments_MsgPathReadDirError(t *testing.T) {
	a := newFakeAgent(func(context.Context, *fakeAgent, acp.SessionId, string) (acp.StopReason, error) {
		return acp.StopReasonEndTurn, nil
	})
	dir := t.TempDir()
	convDir := filepath.Join(dir, "convs", "c1", attachmentDirName, "m1")
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatal(err)
	}
	defer swap(&osReadDirRouter, func(p string) ([]os.DirEntry, error) {
		if filepath.Base(p) == "m1" {
			return nil, errors.New("readdir-fail")
		}
		return os.ReadDir(p)
	})()
	r, _ := New(Config{Agent: a, StateDir: dir, SessionTTL: time.Hour, AttachmentTTL: time.Hour})
	r.sweepAttachmentsOnce()
}

func TestRouter_AttachmentBlocks_InlineReadFail(t *testing.T) {
	defer swap(&osReadFile, func(string) ([]byte, error) { return nil, errors.New("read-fail") })()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("img"))
	}))
	defer srv.Close()
	a := newFakeAgent(func(_ context.Context, fa *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		fa.emit(sid, "x")
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour, HTTPClient: srv.Client()})
	prev := debuglog.Enabled()
	debuglog.SetEnabled(true)
	defer debuglog.SetEnabled(prev)
	if err := r.Prompt(context.Background(), "c1", "u",
		[]Turn{{Role: "user", Content: "hi", MessageID: "m1", Attachments: []Attachment{
			{URL: srv.URL, ContentType: "image/png", Name: "x.png"},
		}}}, Options{}, &captureSink{}); err != nil {
		t.Fatal(err)
	}
}

func TestRouter_DownloadAttachment_OpenMessageDirError(t *testing.T) {
	defer swap(&openMessageDirFn, func(cwd, msgID string) (*os.Root, error) {
		return nil, errors.New("openmsgdir-fail")
	})()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("x"))
	}))
	defer srv.Close()
	a := newFakeAgent(func(_ context.Context, fa *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		fa.emit(sid, "x")
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour, HTTPClient: srv.Client()})
	if err := r.Prompt(context.Background(), "c1", "u",
		[]Turn{{Role: "user", Content: "hi", MessageID: "m1", Attachments: []Attachment{
			{URL: srv.URL, ContentType: "image/png", Name: "x.png"},
		}}}, Options{}, &captureSink{}); err != nil {
		t.Fatal(err)
	}
}

func TestRouter_DownloadAttachment_CopyError(t *testing.T) {
	defer swap(&ioCopy, func(io.Writer, io.Reader) (int64, error) {
		return 0, errors.New("copy-fail")
	})()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("x"))
	}))
	defer srv.Close()
	a := newFakeAgent(func(_ context.Context, fa *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		fa.emit(sid, "x")
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour, HTTPClient: srv.Client()})
	if err := r.Prompt(context.Background(), "c1", "u",
		[]Turn{{Role: "user", Content: "hi", MessageID: "m1", Attachments: []Attachment{
			{URL: srv.URL, ContentType: "image/png", Name: "x.png"},
		}}}, Options{}, &captureSink{}); err != nil {
		t.Fatal(err)
	}
}

func TestRouter_OpenMessageDir_OpenRootError(t *testing.T) {
	defer swap(&osOpenRoot, func(string) (*os.Root, error) { return nil, errors.New("openroot-fail") })()
	if _, err := openMessageDir(t.TempDir(), "m1"); err == nil {
		t.Fatal("expected error")
	}
}

func TestRouter_OpenMessageDir_SubOpenRootError(t *testing.T) {
	// First call (parent) succeeds; second call (child) errors.
	calls := 0
	defer swap(&osOpenRoot, func(p string) (*os.Root, error) {
		calls++
		if calls == 1 {
			return os.OpenRoot(p) // real
		}
		return nil, errors.New("subroot-fail")
	})()
	// We need to also use a fake parent.OpenRoot — but parent is *os.Root from real call.
	// Skip: this branch is virtually identical to the parent branch. Use a different
	// approach: replace osOpenRoot to return a *os.Root whose internal Mkdir is fine
	// but OpenRoot fails. Since *os.Root has unexported state we can't fake it.
	// Instead, hit the path by making msgID name fail OpenRoot — which requires the
	// kernel to refuse opening a directory we just created. Hard. Accept dropping
	// this assertion via ENOTDIR instead: pre-create msgID as a file in attBase so
	// Mkdir errors with ErrExist (handled), then OpenRoot of a file errors.
	cwd := t.TempDir()
	attBase := filepath.Join(cwd, attachmentDirName)
	if err := os.MkdirAll(attBase, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(attBase, "m1"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Reset osOpenRoot for real use in this test.
	osOpenRoot = os.OpenRoot
	if _, err := openMessageDir(cwd, "m1"); err == nil {
		t.Fatal("expected sub OpenRoot error")
	}
}

func TestRouter_OpenMessageDir_MkdirAllFailsViaSwap(t *testing.T) {
	defer swap(&osMkdirAllAtt, func(string, os.FileMode) error { return errors.New("mkdir-fail") })()
	if _, err := openMessageDir(t.TempDir(), "m1"); err == nil {
		t.Fatal("expected mkdir error")
	}
}

func TestSweepAttachments_FileInfoError(t *testing.T) {
	a := newFakeAgent(func(context.Context, *fakeAgent, acp.SessionId, string) (acp.StopReason, error) {
		return acp.StopReasonEndTurn, nil
	})
	dir := t.TempDir()
	convDir := filepath.Join(dir, "convs", "c1", attachmentDirName, "m1")
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(convDir, "old.png")
	if err := os.WriteFile(old, []byte("d"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldT := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, oldT, oldT); err != nil {
		t.Fatal(err)
	}

	// Inject readDir that returns a fake DirEntry whose Info() errors.
	defer swap(&osReadDirRouter, func(p string) ([]os.DirEntry, error) {
		es, err := os.ReadDir(p)
		if err != nil {
			return nil, err
		}
		// Wrap entries inside the message dir to make Info() fail.
		if filepath.Base(p) == "m1" {
			out := make([]os.DirEntry, len(es))
			for i, e := range es {
				out[i] = errInfoEntry{DirEntry: e}
			}
			return out, nil
		}
		return es, nil
	})()

	r, _ := New(Config{Agent: a, StateDir: dir, SessionTTL: time.Hour, AttachmentTTL: time.Hour})
	r.sweepAttachmentsOnce()
	// Old file should remain (Info errored, marked liveCount).
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("file should remain: %v", err)
	}
}

type errInfoEntry struct{ os.DirEntry }

func (errInfoEntry) Info() (os.FileInfo, error) { return nil, errors.New("info-fail") }

func TestRouter_Install_LostRace(t *testing.T) {
	// Deterministic coverage for the lost-race branch in
	// (*Router).install: pre-populate r.sessions with an entry under
	// some convID, then call install with a freshly-built loser st;
	// install must return the existing entry + false and close the
	// loser's drainStop channel.
	a := newFakeAgent(func(context.Context, *fakeAgent, acp.SessionId, string) (acp.StopReason, error) {
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour})

	winner := &sessionState{drainStop: make(chan struct{})}
	r.sessions["c1"] = winner

	loser := &sessionState{drainStop: make(chan struct{})}
	got, won := r.install("c1", loser)
	if won {
		t.Fatalf("install: want won=false on collision")
	}
	if got != winner {
		t.Fatalf("install: want existing winner returned")
	}
	select {
	case <-loser.drainStop:
	default:
		t.Fatalf("install: loser.drainStop not closed")
	}
}

// raceInjectingAgent wraps a fakeAgent and, on NewSession, writes a
// pre-built winner sessionState into the router's session map before
// returning. This forces the subsequent install() call inside
// getOrCreate to lose deterministically — exercising the !won branch
// (freshSeed=false) without relying on goroutine scheduling.
type raceInjectingAgent struct {
	*fakeAgent
	r      *Router
	convID string
}

func (a *raceInjectingAgent) NewSession(ctx context.Context, cwd string, sink acpclient.SessionUpdateSink, sysBlocks []acp.ContentBlock) (acp.SessionId, error) {
	sid, err := a.fakeAgent.NewSession(ctx, cwd, sink, sysBlocks)
	if err != nil {
		return sid, err
	}
	a.r.mu.Lock()
	a.r.sessions[a.convID] = &sessionState{
		sessionID: "winner",
		drainStop: make(chan struct{}),
	}
	a.r.mu.Unlock()
	return sid, nil
}

func TestRouter_GetOrCreate_LostRaceFreshSeedFalse(t *testing.T) {
	// Drives the !won branch in getOrCreate: install loses, so
	// freshSeed must be reset to false even though the inbound query
	// has prior turns.
	fa := newFakeAgent(func(context.Context, *fakeAgent, acp.SessionId, string) (acp.StopReason, error) {
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{Agent: fa, StateDir: t.TempDir(), SessionTTL: time.Hour})
	r.cfg.Agent = &raceInjectingAgent{fakeAgent: fa, r: r, convID: "c1"}

	query := []Turn{
		{Role: "user", Content: "first"},
		{Role: "bot", Content: "reply"},
		{Role: "user", Content: "second"},
	}
	st, freshSeed, err := r.getOrCreate(context.Background(), "c1", "u", query)
	if err != nil {
		t.Fatal(err)
	}
	if freshSeed {
		t.Fatalf("freshSeed must be false after losing install race")
	}
	if string(st.sessionID) != "winner" {
		t.Fatalf("want winner returned, got sid=%q", st.sessionID)
	}
}

func TestSweepAttachments_RemoveError(t *testing.T) {
	defer swap(&osRemove, func(string) error { return errors.New("remove-fail") })()
	prev := debuglog.Enabled()
	debuglog.SetEnabled(true)
	defer debuglog.SetEnabled(prev)

	a := newFakeAgent(func(context.Context, *fakeAgent, acp.SessionId, string) (acp.StopReason, error) {
		return acp.StopReasonEndTurn, nil
	})
	dir := t.TempDir()
	convDir := filepath.Join(dir, "convs", "c1", attachmentDirName, "m1")
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(convDir, "old.png")
	if err := os.WriteFile(old, []byte("d"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldT := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, oldT, oldT); err != nil {
		t.Fatal(err)
	}
	r, _ := New(Config{Agent: a, StateDir: dir, SessionTTL: time.Hour, AttachmentTTL: time.Hour})
	r.sweepAttachmentsOnce()
}

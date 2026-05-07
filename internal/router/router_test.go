package router

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/poe-acp/internal/acpclient"
)

// fakeAgent implements Agent for tests.
type fakeAgent struct {
	mu      sync.Mutex
	sinks   map[acp.SessionId]acpclient.SessionUpdateSink
	nextID  int
	prompts int32

	// Hook: called with (sid, text) when Prompt is invoked. The hook
	// should use the registered sink to emit updates before returning.
	onPrompt func(ctx context.Context, a *fakeAgent, sid acp.SessionId, text string) (acp.StopReason, error)

	// Optional overrides for the resume tier.
	caps             acpclient.Caps
	listResult       []acpclient.SessionInfo
	listErr          error
	resumeErr        error
	newSessErr       error
	cancelErr        error
	setModelErr      error
	setConfigErr     error
	listCalls        int32
	resumeCalls      int32
	newSessCalls     int32
	cancelCalls      int32
	setModelCalls    int32
	setConfigCalls   int32
	lastPromptTxt    string
	lastPromptBlocks []acp.ContentBlock
	lastSysBlocks    []acp.ContentBlock
}

func newFakeAgent(onPrompt func(ctx context.Context, a *fakeAgent, sid acp.SessionId, text string) (acp.StopReason, error)) *fakeAgent {
	return &fakeAgent{
		sinks:    make(map[acp.SessionId]acpclient.SessionUpdateSink),
		onPrompt: onPrompt,
	}
}

func (f *fakeAgent) Caps() acpclient.Caps { return f.caps }
func (f *fakeAgent) ListSessions(_ context.Context, _ string) ([]acpclient.SessionInfo, error) {
	atomic.AddInt32(&f.listCalls, 1)
	return f.listResult, f.listErr
}
func (f *fakeAgent) ResumeSession(_ context.Context, _ string, sid acp.SessionId, sink acpclient.SessionUpdateSink) error {
	atomic.AddInt32(&f.resumeCalls, 1)
	if f.resumeErr != nil {
		return f.resumeErr
	}
	f.mu.Lock()
	f.sinks[sid] = sink
	f.mu.Unlock()
	return nil
}
func (f *fakeAgent) NewSession(_ context.Context, _ string, sink acpclient.SessionUpdateSink, sysBlocks []acp.ContentBlock) (acp.SessionId, error) {
	atomic.AddInt32(&f.newSessCalls, 1)
	if f.newSessErr != nil {
		return "", f.newSessErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := acp.SessionId("sess-" + time.Now().Format("150405") + "-" + itoa(f.nextID))
	f.sinks[id] = sink
	f.lastSysBlocks = sysBlocks
	return id, nil
}

func (f *fakeAgent) Prompt(ctx context.Context, sid acp.SessionId, prompt []acp.ContentBlock) (acp.StopReason, error) {
	atomic.AddInt32(&f.prompts, 1)
	var text string
	if len(prompt) > 0 && prompt[0].Text != nil {
		text = prompt[0].Text.Text
	}
	f.mu.Lock()
	f.lastPromptTxt = text
	f.lastPromptBlocks = prompt
	f.mu.Unlock()
	return f.onPrompt(ctx, f, sid, text)
}

func (f *fakeAgent) Cancel(_ context.Context, _ acp.SessionId) error {
	atomic.AddInt32(&f.cancelCalls, 1)
	return f.cancelErr
}
func (f *fakeAgent) SetModel(_ context.Context, _ acp.SessionId, _ string) error {
	atomic.AddInt32(&f.setModelCalls, 1)
	return f.setModelErr
}
func (f *fakeAgent) SetConfigOption(_ context.Context, _ acp.SessionId, _, _ string) error {
	atomic.AddInt32(&f.setConfigCalls, 1)
	return f.setConfigErr
}

func (f *fakeAgent) emit(sid acp.SessionId, chunk string) {
	f.mu.Lock()
	sink := f.sinks[sid]
	f.mu.Unlock()
	if sink == nil {
		return
	}
	_ = sink.OnUpdate(context.Background(), acp.SessionNotification{
		SessionId: sid,
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
				Content: acp.TextBlock(chunk),
			},
		},
	})
}

// emitUpdate is like emit but sends an arbitrary SessionUpdate.
func (f *fakeAgent) emitUpdate(sid acp.SessionId, u acp.SessionUpdate) {
	f.mu.Lock()
	sink := f.sinks[sid]
	f.mu.Unlock()
	if sink == nil {
		return
	}
	_ = sink.OnUpdate(context.Background(), acp.SessionNotification{SessionId: sid, Update: u})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// captureSink captures router output for assertions.
type captureSink struct {
	mu          sync.Mutex
	text        strings.Builder
	errText     string
	errType     string
	replaceText string
	done        bool
	firstCalled bool
}

func (s *captureSink) FirstChunk() {
	s.mu.Lock()
	s.firstCalled = true
	s.mu.Unlock()
}
func (s *captureSink) Text(t string) error {
	s.mu.Lock()
	s.text.WriteString(t)
	s.mu.Unlock()
	return nil
}
func (s *captureSink) Replace(t string) error {
	s.mu.Lock()
	s.replaceText = t
	s.mu.Unlock()
	return nil
}
func (s *captureSink) Error(t, et string) error {
	s.mu.Lock()
	s.errText, s.errType = t, et
	s.mu.Unlock()
	return nil
}
func (s *captureSink) Done() error { s.mu.Lock(); s.done = true; s.mu.Unlock(); return nil }

func TestRouter_PromptStreamsText(t *testing.T) {
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, text string) (acp.StopReason, error) {
		a.emit(sid, "hello ")
		a.emit(sid, "world")
		return acp.StopReasonEndTurn, nil
	})

	dir := t.TempDir()
	r, err := New(Config{Agent: agent, StateDir: dir, SessionTTL: time.Hour})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	sink := &captureSink{}
	if err := r.Prompt(context.Background(), "conv-a", "user-1", []Turn{{Role: "user", Content: "hi"}}, Options{}, sink); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if got := sink.text.String(); got != "hello world" {
		t.Fatalf("text=%q want %q", got, "hello world")
	}
	if !sink.done {
		t.Fatal("done not called")
	}
	if !sink.firstCalled {
		t.Fatal("FirstChunk not called")
	}
	if r.Len() != 1 {
		t.Fatalf("sessions=%d want 1", r.Len())
	}
}

func TestRouter_ReusesSession(t *testing.T) {
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	r, err := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		if err := r.Prompt(context.Background(), "conv-x", "u", []Turn{{Role: "user", Content: "ping"}}, Options{}, &captureSink{}); err != nil {
			t.Fatal(err)
		}
	}
	if r.Len() != 1 {
		t.Fatalf("want 1 session (reused), got %d", r.Len())
	}
	if agent.nextID != 1 {
		t.Fatalf("want NewSession called once, got %d", agent.nextID)
	}
}

func TestRouter_StopReasons(t *testing.T) {
	cases := map[string]struct {
		stop     acp.StopReason
		wantText string
		wantErr  bool
		wantRepl bool
	}{
		"end_turn":   {acp.StopReasonEndTurn, "", false, false},
		"max_tokens": {acp.StopReasonMaxTokens, "_(response truncated: max tokens)_", false, false},
		"refusal":    {acp.StopReasonRefusal, "", true, false},
		"cancelled":  {acp.StopReasonCancelled, "", false, true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			agent := newFakeAgent(func(_ context.Context, _ *fakeAgent, _ acp.SessionId, _ string) (acp.StopReason, error) {
				return c.stop, nil
			})
			r, _ := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})
			sink := &captureSink{}
			_ = r.Prompt(context.Background(), "c", "u", []Turn{{Role: "user", Content: "x"}}, Options{}, sink)
			if !sink.done {
				t.Fatal("done not called")
			}
			if c.wantErr && sink.errText == "" {
				t.Fatal("want error")
			}
			if c.wantRepl && sink.replaceText == "" {
				t.Fatal("want replace")
			}
			if c.wantText != "" && !strings.Contains(sink.text.String(), c.wantText) {
				t.Fatalf("text=%q missing %q", sink.text.String(), c.wantText)
			}
		})
	}
}

func TestRouter_IdleGC(t *testing.T) {
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})

	// Use a virtual clock so we can jump forward.
	var now int64 = 1_000_000_000_000
	nowFn := func() time.Time { return time.Unix(0, atomic.LoadInt64(&now)) }

	r, err := New(Config{
		Agent:      agent,
		StateDir:   t.TempDir(),
		SessionTTL: time.Minute,
		Now:        nowFn,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Prompt(context.Background(), "c1", "u", []Turn{{Role: "user", Content: "hi"}}, Options{}, &captureSink{}); err != nil {
		t.Fatal(err)
	}
	if r.Len() != 1 {
		t.Fatal("want 1 session")
	}

	// Jump clock past TTL, run gc.
	atomic.StoreInt64(&now, now+int64(2*time.Minute))
	r.gcOnce()
	if r.Len() != 0 {
		t.Fatalf("want 0 sessions after GC, got %d", r.Len())
	}
}

// --- Resume / cold-start tiering tests ---

func TestRouter_ResumesWhenAgentSupportsListResume(t *testing.T) {
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	agent.caps = acpclient.Caps{ListSessions: true, ResumeSession: true}
	agent.listResult = []acpclient.SessionInfo{{SessionId: "prior-sid"}}

	r, err := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	query := []Turn{
		{Role: "user", Content: "hi"},
		{Role: "bot", Content: "hello"},
		{Role: "user", Content: "hi again"},
	}
	if err := r.Prompt(context.Background(), "c1", "u", query, Options{}, &captureSink{}); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&agent.listCalls) != 1 {
		t.Fatalf("want 1 list call, got %d", agent.listCalls)
	}
	if atomic.LoadInt32(&agent.resumeCalls) != 1 {
		t.Fatalf("want 1 resume call, got %d", agent.resumeCalls)
	}
	if atomic.LoadInt32(&agent.newSessCalls) != 0 {
		t.Fatalf("want 0 new-session calls (should resume), got %d", agent.newSessCalls)
	}
	// Resumed: agent already has history; only the latest user turn is sent.
	if agent.lastPromptTxt != "hi again" {
		t.Fatalf("prompt text=%q want %q", agent.lastPromptTxt, "hi again")
	}
}

func TestRouter_NewSessionWhenListEmpty(t *testing.T) {
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	agent.caps = acpclient.Caps{ListSessions: true, ResumeSession: true}
	// listResult nil → empty, no prior session.

	r, _ := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})
	query := []Turn{
		{Role: "user", Content: "first"},
		{Role: "bot", Content: "reply"},
		{Role: "user", Content: "second"},
	}
	if err := r.Prompt(context.Background(), "c1", "u", query, Options{}, &captureSink{}); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&agent.listCalls) != 1 {
		t.Fatalf("want 1 list call, got %d", agent.listCalls)
	}
	if atomic.LoadInt32(&agent.resumeCalls) != 0 {
		t.Fatalf("want 0 resume calls, got %d", agent.resumeCalls)
	}
	if atomic.LoadInt32(&agent.newSessCalls) != 1 {
		t.Fatalf("want 1 new-session call, got %d", agent.newSessCalls)
	}
	// No resume: seed with full transcript.
	if !strings.Contains(agent.lastPromptTxt, "User: first") || !strings.Contains(agent.lastPromptTxt, "Assistant: reply") || !strings.Contains(agent.lastPromptTxt, "User: second") {
		t.Fatalf("seed prompt missing transcript pieces: %q", agent.lastPromptTxt)
	}
}

func TestRouter_NewSessionWhenCapsAbsent(t *testing.T) {
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	// No caps at all.

	r, _ := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})
	query := []Turn{
		{Role: "user", Content: "first"},
		{Role: "bot", Content: "reply"},
		{Role: "user", Content: "second"},
	}
	if err := r.Prompt(context.Background(), "c1", "u", query, Options{}, &captureSink{}); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&agent.listCalls) != 0 {
		t.Fatalf("want 0 list calls (caps absent), got %d", agent.listCalls)
	}
	if atomic.LoadInt32(&agent.newSessCalls) != 1 {
		t.Fatalf("want 1 new-session call, got %d", agent.newSessCalls)
	}
	if !strings.Contains(agent.lastPromptTxt, "User: second") {
		t.Fatalf("expected seeded transcript: %q", agent.lastPromptTxt)
	}
}

func TestRouter_HotPathSendsLatestOnly(t *testing.T) {
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	r, _ := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})

	// First call: cold + single user turn → no seed.
	if err := r.Prompt(context.Background(), "c1", "u", []Turn{{Role: "user", Content: "one"}}, Options{}, &captureSink{}); err != nil {
		t.Fatal(err)
	}
	// Second call: hot, even with prior turns the router sends only latest.
	q := []Turn{
		{Role: "user", Content: "one"},
		{Role: "bot", Content: "ok"},
		{Role: "user", Content: "two"},
	}
	if err := r.Prompt(context.Background(), "c1", "u", q, Options{}, &captureSink{}); err != nil {
		t.Fatal(err)
	}
	if agent.lastPromptTxt != "two" {
		t.Fatalf("hot-path prompt=%q want %q", agent.lastPromptTxt, "two")
	}
	if atomic.LoadInt32(&agent.newSessCalls) != 1 {
		t.Fatalf("want 1 new-session call (reused), got %d", agent.newSessCalls)
	}
}

func TestRouter_FallsBackWhenResumeErrors(t *testing.T) {
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	agent.caps = acpclient.Caps{ListSessions: true, ResumeSession: true}
	agent.listResult = []acpclient.SessionInfo{{SessionId: "stale"}}
	agent.resumeErr = fmt.Errorf("session not found")

	r, _ := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})
	query := []Turn{
		{Role: "user", Content: "hi"},
		{Role: "bot", Content: "hey"},
		{Role: "user", Content: "again"},
	}
	if err := r.Prompt(context.Background(), "c1", "u", query, Options{}, &captureSink{}); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&agent.resumeCalls) != 1 {
		t.Fatalf("want 1 resume call, got %d", agent.resumeCalls)
	}
	if atomic.LoadInt32(&agent.newSessCalls) != 1 {
		t.Fatalf("want fallback NewSession after resume error, got %d", agent.newSessCalls)
	}
	if !strings.Contains(agent.lastPromptTxt, "User: again") {
		t.Fatalf("expected seed prompt after fallback: %q", agent.lastPromptTxt)
	}
}

func TestRouter_RaceLoserDoesNotSeed(t *testing.T) {
	// Two concurrent cold-path requests for the same conv with prior
	// turns must not BOTH issue a seeded transcript prompt. Whoever
	// loses the install race must take the hot path (latest user turn
	// only) on the winner's session.
	var (
		seenMu sync.Mutex
		seen   []string
	)
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, text string) (acp.StopReason, error) {
		seenMu.Lock()
		seen = append(seen, text)
		seenMu.Unlock()
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})

	r, _ := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour})

	query := []Turn{
		{Role: "user", Content: "first"},
		{Role: "bot", Content: "reply"},
		{Role: "user", Content: "second"},
	}

	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			_ = r.Prompt(context.Background(), "race-conv", "u", query, Options{}, &captureSink{})
		}()
	}
	wg.Wait()

	seenMu.Lock()
	defer seenMu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("want 2 prompts, got %d: %v", len(seen), seen)
	}
	seeded := 0
	for _, p := range seen {
		if strings.Contains(p, "[Resuming a prior conversation") {
			seeded++
		}
	}
	if seeded > 1 {
		t.Fatalf("more than one prompt was seeded; double-seeding hot session: %v", seen)
	}
	if r.Len() != 1 {
		t.Fatalf("want 1 session after race, got %d", r.Len())
	}
}

func TestRouter_GCDoesNotEvictMidPrompt(t *testing.T) {
	// Long-running Prompt: GC fires after the TTL would have lapsed
	// since session creation, but the session is currently in use.
	// touch() at the start of Prompt must keep it alive.
	var now int64 = 1_000_000_000_000
	nowFn := func() time.Time { return time.Unix(0, atomic.LoadInt64(&now)) }

	gcDuring := make(chan struct{})
	releasePrompt := make(chan struct{})

	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		close(gcDuring)
		<-releasePrompt
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})

	r, _ := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Minute, Now: nowFn})

	done := make(chan error, 1)
	go func() {
		done <- r.Prompt(context.Background(), "c1", "u", []Turn{{Role: "user", Content: "hi"}}, Options{}, &captureSink{})
	}()

	<-gcDuring
	// Jump clock past TTL relative to creation time, then run GC.
	atomic.StoreInt64(&now, now+int64(2*time.Minute))
	r.gcOnce()
	if r.Len() != 1 {
		close(releasePrompt)
		t.Fatalf("session evicted mid-prompt; want 1 session, got %d", r.Len())
	}
	close(releasePrompt)
	if err := <-done; err != nil {
		t.Fatalf("prompt: %v", err)
	}
}

func TestRouter_OnlyLatestUserAttachmentsAreForwarded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("png-bytes"))
	}))
	defer srv.Close()

	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})

	dir := t.TempDir()
	r, err := New(Config{Agent: agent, StateDir: dir, SessionTTL: time.Hour, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	turns := []Turn{
		{Role: "user", MessageID: "m1", Content: "first", Attachments: []Attachment{{URL: srv.URL + "/old.png", ContentType: "image/png", Name: "old.png"}}},
		{Role: "bot", Content: "ack"},
		{Role: "user", MessageID: "m2", Content: "second", Attachments: []Attachment{{URL: srv.URL + "/new.png", ContentType: "image/png", Name: "new.png"}}},
	}
	if err := r.Prompt(context.Background(), "conv-latest", "u1", turns, Options{}, &captureSink{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	agent.mu.Lock()
	blocks := agent.lastPromptBlocks
	agent.mu.Unlock()

	// Only the latest user turn's attachment should appear, as a
	// file:// ResourceLink whose path lives under the conv cwd.
	var seenLinks []string
	for _, b := range blocks {
		if b.ResourceLink != nil {
			seenLinks = append(seenLinks, b.ResourceLink.Uri)
		}
	}
	if len(seenLinks) != 1 {
		t.Fatalf("links=%v want exactly one", seenLinks)
	}
	if !strings.HasPrefix(seenLinks[0], "file://") || !strings.Contains(seenLinks[0], "new.png") {
		t.Fatalf("link=%q want file:// path containing new.png", seenLinks[0])
	}
	if strings.Contains(seenLinks[0], "old.png") {
		t.Fatalf("old.png leaked: %q", seenLinks[0])
	}
}

func TestRouter_EmptyAttachmentsSliceProducesSingleTextBlock(t *testing.T) {
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	dir := t.TempDir()
	r, err := New(Config{Agent: agent, StateDir: dir, SessionTTL: time.Hour})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for name, atts := range map[string][]Attachment{
		"nil":   nil,
		"empty": {},
	} {
		t.Run(name, func(t *testing.T) {
			turns := []Turn{{Role: "user", Content: "x", Attachments: atts}}
			if err := r.Prompt(context.Background(), "conv-"+name, "u", turns, Options{}, &captureSink{}); err != nil {
				t.Fatalf("Prompt: %v", err)
			}
			agent.mu.Lock()
			n := len(agent.lastPromptBlocks)
			agent.mu.Unlock()
			if n != 1 {
				t.Fatalf("blocks=%d want 1", n)
			}
		})
	}
}

func TestRouter_DropsAttachmentsWithEmptyURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("blob"))
	}))
	defer srv.Close()
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	dir := t.TempDir()
	r, err := New(Config{Agent: agent, StateDir: dir, SessionTTL: time.Hour, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	turns := []Turn{{
		Role:      "user",
		MessageID: "m1",
		Content:   "x",
		Attachments: []Attachment{
			{URL: "", Name: "ghost.txt"},
			{URL: srv.URL + "/real.bin", Name: "real.bin", ContentType: "application/octet-stream"},
		},
	}}
	if err := r.Prompt(context.Background(), "conv-empty-url", "u", turns, Options{}, &captureSink{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	// Real attachment downloads + emits one file:// ResourceLink; ghost
	// is silently dropped (empty URL).
	if len(agent.lastPromptBlocks) != 2 {
		t.Fatalf("blocks=%d want 2 (text + link)", len(agent.lastPromptBlocks))
	}
	rl := agent.lastPromptBlocks[1].ResourceLink
	if rl == nil {
		t.Fatalf("block[1] not ResourceLink: %+v", agent.lastPromptBlocks[1])
	}
	if !strings.HasPrefix(rl.Uri, "file://") || !strings.Contains(rl.Uri, "real.bin") {
		t.Fatalf("uri=%q want file:// path containing real.bin", rl.Uri)
	}
}

func TestRouter_ParsedContentEmittedAsResourceWhenCapable(t *testing.T) {
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	agent.caps = acpclient.Caps{EmbeddedContext: true}
	dir := t.TempDir()
	r, err := New(Config{Agent: agent, StateDir: dir, SessionTTL: time.Hour})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	turns := []Turn{{
		Role:    "user",
		Content: "summarise",
		Attachments: []Attachment{{
			URL:           "https://poe.example/doc.txt",
			ContentType:   "text/plain",
			Name:          "doc.txt",
			ParsedContent: "hello world",
		}},
	}}
	if err := r.Prompt(context.Background(), "conv-emb", "u", turns, Options{}, &captureSink{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.lastPromptBlocks) != 2 {
		t.Fatalf("blocks=%d want 2", len(agent.lastPromptBlocks))
	}
	res := agent.lastPromptBlocks[1].Resource
	if res == nil {
		t.Fatalf("block[1] not Resource: %+v", agent.lastPromptBlocks[1])
	}
	trc := res.Resource.TextResourceContents
	if trc == nil {
		t.Fatalf("Resource not TextResourceContents: %+v", res.Resource)
	}
	if trc.Uri != "https://poe.example/doc.txt" || trc.Text != "hello world" {
		t.Fatalf("trc=%+v", trc)
	}
	if trc.MimeType == nil || *trc.MimeType != "text/plain" {
		t.Fatalf("mime=%v", trc.MimeType)
	}
}

func TestRouter_ParsedContentIgnoredWhenAgentLacksCap(t *testing.T) {
	// Without embeddedContext the relay can't pass parsed text inline,
	// so it falls back to the universal path: download + file:// link.
	body := "hello world"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	// EmbeddedContext defaults to false.
	dir := t.TempDir()
	r, err := New(Config{Agent: agent, StateDir: dir, SessionTTL: time.Hour, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	turns := []Turn{{
		Role:      "user",
		MessageID: "m1",
		Content:   "summarise",
		Attachments: []Attachment{{
			URL:           srv.URL + "/doc.txt",
			ContentType:   "text/plain",
			Name:          "doc.txt",
			ParsedContent: "hello world", // unused without embeddedContext
		}},
	}}
	if err := r.Prompt(context.Background(), "conv-noemb", "u", turns, Options{}, &captureSink{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	rl := agent.lastPromptBlocks[1].ResourceLink
	if rl == nil {
		t.Fatalf("expected ResourceLink, got %+v", agent.lastPromptBlocks[1])
	}
	if !strings.HasPrefix(rl.Uri, "file://") {
		t.Fatalf("uri=%q want file://", rl.Uri)
	}
	if rl.MimeType == nil || *rl.MimeType != "text/plain" {
		t.Fatalf("mime=%v", rl.MimeType)
	}
}

// tinyPNG is a minimal valid 1x1 PNG body, used as a stand-in image
// payload for download tests.
var tinyPNG = []byte{
	0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
	0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
	0x89, 0x00, 0x00, 0x00, 0x0D, 0x49, 0x44, 0x41,
	0x54, 0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00,
	0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE,
	0x42, 0x60, 0x82,
}

// attachmentSrv is a tiny httptest server that serves whatever bytes
// the test routes to it. The path is used as-is so collision tests can
// have the same logical filename.
type attachSrv struct {
	*httptest.Server
	mu     sync.Mutex
	routes map[string]attachRoute
	hits   int32
}

type attachRoute struct {
	body []byte
	ct   string
}

func newAttachSrv(t *testing.T) *attachSrv {
	t.Helper()
	s := &attachSrv{routes: map[string]attachRoute{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&s.hits, 1)
		s.mu.Lock()
		route, ok := s.routes[r.URL.Path]
		s.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		if route.ct != "" {
			w.Header().Set("Content-Type", route.ct)
		}
		_, _ = w.Write(route.body)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *attachSrv) serve(path, ct string, body []byte) string {
	s.mu.Lock()
	s.routes[path] = attachRoute{body: body, ct: ct}
	s.mu.Unlock()
	return s.URL + path
}

func TestRouter_ImageDownloadEmitsLinkAndInlineImageBlock(t *testing.T) {
	srv := newAttachSrv(t)
	url := srv.serve("/cat.png", "image/png", tinyPNG)
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	dir := t.TempDir()
	r, err := New(Config{Agent: agent, StateDir: dir, SessionTTL: time.Hour, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	turns := []Turn{{
		Role: "user", MessageID: "m1", Content: "what is this?",
		Attachments: []Attachment{{URL: url, ContentType: "image/png", Name: "cat.png"}},
	}}
	if err := r.Prompt(context.Background(), "conv-img", "u", turns, Options{}, &captureSink{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.lastPromptBlocks) != 3 {
		t.Fatalf("blocks=%d want 3 (text + link + image)", len(agent.lastPromptBlocks))
	}
	rl := agent.lastPromptBlocks[1].ResourceLink
	if rl == nil || !strings.HasPrefix(rl.Uri, "file://") {
		t.Fatalf("block[1] not file:// ResourceLink: %+v", agent.lastPromptBlocks[1])
	}
	if rl.MimeType == nil || *rl.MimeType != "image/png" {
		t.Fatalf("mime=%v", rl.MimeType)
	}
	expectedPath := filepath.Join(dir, "convs", "conv-img", ".poe-attachments", "m1", "cat.png")
	if rl.Uri != "file://"+expectedPath {
		t.Fatalf("uri=%q want file://%s", rl.Uri, expectedPath)
	}
	// File on disk, content matches.
	disk, err := os.ReadFile(expectedPath)
	if err != nil {
		t.Fatalf("read %s: %v", expectedPath, err)
	}
	if !bytes.Equal(disk, tinyPNG) {
		t.Fatalf("disk bytes mismatch")
	}
	// Inline ImageBlock follows.
	img := agent.lastPromptBlocks[2].Image
	if img == nil {
		t.Fatalf("block[2] not Image: %+v", agent.lastPromptBlocks[2])
	}
	if img.Data != base64.StdEncoding.EncodeToString(tinyPNG) {
		t.Fatalf("inline data mismatch")
	}
	if img.MimeType != "image/png" {
		t.Fatalf("inline mime=%q", img.MimeType)
	}
}

func TestRouter_ImageOverInlineCapStillEmitsFileLink(t *testing.T) {
	body := bytes.Repeat([]byte{0xab}, 4096)
	srv := newAttachSrv(t)
	url := srv.serve("/big.png", "image/png", body)
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	dir := t.TempDir()
	r, err := New(Config{
		Agent: agent, StateDir: dir, SessionTTL: time.Hour,
		HTTPClient:          srv.Client(),
		MaxInlineImageBytes: 1024, // 4096-byte body overflows inline budget
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	turns := []Turn{{
		Role: "user", MessageID: "m1", Content: "x",
		Attachments: []Attachment{{URL: url, ContentType: "image/png", Name: "big.png"}},
	}}
	if err := r.Prompt(context.Background(), "conv-img-big", "u", turns, Options{}, &captureSink{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.lastPromptBlocks) != 2 {
		t.Fatalf("blocks=%d want 2 (text + link, no inline)", len(agent.lastPromptBlocks))
	}
	if agent.lastPromptBlocks[1].ResourceLink == nil {
		t.Fatalf("block[1] not ResourceLink: %+v", agent.lastPromptBlocks[1])
	}
	// File should still be on disk for the agent's tools to consume.
	expectedPath := filepath.Join(dir, "convs", "conv-img-big", ".poe-attachments", "m1", "big.png")
	if _, err := os.Stat(expectedPath); err != nil {
		t.Fatalf("disk file missing: %v", err)
	}
}

func TestRouter_HEICEmitsFileLinkOnly(t *testing.T) {
	body := []byte("\x00\x00\x00\x18ftypheic")
	srv := newAttachSrv(t)
	url := srv.serve("/photo.heic", "image/heic", body)
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	dir := t.TempDir()
	r, err := New(Config{Agent: agent, StateDir: dir, SessionTTL: time.Hour, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	turns := []Turn{{
		Role: "user", MessageID: "m1", Content: "see this",
		Attachments: []Attachment{{URL: url, ContentType: "image/heic", Name: "photo.heic"}},
	}}
	if err := r.Prompt(context.Background(), "conv-heic", "u", turns, Options{}, &captureSink{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.lastPromptBlocks) != 2 {
		t.Fatalf("blocks=%d want 2 (text + file link, no inline for HEIC)", len(agent.lastPromptBlocks))
	}
	if agent.lastPromptBlocks[1].Image != nil {
		t.Fatalf("HEIC must not be inlined")
	}
	rl := agent.lastPromptBlocks[1].ResourceLink
	if rl == nil || !strings.HasPrefix(rl.Uri, "file://") {
		t.Fatalf("block[1] not file:// link: %+v", agent.lastPromptBlocks[1])
	}
}

func TestRouter_PDFEmitsFileLinkOnly(t *testing.T) {
	body := []byte("%PDF-1.4 fake")
	srv := newAttachSrv(t)
	url := srv.serve("/report.pdf", "application/pdf", body)
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	dir := t.TempDir()
	r, err := New(Config{Agent: agent, StateDir: dir, SessionTTL: time.Hour, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	turns := []Turn{{
		Role: "user", MessageID: "m1", Content: "read",
		Attachments: []Attachment{{URL: url, ContentType: "application/pdf", Name: "report.pdf"}},
	}}
	if err := r.Prompt(context.Background(), "conv-pdf", "u", turns, Options{}, &captureSink{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.lastPromptBlocks) != 2 {
		t.Fatalf("blocks=%d want 2", len(agent.lastPromptBlocks))
	}
	rl := agent.lastPromptBlocks[1].ResourceLink
	if rl == nil || !strings.HasPrefix(rl.Uri, "file://") || !strings.Contains(rl.Uri, "report.pdf") {
		t.Fatalf("block[1] not file:// link: %+v", agent.lastPromptBlocks[1])
	}
	if rl.MimeType == nil || *rl.MimeType != "application/pdf" {
		t.Fatalf("mime=%v", rl.MimeType)
	}
}

func TestRouter_ParsedContentSkipsDownload(t *testing.T) {
	// With embeddedContext + parsed_content, no fetch should happen and
	// no file should be written.
	srv := newAttachSrv(t)
	srv.serve("/doc.txt", "text/plain", []byte("from-server"))
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	agent.caps = acpclient.Caps{EmbeddedContext: true}
	dir := t.TempDir()
	r, err := New(Config{Agent: agent, StateDir: dir, SessionTTL: time.Hour, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	turns := []Turn{{
		Role: "user", MessageID: "m1", Content: "summarise",
		Attachments: []Attachment{{
			URL: srv.URL + "/doc.txt", ContentType: "text/plain", Name: "doc.txt",
			ParsedContent: "hello world",
		}},
	}}
	if err := r.Prompt(context.Background(), "conv-parsed", "u", turns, Options{}, &captureSink{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if h := atomic.LoadInt32(&srv.hits); h != 0 {
		t.Fatalf("server hits=%d want 0 (parsed_content path must not fetch)", h)
	}
	// No file should have been written.
	dirPath := filepath.Join(dir, "convs", "conv-parsed", ".poe-attachments")
	if _, err := os.Stat(dirPath); err == nil {
		t.Fatalf(".poe-attachments unexpectedly created at %s", dirPath)
	}
}

func TestRouter_TextWithoutParsedContentDownloads(t *testing.T) {
	body := "log line\nsecond line\n"
	srv := newAttachSrv(t)
	url := srv.serve("/log.txt", "text/plain", []byte(body))
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	agent.caps = acpclient.Caps{EmbeddedContext: true}
	dir := t.TempDir()
	r, err := New(Config{Agent: agent, StateDir: dir, SessionTTL: time.Hour, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	turns := []Turn{{
		Role: "user", MessageID: "m1", Content: "look",
		Attachments: []Attachment{{URL: url, ContentType: "text/plain", Name: "log.txt"}},
	}}
	if err := r.Prompt(context.Background(), "conv-text", "u", turns, Options{}, &captureSink{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	expectedPath := filepath.Join(dir, "convs", "conv-text", ".poe-attachments", "m1", "log.txt")
	disk, err := os.ReadFile(expectedPath)
	if err != nil {
		t.Fatalf("read %s: %v", expectedPath, err)
	}
	if string(disk) != body {
		t.Fatalf("disk=%q want %q", disk, body)
	}
}

func TestRouter_HostileNameContainedByOSRoot(t *testing.T) {
	srv := newAttachSrv(t)
	url := srv.serve("/evil", "image/png", tinyPNG)
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	dir := t.TempDir()
	r, err := New(Config{Agent: agent, StateDir: dir, SessionTTL: time.Hour, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	turns := []Turn{{
		Role: "user", MessageID: "m1", Content: "x",
		Attachments: []Attachment{{
			URL: url, ContentType: "image/png", Name: "../../../../etc/passwd",
		}},
	}}
	if err := r.Prompt(context.Background(), "conv-hostile", "u", turns, Options{}, &captureSink{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	// /etc/passwd must not exist as a side effect, and crucially the
	// real /etc/passwd must not have been touched (it didn't, because
	// os.Root rejected the path; we used the fallback hash name).
	msgDir := filepath.Join(dir, "convs", "conv-hostile", ".poe-attachments", "m1")
	entries, err := os.ReadDir(msgDir)
	if err != nil {
		t.Fatalf("read msg dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries=%d want 1", len(entries))
	}
	if !strings.HasPrefix(entries[0].Name(), "attachment-") {
		t.Fatalf("name=%q want fallback 'attachment-...'", entries[0].Name())
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	rl := agent.lastPromptBlocks[1].ResourceLink
	if rl == nil || !strings.HasPrefix(rl.Uri, "file://"+msgDir+string(os.PathSeparator)) {
		t.Fatalf("link did not stay inside msg dir: %+v", rl)
	}
}

func TestRouter_DuplicateNamesGetCollisionSuffix(t *testing.T) {
	srv := newAttachSrv(t)
	urlA := srv.serve("/a", "image/png", []byte("A"))
	urlB := srv.serve("/b", "image/png", []byte("BB"))
	urlC := srv.serve("/c", "image/png", []byte("CCC"))
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	dir := t.TempDir()
	r, err := New(Config{Agent: agent, StateDir: dir, SessionTTL: time.Hour, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	turns := []Turn{{
		Role: "user", MessageID: "m1", Content: "three same names",
		Attachments: []Attachment{
			{URL: urlA, ContentType: "image/png", Name: "shot.png"},
			{URL: urlB, ContentType: "image/png", Name: "shot.png"},
			{URL: urlC, ContentType: "image/png", Name: "shot.png"},
		},
	}}
	if err := r.Prompt(context.Background(), "conv-dup", "u", turns, Options{}, &captureSink{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	msgDir := filepath.Join(dir, "convs", "conv-dup", ".poe-attachments", "m1")
	gotNames := map[string][]byte{}
	entries, err := os.ReadDir(msgDir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(msgDir, e.Name()))
		gotNames[e.Name()] = b
	}
	if !bytes.Equal(gotNames["shot.png"], []byte("A")) {
		t.Fatalf("shot.png=%q", gotNames["shot.png"])
	}
	if !bytes.Equal(gotNames["shot-2.png"], []byte("BB")) {
		t.Fatalf("shot-2.png=%q", gotNames["shot-2.png"])
	}
	if !bytes.Equal(gotNames["shot-3.png"], []byte("CCC")) {
		t.Fatalf("shot-3.png=%q", gotNames["shot-3.png"])
	}
}

func TestRouter_DownloadFailureFallsBackToHTTPSLink(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()
	agent := newFakeAgent(func(_ context.Context, a *fakeAgent, sid acp.SessionId, _ string) (acp.StopReason, error) {
		a.emit(sid, "ok")
		return acp.StopReasonEndTurn, nil
	})
	dir := t.TempDir()
	r, err := New(Config{Agent: agent, StateDir: dir, SessionTTL: time.Hour, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	turns := []Turn{{
		Role: "user", MessageID: "m1", Content: "x",
		Attachments: []Attachment{{URL: srv.URL + "/broken.png", ContentType: "image/png", Name: "broken.png"}},
	}}
	if err := r.Prompt(context.Background(), "conv-fail", "u", turns, Options{}, &captureSink{}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	rl := agent.lastPromptBlocks[1].ResourceLink
	if rl == nil {
		t.Fatalf("expected ResourceLink fallback: %+v", agent.lastPromptBlocks[1])
	}
	if !strings.HasPrefix(rl.Uri, srv.URL) {
		t.Fatalf("uri=%q want https fallback to original url", rl.Uri)
	}
	if rl.MimeType == nil || *rl.MimeType != "image/png" {
		t.Fatalf("mime=%v", rl.MimeType)
	}
}

func TestRouter_AttachmentTTLClampedToSessionTTL(t *testing.T) {
	// Capture log output to verify the warn line.
	var buf strings.Builder
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	dir := t.TempDir()
	agent := newFakeAgent(func(context.Context, *fakeAgent, acp.SessionId, string) (acp.StopReason, error) {
		return acp.StopReasonEndTurn, nil
	})
	r, err := New(Config{
		Agent: agent, StateDir: dir,
		SessionTTL:    time.Hour,
		AttachmentTTL: time.Minute, // shorter than SessionTTL → should clamp
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if r.cfg.AttachmentTTL != time.Hour {
		t.Fatalf("AttachmentTTL=%s want %s", r.cfg.AttachmentTTL, time.Hour)
	}
	if !strings.Contains(buf.String(), "AttachmentTTL") || !strings.Contains(buf.String(), "clamping") {
		t.Fatalf("expected warn log, got %q", buf.String())
	}
}

func TestRouter_SweepRemovesStaleFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	clock := now
	agent := newFakeAgent(func(context.Context, *fakeAgent, acp.SessionId, string) (acp.StopReason, error) {
		return acp.StopReasonEndTurn, nil
	})
	r, err := New(Config{
		Agent: agent, StateDir: dir,
		SessionTTL:    time.Minute,
		AttachmentTTL: time.Minute,
		Now:           func() time.Time { return clock },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Build a tree by hand.
	convDir := filepath.Join(dir, "convs", "conv-sweep", ".poe-attachments")
	oldMsg := filepath.Join(convDir, "old-msg")
	freshMsg := filepath.Join(convDir, "fresh-msg")
	for _, p := range []string{oldMsg, freshMsg} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	oldFile := filepath.Join(oldMsg, "stale.png")
	freshFile := filepath.Join(freshMsg, "fresh.png")
	if err := os.WriteFile(oldFile, []byte("old"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(freshFile, []byte("fresh"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Backdate the old file's mtime.
	past := now.Add(-2 * time.Minute)
	if err := os.Chtimes(oldFile, past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	r.sweepAttachmentsOnce()

	if _, err := os.Stat(oldFile); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stale file still present: %v", err)
	}
	if _, err := os.Stat(oldMsg); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("empty old-msg dir not removed: %v", err)
	}
	if _, err := os.Stat(freshFile); err != nil {
		t.Fatalf("fresh file removed: %v", err)
	}
	if _, err := os.Stat(freshMsg); err != nil {
		t.Fatalf("fresh msg dir removed: %v", err)
	}
}

// eventKind distinguishes SSE event types in the ordering test.
type eventKind string

const (
	evReplace eventKind = "replace"
	evText    eventKind = "text"
)

type sseEvent struct {
	kind eventKind
	body string
}

// eventSink records Replace and Text calls in arrival order, simulating
// the spinner-clearing behaviour of handler.sink: FirstChunk emits
// Replace("") to wipe the spinner before real content arrives.
type eventSink struct {
	mu     sync.Mutex
	events []sseEvent
}

func (s *eventSink) FirstChunk() {
	s.mu.Lock()
	s.events = append(s.events, sseEvent{evReplace, ""})
	s.mu.Unlock()
}
func (s *eventSink) Text(t string) error {
	s.mu.Lock()
	s.events = append(s.events, sseEvent{evText, t})
	s.mu.Unlock()
	return nil
}
func (s *eventSink) Replace(t string) error   { return nil }
func (s *eventSink) Error(t, et string) error { return nil }
func (s *eventSink) Done() error              { return nil }

// simulatedContent replays an event sequence as Poe renders it:
// Replace("") wipes all prior Text; subsequent Text events append.
func simulatedContent(events []sseEvent) string {
	var buf strings.Builder
	for _, ev := range events {
		switch ev.kind {
		case evReplace:
			buf.Reset()
			buf.WriteString(ev.body)
		case evText:
			buf.WriteString(ev.body)
		}
	}
	return buf.String()
}

// TestRouter_FirstChunkReplaceCannotWipeSubsequentText is the regression
// test for the "missing text content" bug.
//
// Root cause: the ACP SDK spawns one goroutine per notification. In the
// old per-turn-channel design a race between goroutine A's
// FirstChunk→Replace("") and goroutine B's Text could erase B's content.
// With the session-lifetime channel the drain goroutine is the sole sink
// writer so the race is structurally impossible.
//
// The test wires up a full session-lifetime drain goroutine, sends two
// chunks concurrently via OnUpdate, and verifies both survive.
func TestRouter_FirstChunkReplaceCannotWipeSubsequentText(t *testing.T) {
	rec := &eventSink{}
	st := &sessionState{
		convID:    "test",
		chunkCh:   make(chan chunkMsg, 64),
		drainStop: make(chan struct{}),
	}
	go st.drainChunks()

	// Begin turn so the drain goroutine has a sink to write to.
	st.chunkCh <- chunkMsg{beginTurn: &turnDef{sink: rec}}

	makeChunk := func(text string) acp.SessionNotification {
		return acp.SessionNotification{
			SessionId: "test",
			Update: acp.SessionUpdate{
				AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
					Content: acp.TextBlock(text),
				},
			},
		}
	}

	// Send two chunks concurrently; the channel serialises them.
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, chunk := range []string{"alpha", "beta"} {
		chunk := chunk
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_ = st.OnUpdate(context.Background(), makeChunk(chunk))
		}()
	}
	close(start)
	wg.Wait()

	// End the turn and wait for the drain goroutine to finish processing.
	endDone := make(chan struct{})
	st.chunkCh <- chunkMsg{endTurn: endDone}
	<-endDone

	// Both chunks must be visible; neither should be erased by Replace("").
	content := simulatedContent(rec.events)
	if content != "alphabeta" && content != "betaalpha" {
		t.Errorf("content = %q; want both chunks present (neither erased by Replace)", content)
	}

	close(st.drainStop)
}

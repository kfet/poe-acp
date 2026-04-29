package router

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/poe-acp-relay/internal/acpclient"
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
	caps          acpclient.Caps
	listResult    []acpclient.SessionInfo
	listErr       error
	resumeErr     error
	listCalls     int32
	resumeCalls   int32
	newSessCalls  int32
	lastPromptTxt string
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
func (f *fakeAgent) NewSession(_ context.Context, _ string, sink acpclient.SessionUpdateSink) (acp.SessionId, error) {
	atomic.AddInt32(&f.newSessCalls, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := acp.SessionId("sess-" + time.Now().Format("150405") + "-" + itoa(f.nextID))
	f.sinks[id] = sink
	return id, nil
}

func (f *fakeAgent) Prompt(ctx context.Context, sid acp.SessionId, text string) (acp.StopReason, error) {
	atomic.AddInt32(&f.prompts, 1)
	f.mu.Lock()
	f.lastPromptTxt = text
	f.mu.Unlock()
	return f.onPrompt(ctx, f, sid, text)
}

func (f *fakeAgent) Cancel(_ context.Context, _ acp.SessionId) error { return nil }
func (f *fakeAgent) SetModel(_ context.Context, _ acp.SessionId, _ string) error {
	return nil
}
func (f *fakeAgent) SetConfigOption(_ context.Context, _ acp.SessionId, _, _ string) error {
	return nil
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

package router

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
	"github.com/kfet/acp-kit/interject"
)

// ijAgent is a forkAgent with CloseSession whose main-session prompts
// block until released, so a second message lands mid-turn.
type ijAgent struct {
	*forkAgent
	started chan struct{} // one send per main prompt
	release chan struct{} // one receive per main prompt
	forkOut string
	onFork  func() // runs inside the fork's prompt
	// cancelled, when set, receives once per Cancel.
	cancelled chan struct{}
	closes    int32

	mu    sync.Mutex
	mains []string
}

func (a *ijAgent) Cancel(ctx context.Context, sid acp.SessionId) error {
	err := a.fakeAgent.Cancel(ctx, sid)
	if a.cancelled != nil {
		a.cancelled <- struct{}{}
	}
	return err
}

func (a *ijAgent) CloseSession(context.Context, acp.SessionId) error {
	atomic.AddInt32(&a.closes, 1)
	return nil
}

func newIJAgent(forkOut string) *ijAgent {
	a := &ijAgent{forkAgent: newForkAgent(), started: make(chan struct{}, 8), release: make(chan struct{}, 8), forkOut: forkOut}
	a.onPrompt = func(_ context.Context, fa *fakeAgent, sid acp.SessionId, text string) (acp.StopReason, error) {
		if strings.HasPrefix(string(sid), "fork-") {
			if a.onFork != nil {
				a.onFork()
			}
			fa.emit(sid, a.forkOut)
			return acp.StopReasonEndTurn, nil
		}
		a.mu.Lock()
		a.mains = append(a.mains, text)
		a.mu.Unlock()
		a.started <- struct{}{}
		<-a.release
		fa.emit(sid, "main")
		return acp.StopReasonEndTurn, nil
	}
	return a
}

func (a *ijAgent) mainTexts() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.mains...)
}

func ijRouter(t *testing.T, a Agent) *Router {
	t.Helper()
	r, err := New(Config{Agent: a, StateDir: t.TempDir(), SessionTTL: time.Hour, Interject: interject.Config{Enabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func q(text string) []Turn { return []Turn{{Role: "user", Content: text, MessageID: "m-" + text}} }

// startMain runs a first message and waits until its main turn is live.
func startMain(t *testing.T, r *Router, a *ijAgent) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- r.Prompt(context.Background(), "c", "u", q("first"), Options{}, &captureSink{}) }()
	<-a.started
	return done
}

func sinkText(s *captureSink) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.text.String()
}

func TestInterject_NoneAnswersAndMainContinues(t *testing.T) {
	a := newIJAgent("it is 4\n1 9")
	r := ijRouter(t, a)
	done := startMain(t, r, a)

	s := &captureSink{}
	if err := r.Prompt(context.Background(), "c", "u", q("2+2?"), Options{}, s); err != nil {
		t.Fatal(err)
	}
	if got := sinkText(s); !strings.Contains(got, "it is 4") || !strings.Contains(got, "quick reply · answered") || !s.done || !s.firstCalled {
		t.Fatalf("reply=%q done=%v", got, s.done)
	}
	if atomic.LoadInt32(&a.cancelCalls) != 0 || atomic.LoadInt32(&a.closes) != 1 {
		t.Fatalf("cancel=%d closes=%d", a.cancelCalls, a.closes)
	}
	a.release <- struct{}{}
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	// The next main prompt carries the side Q&A.
	go func() { a.release <- struct{}{} }()
	if err := r.Prompt(context.Background(), "c", "u", q("next"), Options{}, &captureSink{}); err != nil {
		t.Fatal(err)
	}
	<-a.started
	m := a.mainTexts()
	if last := m[len(m)-1]; !strings.Contains(last, "Q: 2+2?\nA: it is 4") || !strings.HasSuffix(last, "next") {
		t.Fatalf("next prompt=%q", last)
	}
}

func TestInterject_EmptyAnswerPostsTagOnly(t *testing.T) {
	a := newIJAgent("1 9")
	r := ijRouter(t, a)
	done := startMain(t, r, a)
	s := &captureSink{}
	if err := r.Prompt(context.Background(), "c", "u", q("hm"), Options{}, s); err != nil {
		t.Fatal(err)
	}
	if got := sinkText(s); got != "*(quick reply · answered)*\n\n" || !s.done {
		t.Fatalf("reply=%q done=%v", got, s.done)
	}
	a.release <- struct{}{}
	<-done
}

func TestInterject_AbortCancelsMain(t *testing.T) {
	a := newIJAgent("stopping\n4 9")
	r := ijRouter(t, a)
	done := startMain(t, r, a)
	s := &captureSink{}
	if err := r.Prompt(context.Background(), "c", "u", q("stop"), Options{}, s); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&a.cancelCalls) != 1 || !s.done || !strings.Contains(sinkText(s), "stopping") {
		t.Fatalf("cancel=%d reply=%q", a.cancelCalls, sinkText(s))
	}
	a.release <- struct{}{}
	<-done
}

func TestInterject_SteerCancelsAndReprompts(t *testing.T) {
	a := newIJAgent("ok, redirecting\n3 9")
	r := ijRouter(t, a)
	a.cancelled = make(chan struct{}, 1)
	done := startMain(t, r, a)
	s := &captureSink{}
	steer := make(chan error, 1)
	go func() { steer <- r.Prompt(context.Background(), "c", "u", q("do Y"), Options{}, s) }()
	<-a.cancelled
	a.release <- struct{}{} // main ends (the fake ignores cancel)
	<-done
	<-a.started
	a.release <- struct{}{}
	if err := <-steer; err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&a.cancelCalls) != 1 {
		t.Fatalf("cancel=%d", a.cancelCalls)
	}
	m := a.mainTexts()
	if last := m[len(m)-1]; !strings.HasSuffix(last, "do Y") || !strings.Contains(last, "A: ok, redirecting") {
		t.Fatalf("steer prompt=%q", last)
	}
	if got := sinkText(s); !strings.Contains(got, "redirecting") || !strings.Contains(got, "main") {
		t.Fatalf("sink=%q", got)
	}
}

func TestInterject_QueueWhileMainRuns(t *testing.T) {
	a := newIJAgent("noted\n2 9")
	r := ijRouter(t, a)
	done := startMain(t, r, a)
	s := &captureSink{}
	if err := r.Prompt(context.Background(), "c", "u", q("also Z"), Options{}, s); err != nil {
		t.Fatal(err)
	}
	if !s.done || !r.ij.Queued("c") {
		t.Fatal("queued reply not finalised / message not queued")
	}
	a.release <- struct{}{}
	<-done
}

func TestInterject_QueueAfterMainEndedStartsTurn(t *testing.T) {
	a := newIJAgent("noted\n2 9")
	r := ijRouter(t, a)
	done := startMain(t, r, a)
	a.onFork = func() {
		a.release <- struct{}{}
		<-done
	}
	s := &captureSink{}
	res := make(chan error, 1)
	go func() { res <- r.Prompt(context.Background(), "c", "u", q("also Z"), Options{}, s) }()
	<-a.started
	a.release <- struct{}{}
	if err := <-res; err != nil {
		t.Fatal(err)
	}
	m := a.mainTexts()
	last := m[len(m)-1]
	if strings.Count(last, "also Z") != 2 || !strings.Contains(last, "[queued messages from the user]\nalso Z") {
		t.Fatalf("queued turn prompt=%q", last)
	}
	if r.ij.Queued("c") {
		t.Fatal("queue not drained")
	}
}

func TestInterject_FallbacksQueueBehindMain(t *testing.T) {
	for name, mk := range map[string]func() (Agent, *ijAgent){
		"fork error": func() (Agent, *ijAgent) {
			a := newIJAgent("x\n1 9")
			a.forkErr = errors.New("boom")
			return a, a
		},
		"no fork support": func() (Agent, *ijAgent) {
			a := newIJAgent("x\n1 9")
			return noForkAgent{a.fakeAgent}, a
		},
	} {
		t.Run(name, func(t *testing.T) {
			ag, a := mk()
			r := ijRouter(t, ag)
			done := startMain(t, r, a)
			r.mu.Lock()
			st := r.sessions["c"]
			r.mu.Unlock()
			if got := r.interjectTurn(context.Background(), st, "m-later", "later", &captureSink{}); got != ijFallback {
				t.Fatalf("outcome=%d, want fallback", got)
			}
			a.release <- struct{}{}
			<-done
		})
	}
}

// noForkAgent hides ForkSession.
type noForkAgent struct{ *fakeAgent }

func TestInterject_CommandSkipsInterject(t *testing.T) {
	a := newIJAgent("x\n1 9")
	r := ijRouter(t, a)
	done := startMain(t, r, a)
	res := make(chan error, 1)
	go func() { res <- r.Prompt(context.Background(), "c", "u", q("/session"), Options{}, &captureSink{}) }()
	a.release <- struct{}{}
	<-done
	<-a.started
	a.release <- struct{}{}
	<-res
	if atomic.LoadInt32(&a.forkCalls) != 0 {
		t.Fatal("command must not be interjected")
	}
}

func TestWithSidePrefix_NonTextFirstBlock(t *testing.T) {
	r := ijRouter(t, newIJAgent(""))
	img := acp.ImageBlock("data", "image/png")
	if got := r.withSidePrefix("c", []acp.ContentBlock{img}); len(got) != 1 {
		t.Fatalf("no prefix must keep blocks: %d", len(got))
	}
	r.ij.Take("c")
	// Seed side state through a real Ask.
	a := newIJAgent("hi\n1 9")
	ctx := context.WithValue(context.Background(), agentCtxKey{}, Agent(a))
	if _, err := r.ij.Ask(ctx, "c", "", "s", "q"); err != nil {
		t.Fatal(err)
	}
	got := r.withSidePrefix("c", []acp.ContentBlock{img})
	if len(got) != 2 || got[0].Text == nil || !strings.Contains(got[0].Text.Text, "Q: q") || got[1].Image == nil {
		t.Fatalf("got %+v", got)
	}
}

func TestAgentDispatch_NoAgentInCtx(t *testing.T) {
	if _, err := (agentDispatch{}).ForkSession(context.Background(), "", "p", "", nil); !errors.Is(err, client.ErrForkUnsupported) {
		t.Fatalf("err=%v", err)
	}
}

func TestInterject_RedriveOfLiveTurnFallsBack(t *testing.T) {
	a := newIJAgent("x\n1 9")
	r := ijRouter(t, a)
	done := startMain(t, r, a)
	r.mu.Lock()
	st := r.sessions["c"]
	r.mu.Unlock()
	if got := r.interjectTurn(context.Background(), st, "m-first", "first", &captureSink{}); got != ijFallback {
		t.Fatalf("outcome=%d, want fallback", got)
	}
	if atomic.LoadInt32(&a.forkCalls) != 0 {
		t.Fatal("redrive must not fork")
	}
	a.release <- struct{}{}
	<-done
}

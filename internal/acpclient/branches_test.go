package acpclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// fakeAgent is a minimal in-process ACP agent implementation. The relay
// side talks to it over io.Pipe pairs; nothing leaves the test binary.
type fakeAgent struct {
	conn     *acp.Connection
	closeIn  io.Closer
	closeOut io.Closer

	// Behaviour knobs (set before the connection starts processing).
	initRaw      json.RawMessage // raw initialize response body
	newSessResp  acp.NewSessionResponse
	newSessErr   *acp.RequestError
	listResp     json.RawMessage
	listErr      *acp.RequestError
	resumeErr    *acp.RequestError
	promptStop   acp.StopReason
	promptErr    *acp.RequestError
	setModelErr  *acp.RequestError
	setConfigErr *acp.RequestError
	authResp     json.RawMessage
	authErr      *acp.RequestError
	updates      []acp.SessionNotification // emitted before responding to prompt
	cancelled    atomic.Bool
	cancelCh     chan struct{} // closed on first session/cancel; nil-safe

	// Capture for assertions.
	mu             sync.Mutex
	gotPromptCount int
	gotSetModelArg string
}

func (f *fakeAgent) handle(ctx context.Context, method string, params json.RawMessage) (any, *acp.RequestError) {
	switch method {
	case acp.AgentMethodInitialize:
		if len(f.initRaw) > 0 {
			return json.RawMessage(f.initRaw), nil
		}
		return acp.InitializeResponse{ProtocolVersion: acp.ProtocolVersionNumber}, nil
	case acp.AgentMethodSessionNew:
		if f.newSessErr != nil {
			return nil, f.newSessErr
		}
		return f.newSessResp, nil
	case acp.AgentMethodSessionPrompt:
		f.mu.Lock()
		f.gotPromptCount++
		f.mu.Unlock()
		var p acp.PromptRequest
		_ = json.Unmarshal(params, &p)
		// Emit pre-recorded updates.
		for _, u := range f.updates {
			u.SessionId = p.SessionId
			_ = f.conn.SendNotification(ctx, acp.ClientMethodSessionUpdate, u)
		}
		if f.promptErr != nil {
			return nil, f.promptErr
		}
		stop := f.promptStop
		if stop == "" {
			stop = acp.StopReasonEndTurn
		}
		return acp.PromptResponse{StopReason: stop}, nil
	case "session/cancel":
		if f.cancelled.CompareAndSwap(false, true) && f.cancelCh != nil {
			close(f.cancelCh)
		}
		return nil, nil
	case acp.AgentMethodSessionSetModel:
		var p acp.UnstableSetSessionModelRequest
		_ = json.Unmarshal(params, &p)
		f.mu.Lock()
		f.gotSetModelArg = string(p.ModelId)
		f.mu.Unlock()
		if f.setModelErr != nil {
			return nil, f.setModelErr
		}
		return acp.UnstableSetSessionModelResponse{}, nil
	case "session/set_config_option":
		if f.setConfigErr != nil {
			return nil, f.setConfigErr
		}
		return json.RawMessage("{}"), nil
	case "session/list":
		if f.listErr != nil {
			return nil, f.listErr
		}
		if len(f.listResp) > 0 {
			return json.RawMessage(f.listResp), nil
		}
		return json.RawMessage(`{"sessions":[]}`), nil
	case "session/resume":
		if f.resumeErr != nil {
			return nil, f.resumeErr
		}
		return json.RawMessage("{}"), nil
	case acp.AgentMethodAuthenticate:
		if f.authErr != nil {
			return nil, f.authErr
		}
		if len(f.authResp) > 0 {
			return json.RawMessage(f.authResp), nil
		}
		return json.RawMessage("{}"), nil
	}
	return nil, acp.NewMethodNotFound(method)
}

// startFakeAgent wires a fake agent + connect()-based AgentProc over a pipe pair.
func startFakeAgent(t *testing.T, fa *fakeAgent, cfg Config) *AgentProc {
	t.Helper()
	// Two pipes: client→agent (clientStdin → agentStdin), agent→client.
	cs2as_r, cs2as_w := io.Pipe()
	as2cs_r, as2cs_w := io.Pipe()

	fa.conn = acp.NewConnection(fa.handle, as2cs_w, cs2as_r)
	fa.closeIn = cs2as_r
	fa.closeOut = as2cs_w

	// connect() is the post-spawn handshake; pass nil for cmd.
	a, err := connect(context.Background(), cfg, nil, cs2as_w, as2cs_r)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		_ = cs2as_w.Close()
		_ = as2cs_r.Close()
		_ = cs2as_r.Close()
		_ = as2cs_w.Close()
	})
	return a
}

type stubPolicy struct{}

func (stubPolicy) Decide(_ context.Context, _ acp.RequestPermissionRequest) acp.RequestPermissionResponse {
	return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{}}
}

type capturingSink struct {
	mu      sync.Mutex
	updates []acp.SessionNotification
	gotCh   chan struct{} // closed on first OnUpdate; nil-safe
}

func (c *capturingSink) OnUpdate(_ context.Context, n acp.SessionNotification) error {
	c.mu.Lock()
	first := len(c.updates) == 0
	c.updates = append(c.updates, n)
	c.mu.Unlock()
	if first && c.gotCh != nil {
		close(c.gotCh)
	}
	return nil
}

func TestStart_ConfigErrors(t *testing.T) {
	if _, err := Start(context.Background(), Config{}); err == nil {
		t.Fatal("empty Command")
	}
	if _, err := Start(context.Background(), Config{Command: []string{"x"}}); err == nil {
		t.Fatal("nil Policy")
	}
	// Bad command path → exec start fails.
	if _, err := Start(context.Background(), Config{
		Command: []string{"/nonexistent/bin/please/no"},
		Policy:  stubPolicy{},
	}); err == nil {
		t.Fatal("expected start error")
	}
}

func TestStart_RealSubprocess_InitializeFails(t *testing.T) {
	// Use /bin/echo: process exits immediately, initialize hangs/fails.
	// Also exercises the cfg.Env != nil path.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := Start(ctx, Config{
		Command: []string{"/bin/echo"},
		Policy:  stubPolicy{},
		Env:     os.Environ(),
		Cwd:     t.TempDir(),
		Stderr:  io.Discard,
	}); err == nil {
		t.Fatal("expected initialize error")
	}
}

func TestStart_RealSubprocess_HappyPath(t *testing.T) {
	if os.Getenv("ACPCLIENT_FAKE_AGENT") == "1" {
		// Re-entered as the agent: serve a single initialize over stdio.
		runFakeAgentStdio()
		return
	}
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, err := Start(ctx, Config{
		Command: []string{exe, "-test.run=TestStart_RealSubprocess_HappyPath"},
		Env:     append(os.Environ(), "ACPCLIENT_FAKE_AGENT=1"),
		Policy:  stubPolicy{},
		Cwd:     t.TempDir(),
		Stderr:  io.Discard,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
}

// runFakeAgentStdio runs a minimal ACP agent on stdin/stdout that responds
// to the relay's initialize call with an empty success response.
func runFakeAgentStdio() {
	// Read until we see a complete JSON-RPC frame, then respond.
	dec := json.NewDecoder(os.Stdin)
	for {
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
		}
		if err := dec.Decode(&req); err != nil {
			return
		}
		if req.Method == "initialize" {
			resp := map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(req.ID),
				"result":  map[string]any{"protocolVersion": 1},
			}
			b, _ := json.Marshal(resp)
			b = append(b, '\n')
			os.Stdout.Write(b)
			return
		}
	}
}

func TestParseAuthMethods(t *testing.T) {
	got := parseAuthMethods(json.RawMessage(`{"authMethods":[{"id":"oauth-x","name":"X","type":"agent"}]}`))
	if len(got) != 1 || got[0].ID != "oauth-x" {
		t.Fatalf("got %+v", got)
	}
}

func TestParseAuthResult(t *testing.T) {
	got := parseAuthResult(json.RawMessage(`{"_meta":{"auth":{"state":"needs_redirect","id":"i","url":"u","instructions":"do this"}}}`))
	if got != (AuthResult{State: "needs_redirect", ID: "i", URL: "u", Instructions: "do this"}) {
		t.Fatalf("got %+v", got)
	}
}

func TestAgentProc_FullFlow(t *testing.T) {
	fa := &fakeAgent{
		initRaw: json.RawMessage(`{
			"protocolVersion":1,
			"agentCapabilities":{
				"loadSession":true,
				"sessionCapabilities":{"list":{},"resume":{}},
				"promptCapabilities":{"embeddedContext":true},
				"_meta":{"session.systemPrompt":{"version":1}}
			},
			"authMethods":[{"id":"oauth-anthropic","name":"Anthropic","type":"agent"}]
		}`),
		newSessResp: acp.NewSessionResponse{
			SessionId: "s1",
			Models: &acp.SessionModelState{
				CurrentModelId:  "anthropic/x",
				AvailableModels: []acp.ModelInfo{{ModelId: "anthropic/x", Name: "X"}},
			},
		},
		updates: []acp.SessionNotification{
			{Update: acp.SessionUpdate{
				AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("hello")},
			}},
		},
		listResp: json.RawMessage(`{"sessions":[{"sessionId":"prior-1","cwd":"/x"}]}`),
		cancelCh: make(chan struct{}),
	}
	cfg := Config{Command: []string{"/bin/true"}, Policy: stubPolicy{}, Cwd: t.TempDir()}
	a := startFakeAgent(t, fa, cfg)

	caps := a.Caps()
	if !caps.LoadSession || !caps.ListSessions || !caps.ResumeSession || !caps.EmbeddedContext || !caps.SystemPrompt {
		t.Fatalf("caps: %+v", caps)
	}
	if ms := a.AuthMethods(); len(ms) != 1 {
		t.Fatalf("AuthMethods: %+v", ms)
	}

	sink := &capturingSink{gotCh: make(chan struct{})}
	sid, err := a.NewSession(context.Background(), "/cwd", sink, []acp.ContentBlock{acp.TextBlock("system")})
	if err != nil {
		t.Fatal(err)
	}
	if sid != "s1" {
		t.Fatalf("sid=%v", sid)
	}
	if models, cur := a.Models(); len(models) != 1 || cur != "anthropic/x" {
		t.Fatalf("Models: %v %v", models, cur)
	}
	// Models with no list yet → empty.
	a2 := &AgentProc{}
	if m, c := a2.Models(); m != nil || c != "" {
		t.Fatal()
	}

	if err := a.SetModel(context.Background(), sid, "openai/gpt-5"); err != nil {
		t.Fatal(err)
	}
	if err := a.SetConfigOption(context.Background(), sid, "thinking_level", "high"); err != nil {
		t.Fatal(err)
	}
	stop, err := a.Prompt(context.Background(), sid, []acp.ContentBlock{acp.TextBlock("hi")})
	if err != nil {
		t.Fatal(err)
	}
	if stop != acp.StopReasonEndTurn {
		t.Fatalf("stop=%v", stop)
	}
	// Wait for update to be dispatched.
	select {
	case <-sink.gotCh:
	case <-time.After(3 * time.Second):
		t.Fatal("no updates")
	}
	sink.mu.Lock()
	got := len(sink.updates)
	sink.mu.Unlock()
	if got == 0 {
		t.Fatal("no updates")
	}

	if err := a.Cancel(context.Background(), sid); err != nil {
		t.Fatal(err)
	}
	// Wait for cancel notification.
	select {
	case <-fa.cancelCh:
	case <-time.After(3 * time.Second):
		t.Fatal("cancel not received")
	}

	sessions, err := a.ListSessions(context.Background(), "/x")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].SessionId != "prior-1" {
		t.Fatalf("sessions: %+v", sessions)
	}

	if err := a.ResumeSession(context.Background(), "/x", "prior-1", sink); err != nil {
		t.Fatal(err)
	}

	// Authenticate.
	fa.authResp = json.RawMessage(`{"_meta":{"auth":{"state":"ok","id":"i"}}}`)
	r, err := a.Authenticate(context.Background(), "oauth-anthropic", "i", "https://localhost/cb", false)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != "ok" {
		t.Fatalf("auth=%+v", r)
	}
	// Cancel form.
	if _, err := a.Authenticate(context.Background(), "oauth-anthropic", "i", "", true); err != nil {
		t.Fatal(err)
	}
}

func TestAgentProc_ProbeModels(t *testing.T) {
	fa := &fakeAgent{
		newSessResp: acp.NewSessionResponse{
			SessionId: "probe-s",
			Models: &acp.SessionModelState{
				CurrentModelId:  "x",
				AvailableModels: []acp.ModelInfo{{ModelId: "x"}},
			},
		},
	}
	a := startFakeAgent(t, fa, Config{Command: []string{"x"}, Policy: stubPolicy{}, Cwd: t.TempDir()})
	if err := a.ProbeModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Idempotent.
	if err := a.ProbeModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Probe with NewSession failure.
	fa2 := &fakeAgent{newSessErr: acp.NewInternalError(map[string]any{"e": "x"})}
	a2 := startFakeAgent(t, fa2, Config{Command: []string{"x"}, Policy: stubPolicy{}, Cwd: t.TempDir()})
	if err := a2.ProbeModels(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

func TestAgentProc_ErrorPaths(t *testing.T) {
	fa := &fakeAgent{
		newSessErr:   acp.NewInternalError(map[string]any{"e": "no"}),
		listErr:      acp.NewInternalError(map[string]any{"e": "no"}),
		resumeErr:    acp.NewInternalError(map[string]any{"e": "no"}),
		promptErr:    acp.NewInternalError(map[string]any{"e": "no"}),
		setModelErr:  acp.NewInternalError(map[string]any{"e": "no"}),
		setConfigErr: acp.NewInternalError(map[string]any{"e": "no"}),
		authErr:      acp.NewInternalError(map[string]any{"e": "no"}),
	}
	a := startFakeAgent(t, fa, Config{Command: []string{"x"}, Policy: stubPolicy{}, Cwd: t.TempDir()})

	if _, err := a.NewSession(context.Background(), "/x", &capturingSink{}, nil); err == nil {
		t.Fatal()
	}
	if _, err := a.ListSessions(context.Background(), "/x"); err == nil {
		t.Fatal()
	}
	if err := a.ResumeSession(context.Background(), "/x", "s", &capturingSink{}); err == nil {
		t.Fatal()
	}
	if _, err := a.Prompt(context.Background(), "s", nil); err == nil {
		t.Fatal()
	}
	if err := a.SetModel(context.Background(), "s", "m"); err == nil {
		t.Fatal()
	}
	if err := a.SetConfigOption(context.Background(), "s", "k", "v"); err == nil {
		t.Fatal()
	}
	if _, err := a.Authenticate(context.Background(), "m", "", "", false); err == nil {
		t.Fatal()
	}
}

func TestNoopSink(t *testing.T) {
	if err := (noopSink{}).OnUpdate(context.Background(), acp.SessionNotification{}); err != nil {
		t.Fatal(err)
	}
}

func TestToReqErr(t *testing.T) {
	e := toReqErr(errors.New("boom"))
	if e == nil || e.Code == 0 {
		t.Fatal()
	}
}

func TestAgentProc_Close(t *testing.T) {
	// nil cmd or process — returns nil.
	a := &AgentProc{}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	// Close with a real running process that exits on signal.
	cmd := exec.CommandContext(t.Context(), "sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("sleep unavailable: %v", err)
	}
	a2 := &AgentProc{cmd: cmd}
	if err := a2.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestAgentProc_Close_KillBranch forces the SIGKILL fallback by
// replacing the gentle signal with a no-op (signal 0, "presence
// check") so the child stays alive past closeKillTimeout, then
// shrinking the timeout so Kill fires promptly. Sending signal 0
// avoids the macOS-specific behaviour where wait4 reports a
// SIG_IGN'd child as exited.
func TestAgentProc_Close_KillBranch(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("sleep unavailable: %v", err)
	}
	defer swapVar(&closeKillTimeout, 50*time.Millisecond)()
	defer swapVar[os.Signal](&closeGentleSignal, syscall.Signal(0))()

	a := &AgentProc{cmd: cmd}
	done := make(chan error, 1)
	go func() { done <- a.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("Close did not return; kill branch may be broken")
	}
}

func swapVar[T any](dst *T, v T) func() {
	old := *dst
	*dst = v
	return func() { *dst = old }
}

func TestDispatch_AllPaths(t *testing.T) {
	a := &AgentProc{
		cfg:   Config{Policy: stubPolicy{}},
		sinks: map[acp.SessionId]SessionUpdateSink{},
	}
	ctx := context.Background()

	// Unknown method.
	if _, err := a.dispatch(ctx, "weird/method", nil); err == nil {
		t.Fatal("expected method-not-found")
	}
	// session/update with bad params → InvalidParams.
	_, e := a.dispatch(ctx, acp.ClientMethodSessionUpdate, json.RawMessage("not-json"))
	if e == nil {
		t.Fatal()
	}
	// session/update with no sink → nil error.
	good, _ := json.Marshal(acp.SessionNotification{
		SessionId: "no-sink",
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("x")},
		},
	})
	if _, e := a.dispatch(ctx, acp.ClientMethodSessionUpdate, good); e != nil {
		t.Fatalf("%+v", e)
	}
	// session/update with sink that errors.
	a.sinks["s1"] = errSink{}
	good2, _ := json.Marshal(acp.SessionNotification{
		SessionId: "s1",
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("x")},
		},
	})
	if _, e := a.dispatch(ctx, acp.ClientMethodSessionUpdate, good2); e == nil {
		t.Fatal("expected error from sink")
	}
	// session/request_permission bad params.
	if _, e := a.dispatch(ctx, acp.ClientMethodSessionRequestPermission, json.RawMessage("nope")); e == nil {
		t.Fatal()
	}
	// session/request_permission good.
	rp, _ := json.Marshal(acp.RequestPermissionRequest{})
	if _, e := a.dispatch(ctx, acp.ClientMethodSessionRequestPermission, rp); e != nil {
		t.Fatal(e)
	}
	// fs/read_text_file bad params.
	if _, e := a.dispatch(ctx, acp.ClientMethodFsReadTextFile, json.RawMessage("nope")); e == nil {
		t.Fatal()
	}
	// fs/read_text_file good.
	tmp := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(tmp, []byte("a\nb\nc\nd\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rr, _ := json.Marshal(acp.ReadTextFileRequest{Path: tmp})
	if _, e := a.dispatch(ctx, acp.ClientMethodFsReadTextFile, rr); e != nil {
		t.Fatal(e)
	}
	// fs/read_text_file error from file.
	bad, _ := json.Marshal(acp.ReadTextFileRequest{Path: "/nonexistent/" + t.Name() + "/x"})
	if _, e := a.dispatch(ctx, acp.ClientMethodFsReadTextFile, bad); e == nil {
		t.Fatal()
	}
	// fs/write_text_file bad params.
	if _, e := a.dispatch(ctx, acp.ClientMethodFsWriteTextFile, json.RawMessage("nope")); e == nil {
		t.Fatal()
	}
	// fs/write_text_file good.
	wp := filepath.Join(t.TempDir(), "out", "x.txt")
	wr, _ := json.Marshal(acp.WriteTextFileRequest{Path: wp, Content: "hello"})
	if _, e := a.dispatch(ctx, acp.ClientMethodFsWriteTextFile, wr); e != nil {
		t.Fatal(e)
	}
	// fs/write_text_file error from file.
	br, _ := json.Marshal(acp.WriteTextFileRequest{Path: "relative/path", Content: "x"})
	if _, e := a.dispatch(ctx, acp.ClientMethodFsWriteTextFile, br); e == nil {
		t.Fatal()
	}
}

type errSink struct{}

func (errSink) OnUpdate(context.Context, acp.SessionNotification) error {
	return errors.New("sink err")
}

func TestReadTextFile_LineLimit(t *testing.T) {
	a := &AgentProc{}
	tmp := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(tmp, []byte("a\nb\nc\nd\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Relative path → error.
	if _, err := a.readTextFile(acp.ReadTextFileRequest{Path: "rel"}); err == nil {
		t.Fatal()
	}
	line := 2
	limit := 2
	r, err := a.readTextFile(acp.ReadTextFileRequest{Path: tmp, Line: &line, Limit: &limit})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Content, "b") || strings.Contains(r.Content, "d") {
		t.Fatalf("got %q", r.Content)
	}
	// Line beyond file.
	huge := 100
	r2, _ := a.readTextFile(acp.ReadTextFileRequest{Path: tmp, Line: &huge, Limit: &limit})
	if r2.Content != "" {
		t.Fatalf("expected empty got %q", r2.Content)
	}
	// Read failure.
	if _, err := a.readTextFile(acp.ReadTextFileRequest{Path: "/nonexistent/zzz"}); err == nil {
		t.Fatal()
	}
}

func TestWriteTextFile_BadDir(t *testing.T) {
	a := &AgentProc{}
	// Make the parent dir a file so MkdirAll fails.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "f", "child.txt")
	if err := a.writeTextFile(acp.WriteTextFileRequest{Path: target, Content: "x"}); err == nil {
		t.Fatal("expected mkdir error")
	}
}

func TestSinkFor(t *testing.T) {
	a := &AgentProc{sinks: map[acp.SessionId]SessionUpdateSink{"s1": noopSink{}}}
	if a.sinkFor("s1") == nil {
		t.Fatal()
	}
	if a.sinkFor("nope") != nil {
		t.Fatal()
	}
}

// startSleepProc and startTrapProc removed: tests now use exec.CommandContext directly.

package mcpattach

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// recordAttach is a boundAttach that records its last call.
type recordAttach struct {
	mu     sync.Mutex
	path   string
	name   string
	inline bool
	called bool
	conv   string
	err    error
}

func (r *recordAttach) fn(path, name string, inline bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.path, r.name, r.inline, r.called = path, name, inline, true
	return r.err
}

// attachFn is the 4-arg AttachFunc form for the Listener; it records conv.
func (r *recordAttach) attachFn(conv, path, name string, inline bool) error {
	r.mu.Lock()
	r.conv = conv
	r.mu.Unlock()
	return r.fn(path, name, inline)
}

// driveMCP runs RunMCP over canned input lines, returning decoded JSON-RPC
// responses (one per request line that warrants a reply). The suggest
// handler is a no-op; use driveMCPH to supply a custom Handlers.
func driveMCP(t *testing.T, attach func(path, name string, inline bool) error, lines ...string) []map[string]any {
	t.Helper()
	return driveMCPH(t, Handlers{Attach: attach, Suggest: func([]string) error { return nil }}, lines...)
}

func driveMCPH(t *testing.T, h Handlers, lines ...string) []map[string]any {
	t.Helper()
	in := strings.NewReader(strings.Join(lines, "\n") + "\n")
	var out bytes.Buffer
	if err := RunMCP(in, &out, h); err != nil {
		t.Fatalf("RunMCP: %v", err)
	}
	var resps []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("bad response line %q: %v", l, err)
		}
		resps = append(resps, m)
	}
	return resps
}

func noopAttach(string, string, bool) error { return nil }

// noopSuggestFn is the 2-arg SuggestFunc form for the Listener.
func noopSuggestFn(string, []string) error { return nil }

// shortSock returns a short unix socket path (macOS caps sun_path ~104
// chars, so t.TempDir's long names overflow). Cleaned up via t.Cleanup.
func shortSock(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "ma")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return filepath.Join(d, "s.sock")
}

// --- MCP state machine ---

func TestMCP_InitializeAndList(t *testing.T) {
	r := driveMCP(t, noopAttach,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	)
	if len(r) != 2 {
		t.Fatalf("want 2 responses, got %d: %v", len(r), r)
	}
	init := r[0]["result"].(map[string]any)
	if init["protocolVersion"] != "2025-06-18" {
		t.Errorf("protocolVersion = %v", init["protocolVersion"])
	}
	tools := r[1]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("want 2 tools, got %v", tools)
	}
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.(map[string]any)["name"].(string)] = true
	}
	if !names["attach"] || !names["suggest"] {
		t.Fatalf("tools missing attach/suggest: %v", tools)
	}
}

func TestMCP_ToolCall_Success(t *testing.T) {
	var ra recordAttach
	r := driveMCP(t, ra.fn,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"attach","arguments":{"path":"/tmp/x.md","name":"X","inline":true}}}`,
	)
	res := r[0]["result"].(map[string]any)
	if _, isErr := res["isError"]; isErr {
		t.Fatalf("tool returned error: %v", res)
	}
	if !ra.called || ra.path != "/tmp/x.md" || ra.name != "X" || !ra.inline {
		t.Fatalf("attach args wrong: called=%v path=%q name=%q inline=%v", ra.called, ra.path, ra.name, ra.inline)
	}
}

func TestMCP_ToolCall_SuccessEmptyName(t *testing.T) {
	r := driveMCP(t, noopAttach,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"attach","arguments":{"path":"/tmp/x.md"}}}`,
	)
	res := r[0]["result"].(map[string]any)
	txt := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(txt, "/tmp/x.md") {
		t.Fatalf("want path in confirmation, got %q", txt)
	}
}

func TestMCP_ToolCall_AttachError(t *testing.T) {
	ra := recordAttach{err: errors.New("boom")}
	r := driveMCP(t, ra.fn,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"attach","arguments":{"path":"/x"}}}`,
	)
	res := r[0]["result"].(map[string]any)
	if res["isError"] != true {
		t.Fatalf("want isError on attach failure: %v", res)
	}
}

func TestMCP_ToolCall_MissingPath(t *testing.T) {
	r := driveMCP(t, noopAttach,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"attach","arguments":{}}}`,
	)
	if r[0]["result"].(map[string]any)["isError"] != true {
		t.Fatal("want isError for missing path")
	}
}

func TestMCP_ToolCall_BadParams(t *testing.T) {
	r := driveMCP(t, noopAttach,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":123}}`,
	)
	if r[0]["result"].(map[string]any)["isError"] != true {
		t.Fatal("want isError on bad params")
	}
}

func TestMCP_ToolCall_UnknownTool(t *testing.T) {
	r := driveMCP(t, noopAttach,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"other","arguments":{"path":"/x"}}}`,
	)
	if r[0]["result"].(map[string]any)["isError"] != true {
		t.Fatal("want isError for unknown tool")
	}
}

func TestMCP_UnknownMethod(t *testing.T) {
	r := driveMCP(t, noopAttach, `{"jsonrpc":"2.0","id":9,"method":"bogus"}`)
	if r[0]["error"] == nil {
		t.Fatal("want error for unknown method")
	}
}

func TestMCP_NotificationPingBadJSON(t *testing.T) {
	r := driveMCP(t, noopAttach,
		`not json at all`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":3,"method":"ping"}`,
	)
	if len(r) != 1 {
		t.Fatalf("only ping should reply; got %d: %v", len(r), r)
	}
	if _, ok := r[0]["result"]; !ok {
		t.Fatalf("ping result missing: %v", r[0])
	}
}

// errReader returns a non-EOF error to exercise the read-error return.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read boom") }

func TestRunMCP_ReadError(t *testing.T) {
	var out strings.Builder
	if err := RunMCP(errReader{}, &out, Handlers{Attach: noopAttach, Suggest: func([]string) error { return nil }}); err == nil {
		t.Fatal("want read error")
	}
}

// failWriter errors on every Write to exercise the encode-error return.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("write boom") }

func TestRunMCP_WriteError(t *testing.T) {
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")
	if err := RunMCP(in, failWriter{}, Handlers{Attach: noopAttach, Suggest: func([]string) error { return nil }}); err == nil {
		t.Fatal("want write error propagated")
	}
}

func TestRunMCP_ReusesBufioReader(t *testing.T) {
	// A *bufio.Reader passed in is reused (not re-wrapped), so bytes
	// already buffered remain available.
	br := bufio.NewReader(strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n"))
	var out bytes.Buffer
	if err := RunMCP(br, &out, Handlers{Attach: noopAttach, Suggest: func([]string) error { return nil }}); err != nil {
		t.Fatalf("RunMCP: %v", err)
	}
	if !strings.Contains(out.String(), `"result"`) {
		t.Fatalf("want ping result, got %q", out.String())
	}
}

// --- Registry ---

func TestRegistry_RegisterResolve(t *testing.T) {
	reg := NewRegistry()
	tok := reg.Register("conv-1")
	if tok == "" {
		t.Fatal("empty token")
	}
	conv, ok := reg.Resolve(tok)
	if !ok || conv != "conv-1" {
		t.Fatalf("resolve = %q,%v", conv, ok)
	}
	if _, ok := reg.Resolve("nope"); ok {
		t.Fatal("unknown token resolved")
	}
	// fresh token each call
	if tok2 := reg.Register("conv-2"); tok2 == tok {
		t.Fatal("token not fresh")
	}
	// re-registering a conv rotates its token and drops the old one.
	tok1b := reg.Register("conv-1")
	if tok1b == tok {
		t.Fatal("re-register did not rotate token")
	}
	if _, ok := reg.Resolve(tok); ok {
		t.Fatal("old token still resolves after rotation")
	}
	if c, ok := reg.Resolve(tok1b); !ok || c != "conv-1" {
		t.Fatalf("rotated token resolve = %q,%v", c, ok)
	}
}

// --- Listener preamble auth + MCP integration ---

func dialPreamble(t *testing.T, sock, token string) net.Conn {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, err := c.Write([]byte(`{"token":"` + token + `"}` + "\n")); err != nil {
		t.Fatalf("write preamble: %v", err)
	}
	return c
}

func TestListener_ValidTokenFullMCP(t *testing.T) {
	sock := shortSock(t)
	reg := NewRegistry()
	tok := reg.Register("conv-7")
	var ra recordAttach
	l, err := Listen(sock, reg.Resolve, ra.attachFn, noopSuggestFn)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	if l.Path() != sock {
		t.Errorf("Path = %q", l.Path())
	}

	c := dialPreamble(t, sock, tok)
	defer c.Close()
	_, _ = c.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"attach","arguments":{"path":"/tmp/a","name":"A"}}}` + "\n"))
	br := bufio.NewReader(c)
	line, err := br.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	res := m["result"].(map[string]any)
	if _, isErr := res["isError"]; isErr {
		t.Fatalf("tool error: %v", res)
	}
	ra.mu.Lock()
	defer ra.mu.Unlock()
	if !ra.called || ra.path != "/tmp/a" || ra.name != "A" {
		t.Fatalf("attach not invoked correctly: called=%v conv=%q path=%q name=%q", ra.called, ra.conv, ra.path, ra.name)
	}
}

func TestListener_UnknownToken(t *testing.T) {
	sock := shortSock(t)
	reg := NewRegistry()
	l, _ := Listen(sock, reg.Resolve, func(string, string, string, bool) error {
		t.Fatal("attach must not run for unknown token")
		return nil
	}, noopSuggestFn)
	defer l.Close()
	c := dialPreamble(t, sock, "bogus")
	defer c.Close()
	// Server closes the conn without reading further; a read returns EOF.
	br := bufio.NewReader(c)
	if _, err := br.ReadBytes('\n'); err == nil {
		t.Fatal("want EOF after unknown-token rejection")
	}
}

func TestListener_MalformedPreamble(t *testing.T) {
	sock := shortSock(t)
	reg := NewRegistry()
	l, _ := Listen(sock, reg.Resolve, func(string, string, string, bool) error { return nil }, noopSuggestFn)
	defer l.Close()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte("not json\n"))
	br := bufio.NewReader(c)
	if _, err := br.ReadBytes('\n'); err == nil {
		t.Fatal("want EOF after malformed preamble")
	}
}

func TestListener_HangupBeforePreamble(t *testing.T) {
	sock := shortSock(t)
	reg := NewRegistry()
	l, _ := Listen(sock, reg.Resolve, func(string, string, string, bool) error { return nil }, noopSuggestFn)
	defer l.Close()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Half-close the write side so the server's ReadBytes sees EOF with an
	// empty line (the len(line)==0 hangup branch in handle). Then block on a
	// read until the server closes its end (io.EOF), which proves handle()
	// actually ran before the test returns and tears down the listener —
	// otherwise the accept→handle goroutine could race l.Close() and the
	// hangup branch would be flaky-uncovered.
	uc, ok := c.(*net.UnixConn)
	if !ok {
		t.Fatalf("want *net.UnixConn, got %T", c)
	}
	if err := uc.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	if _, err := io.Copy(io.Discard, c); err != nil {
		t.Fatalf("read until server close: %v", err)
	}
}

func TestListener_RemovesSocketOnClose(t *testing.T) {
	sock := shortSock(t)
	reg := NewRegistry()
	l, _ := Listen(sock, reg.Resolve, func(string, string, string, bool) error { return nil }, noopSuggestFn)
	if err := l.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("socket not removed: %v", err)
	}
}

func TestListen_BadPath(t *testing.T) {
	reg := NewRegistry()
	if _, err := Listen("/no/such/dir/x.sock", reg.Resolve, func(string, string, string, bool) error { return nil }, noopSuggestFn); err == nil {
		t.Fatal("want listen error on bad path")
	}
}

// --- Dumb redirector ---

func TestRedir_PreambleAndBidirectionalCopy(t *testing.T) {
	sock := shortSock(t)
	reg := NewRegistry()
	tok := reg.Register("conv-9")
	var ra recordAttach
	l, err := Listen(sock, reg.Resolve, ra.attachFn, noopSuggestFn)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// Drive a full attach through redirect: stdin carries one tools/call,
	// stdout receives the server's reply.
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"attach","arguments":{"path":"/redir/p"}}}` + "\n")
	var out bytes.Buffer
	if err := redirect(sock, tok, in, &out); err != nil {
		t.Fatalf("redirect: %v", err)
	}
	if !strings.Contains(out.String(), "Attached") {
		t.Fatalf("want server reply piped to stdout, got %q", out.String())
	}
	ra.mu.Lock()
	defer ra.mu.Unlock()
	if !ra.called || ra.path != "/redir/p" {
		t.Fatalf("attach not driven through redir: called=%v path=%q", ra.called, ra.path)
	}
}

func TestRedir_DialError(t *testing.T) {
	if err := redirect("/no/such/socket", "tok", strings.NewReader(""), &bytes.Buffer{}); err == nil {
		t.Fatal("want dial error")
	}
}

func TestPump_PreambleWriteError(t *testing.T) {
	a, b := net.Pipe()
	b.Close() // peer closed → write fails
	if err := pump(a, "tok", strings.NewReader("x"), &bytes.Buffer{}); err == nil {
		t.Fatal("want preamble write error")
	}
	a.Close()
}

func TestPump_NetPipeNoCloseWrite(t *testing.T) {
	// net.Pipe does not implement CloseWrite; exercise the false branch
	// of halfCloseWrite. The reader side echoes nothing and closes.
	a, b := net.Pipe()
	go func() {
		br := bufio.NewReader(b)
		_, _ = br.ReadBytes('\n') // consume preamble
		b.Close()
	}()
	// in never EOFs on its own; pump returns when conn→out completes.
	pr, pw := net.Pipe()
	defer pw.Close()
	if err := pump(a, "tok", pr, &bytes.Buffer{}); err != nil {
		t.Fatalf("pump: %v", err)
	}
	a.Close()
}

func TestRunFromEnv_MissingSocket(t *testing.T) {
	os.Unsetenv(EnvSocket)
	if err := RunFromEnv(); err == nil {
		t.Fatal("want error when socket env unset")
	}
}

func TestRunFromEnv_Happy(t *testing.T) {
	sock := shortSock(t)
	reg := NewRegistry()
	tok := reg.Register("c1")
	l, _ := Listen(sock, reg.Resolve, func(string, string, string, bool) error { return nil }, noopSuggestFn)
	defer l.Close()
	t.Setenv(EnvSocket, sock)
	t.Setenv(EnvToken, tok)
	pr, pw, _ := os.Pipe()
	oldStdin := os.Stdin
	os.Stdin = pr
	defer func() { os.Stdin = oldStdin }()
	pw.Close() // immediate EOF on stdin
	if err := RunFromEnv(); err != nil {
		t.Fatalf("RunFromEnv: %v", err)
	}
}

// --- suggest tool ---

func TestMCP_Suggest_Success(t *testing.T) {
	var got []string
	h := Handlers{Attach: noopAttach, Suggest: func(r []string) error { got = r; return nil }}
	r := driveMCPH(t, h,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"suggest","arguments":{"replies":["Yes","No"]}}}`,
	)
	res := r[0]["result"].(map[string]any)
	if _, isErr := res["isError"]; isErr {
		t.Fatalf("suggest returned error: %v", res)
	}
	if len(got) != 2 || got[0] != "Yes" || got[1] != "No" {
		t.Fatalf("suggest args = %v", got)
	}
}

func TestMCP_Suggest_Error(t *testing.T) {
	h := Handlers{Attach: noopAttach, Suggest: func([]string) error { return errors.New("boom") }}
	r := driveMCPH(t, h,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"suggest","arguments":{"replies":["x"]}}}`,
	)
	if r[0]["result"].(map[string]any)["isError"] != true {
		t.Fatal("want isError on suggest failure")
	}
}

func TestMCP_Suggest_BadParams(t *testing.T) {
	h := Handlers{Attach: noopAttach, Suggest: func([]string) error { return nil }}
	r := driveMCPH(t, h,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"suggest","arguments":{"replies":"notarray"}}}`,
	)
	if r[0]["result"].(map[string]any)["isError"] != true {
		t.Fatal("want isError on bad suggest params")
	}
}

func TestListener_SuggestThroughMCP(t *testing.T) {
	sock := shortSock(t)
	reg := NewRegistry()
	tok := reg.Register("conv-s")
	var mu sync.Mutex
	var gotConv string
	var gotReplies []string
	suggest := func(conv string, replies []string) error {
		mu.Lock()
		gotConv, gotReplies = conv, replies
		mu.Unlock()
		return nil
	}
	l, err := Listen(sock, reg.Resolve, func(string, string, string, bool) error { return nil }, suggest)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	c := dialPreamble(t, sock, tok)
	defer c.Close()
	_, _ = c.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"suggest","arguments":{"replies":["A","B"]}}}` + "\n"))
	br := bufio.NewReader(c)
	if _, err := br.ReadBytes('\n'); err != nil {
		t.Fatalf("read response: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotConv != "conv-s" || len(gotReplies) != 2 || gotReplies[0] != "A" {
		t.Fatalf("suggest not routed: conv=%q replies=%v", gotConv, gotReplies)
	}
}

func TestMCP_Attach_BadArgs(t *testing.T) {
	r := driveMCP(t, noopAttach,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"attach","arguments":{"path":123}}}`,
	)
	if r[0]["result"].(map[string]any)["isError"] != true {
		t.Fatal("want isError on bad attach args")
	}
}

package poemcp

import (
	"bufio"
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/kfet/acp-kit/mcphost"
)

type fakeCtrl struct {
	conv     string
	path     string
	name     string
	inline   bool
	replies  []string
	attachEr error
	suggestE error
}

func (f *fakeCtrl) AttachActive(conv, path, name string, inline bool) error {
	f.conv, f.path, f.name, f.inline = conv, path, name, inline
	return f.attachEr
}

func (f *fakeCtrl) SuggestActive(conv string, replies []string) error {
	f.conv, f.replies = conv, replies
	return f.suggestE
}

func TestConfigGetters(t *testing.T) {
	hc := HostConfig()
	if hc.ServerName != "poe" || hc.ServerInfoName != "poe-acp" || hc.SocketName != "mcp.sock" {
		t.Fatalf("HostConfig = %+v", hc)
	}
	if hc.EnvSocket != EnvSocket || hc.EnvToken != EnvToken || hc.RedirSubcommand != Subcommand {
		t.Fatalf("HostConfig env/sub = %+v", hc)
	}
	rc := RedirConfig()
	if rc.Subcommand != "mcp-serve" || len(rc.Aliases) != 1 || rc.Aliases[0] != "mcp-attach" {
		t.Fatalf("RedirConfig = %+v", rc)
	}
}

// liveHost registers the tools against a real host bound to a socket and
// returns the host + a fresh token for conv "c".
func liveHost(t *testing.T, ctrl Controller) (*mcphost.Host, string) {
	t.Helper()
	cfg := HostConfig()
	cfg.BaseDir = "/tmp"
	cfg.RedirCommand = "/bin/true"
	h, err := mcphost.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { h.Close() })
	Register(h, ctrl)
	tok := ""
	for _, e := range h.ServerConfigForSession("c")[0].Stdio.Env {
		if e.Name == EnvToken {
			tok = e.Value
		}
	}
	if err := h.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	return h, tok
}

// call drives one tools/call over the socket and returns the decoded
// result object.
func call(t *testing.T, h *mcphost.Host, tok, line string) map[string]any {
	t.Helper()
	c, err := net.Dial("unix", h.SocketPath())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if _, err := c.Write([]byte(`{"token":"` + tok + `"}` + "\n")); err != nil {
		t.Fatalf("preamble: %v", err)
	}
	if _, err := c.Write([]byte(line + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	br := bufio.NewReader(c)
	respLine, err := br.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(respLine, &m); err != nil {
		t.Fatalf("decode %q: %v", respLine, err)
	}
	return m["result"].(map[string]any)
}

func isErr(res map[string]any) bool {
	_, ok := res["isError"]
	return ok
}

func text(res map[string]any) string {
	return res["content"].([]any)[0].(map[string]any)["text"].(string)
}

func TestAttach_Success(t *testing.T) {
	ctrl := &fakeCtrl{}
	h, tok := liveHost(t, ctrl)
	res := call(t, h, tok, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"attach","arguments":{"path":"/tmp/x","name":"X","inline":true}}}`)
	if isErr(res) {
		t.Fatalf("unexpected error: %v", res)
	}
	if text(res) != "Attached X to the chat." {
		t.Fatalf("text = %q", text(res))
	}
	if ctrl.conv != "c" || ctrl.path != "/tmp/x" || ctrl.name != "X" || !ctrl.inline {
		t.Fatalf("ctrl = %+v", ctrl)
	}
}

func TestAttach_SuccessEmptyName(t *testing.T) {
	h, tok := liveHost(t, &fakeCtrl{})
	res := call(t, h, tok, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"attach","arguments":{"path":"/tmp/x"}}}`)
	if !strings.Contains(text(res), "/tmp/x") {
		t.Fatalf("want path in confirmation, got %q", text(res))
	}
}

func TestAttach_MissingPath(t *testing.T) {
	h, tok := liveHost(t, &fakeCtrl{})
	res := call(t, h, tok, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"attach","arguments":{}}}`)
	if !isErr(res) || text(res) != "path is required" {
		t.Fatalf("res = %v", res)
	}
}

func TestAttach_BadArgs(t *testing.T) {
	h, tok := liveHost(t, &fakeCtrl{})
	res := call(t, h, tok, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"attach","arguments":{"path":123}}}`)
	if !isErr(res) || !strings.HasPrefix(text(res), "invalid params:") {
		t.Fatalf("res = %v", res)
	}
}

func TestAttach_Error(t *testing.T) {
	h, tok := liveHost(t, &fakeCtrl{attachEr: errString("boom")})
	res := call(t, h, tok, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"attach","arguments":{"path":"/x"}}}`)
	if !isErr(res) || !strings.HasPrefix(text(res), "attach failed:") {
		t.Fatalf("res = %v", res)
	}
}

func TestSuggest_Success(t *testing.T) {
	ctrl := &fakeCtrl{}
	h, tok := liveHost(t, ctrl)
	res := call(t, h, tok, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"suggest","arguments":{"replies":["Yes","No"]}}}`)
	if isErr(res) {
		t.Fatalf("unexpected error: %v", res)
	}
	if text(res) != "Suggested replies posted." {
		t.Fatalf("text = %q", text(res))
	}
	if len(ctrl.replies) != 2 || ctrl.replies[0] != "Yes" {
		t.Fatalf("replies = %v", ctrl.replies)
	}
}

func TestSuggest_BadArgs(t *testing.T) {
	h, tok := liveHost(t, &fakeCtrl{})
	res := call(t, h, tok, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"suggest","arguments":{"replies":"nope"}}}`)
	if !isErr(res) || !strings.HasPrefix(text(res), "invalid params:") {
		t.Fatalf("res = %v", res)
	}
}

func TestSuggest_Error(t *testing.T) {
	h, tok := liveHost(t, &fakeCtrl{suggestE: errString("boom")})
	res := call(t, h, tok, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"suggest","arguments":{"replies":["x"]}}}`)
	if !isErr(res) || !strings.HasPrefix(text(res), "suggest failed:") {
		t.Fatalf("res = %v", res)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

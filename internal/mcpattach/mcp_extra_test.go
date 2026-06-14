package mcpattach

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestListener_CloseAndBadRequest(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "s.sock")
	l, err := Listen(sock, "tok", func(SocketRequest) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if l.Path() != sock {
		t.Errorf("Path = %q", l.Path())
	}
	// Non-JSON request → "bad request".
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.Write([]byte("not json\n"))
	var resp SocketResponse
	_ = json.NewDecoder(c).Decode(&resp)
	c.Close()
	if resp.OK || resp.Error != "bad request" {
		t.Errorf("bad-request resp = %+v", resp)
	}
	if err := l.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// socket file removed
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Errorf("socket not removed: %v", err)
	}
}

func TestListener_HandlerError(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "s.sock")
	l, _ := Listen(sock, "tok", func(SocketRequest) error { return errors.New("boom") })
	defer l.Close()
	c, _ := net.Dial("unix", sock)
	_ = json.NewEncoder(c).Encode(SocketRequest{Token: "tok", Conv: "c", Path: "/p"})
	var resp SocketResponse
	_ = json.NewDecoder(c).Decode(&resp)
	c.Close()
	if resp.OK || resp.Error != "boom" {
		t.Errorf("handler-error resp = %+v", resp)
	}
}

func TestListen_BadPath(t *testing.T) {
	if _, err := Listen("/no/such/dir/x.sock", "t", func(SocketRequest) error { return nil }); err == nil {
		t.Fatal("want listen error on bad path")
	}
}

// errReader returns a non-EOF error to exercise the read-error return.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read boom") }

func TestRunStdioServer_ReadError(t *testing.T) {
	var out strings.Builder
	if err := RunStdioServer(errReader{}, &out, "/no", "c", "t"); err == nil {
		t.Fatal("want read error")
	}
}

func TestStdio_NotificationAndPingAndBadJSON(t *testing.T) {
	r := drive(t, "/no", "c", "t",
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

func TestStdio_ToolCall_RelayDialFails(t *testing.T) {
	r := drive(t, "/nonexistent/socket", "c", "t",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"attach","arguments":{"path":"/x"}}}`,
	)
	res := r[0]["result"].(map[string]any)
	if res["isError"] != true {
		t.Fatalf("want isError on dial failure: %v", res)
	}
}

func TestStdio_ToolCall_BadParams(t *testing.T) {
	r := drive(t, "/no", "c", "t",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":123}}`,
	)
	if r[0]["result"].(map[string]any)["isError"] != true {
		t.Fatal("want isError on bad params")
	}
}

func TestStdio_ToolCall_UnknownTool(t *testing.T) {
	r := drive(t, "/no", "c", "t",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"other","arguments":{"path":"/x"}}}`,
	)
	if r[0]["result"].(map[string]any)["isError"] != true {
		t.Fatal("want isError for unknown tool")
	}
}

func TestRunFromEnv(t *testing.T) {
	// missing socket env → error
	os.Unsetenv(EnvSocket)
	if err := RunFromEnv(); err == nil {
		t.Fatal("want error when socket env unset")
	}
	// happy: set env, feed EOF on stdin via a pipe
	dir := t.TempDir()
	sock := filepath.Join(dir, "s.sock")
	l, _ := Listen(sock, "tok", func(SocketRequest) error { return nil })
	defer l.Close()
	t.Setenv(EnvSocket, sock)
	t.Setenv(EnvToken, "tok")
	t.Setenv(EnvConv, "c1")
	pr, pw, _ := os.Pipe()
	oldStdin := os.Stdin
	os.Stdin = pr
	defer func() { os.Stdin = oldStdin }()
	pw.Close() // immediate EOF
	if err := RunFromEnv(); err != nil {
		t.Fatalf("RunFromEnv: %v", err)
	}
}

func TestMCP_ToolCall_SuccessEmptyName(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "s.sock")
	l, err := Listen(sock, "tok", func(SocketRequest) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	r := drive(t, sock, "c", "tok",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"attach","arguments":{"path":"/tmp/x.md"}}}`,
	)
	res := r[0]["result"].(map[string]any)
	if _, isErr := res["isError"]; isErr {
		t.Fatalf("unexpected error: %v", res)
	}
	txt := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(txt, "/tmp/x.md") {
		t.Fatalf("want path in confirmation (empty-name fallback), got %q", txt)
	}
}

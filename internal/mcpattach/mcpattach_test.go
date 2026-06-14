package mcpattach

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// drive runs the stdio server over canned input lines and returns the
// decoded JSON-RPC responses (one per request line).
func drive(t *testing.T, socket, conv, token string, lines ...string) []map[string]any {
	t.Helper()
	in := strings.NewReader(strings.Join(lines, "\n") + "\n")
	var out bytes.Buffer
	if err := RunStdioServer(in, &out, socket, conv, token); err != nil {
		t.Fatalf("server: %v", err)
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

func TestMCP_InitializeAndList(t *testing.T) {
	r := drive(t, "/no/socket", "c1", "tok",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	)
	if len(r) != 2 {
		t.Fatalf("want 2 responses (notification has none), got %d: %v", len(r), r)
	}
	init := r[0]["result"].(map[string]any)
	if init["protocolVersion"] != "2025-06-18" {
		t.Errorf("protocolVersion = %v", init["protocolVersion"])
	}
	tools := r[1]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "attach" {
		t.Fatalf("tools = %v", tools)
	}
}

func TestMCP_ToolCall_RelaysToListener(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "s.sock")
	var mu sync.Mutex
	var got SocketRequest
	l, err := Listen(sock, "tok", func(req SocketRequest) error {
		mu.Lock()
		got = req
		mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()

	r := drive(t, sock, "conv-7", "tok",
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"attach","arguments":{"path":"/tmp/x.md","name":"X","inline":false}}}`,
	)
	if len(r) != 1 {
		t.Fatalf("want 1 response, got %v", r)
	}
	res := r[0]["result"].(map[string]any)
	if _, isErr := res["isError"]; isErr {
		t.Fatalf("tool returned error: %v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if got.Conv != "conv-7" || got.Path != "/tmp/x.md" || got.Name != "X" || got.Token != "tok" {
		t.Fatalf("relayed req wrong: %+v", got)
	}
}

func TestMCP_ToolCall_BadToken(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "s.sock")
	l, _ := Listen(sock, "right", func(SocketRequest) error { return nil })
	defer l.Close()
	r := drive(t, sock, "c", "wrong",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"attach","arguments":{"path":"/a"}}}`,
	)
	res := r[0]["result"].(map[string]any)
	if res["isError"] != true {
		t.Fatalf("want isError for bad token, got %v", res)
	}
}

func TestMCP_ToolCall_MissingPath(t *testing.T) {
	r := drive(t, "/no", "c", "t",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"attach","arguments":{}}}`,
	)
	if r[0]["result"].(map[string]any)["isError"] != true {
		t.Fatal("want isError for missing path")
	}
}

func TestMCP_UnknownMethod(t *testing.T) {
	r := drive(t, "/no", "c", "t", `{"jsonrpc":"2.0","id":9,"method":"bogus"}`)
	if r[0]["error"] == nil {
		t.Fatal("want error for unknown method")
	}
}

package mcpattach

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

// rpcMessage is a minimal JSON-RPC 2.0 envelope (request, response, or
// notification — distinguished by which fields are set).
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // absent => notification
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const mcpProtocolVersion = "2025-06-18"

// RunStdioServer runs the MCP server loop over r/w (stdin/stdout in
// production), relaying attach calls to socketPath. conv/token identify
// and authenticate this session. Returns when r hits EOF.
func RunStdioServer(r io.Reader, w io.Writer, socketPath, conv, token string) error {
	br := bufio.NewReaderSize(r, 1<<20)
	enc := json.NewEncoder(w)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if rerr := handleLine(line, enc, socketPath, conv, token); rerr != nil {
				return rerr
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func handleLine(line []byte, enc *json.Encoder, socketPath, conv, token string) error {
	var msg rpcMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		return nil // ignore non-JSON / blank lines
	}
	if msg.Method == "" || len(msg.ID) == 0 {
		return nil // notification or response; nothing to reply to
	}
	resp := rpcMessage{JSONRPC: "2.0", ID: msg.ID}
	switch msg.Method {
	case "initialize":
		resp.Result = map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "poe-acp-attach", "version": "1"},
		}
	case "tools/list":
		resp.Result = map[string]any{"tools": []any{toolSpec()}}
	case "tools/call":
		resp.Result = handleToolCall(msg.Params, socketPath, conv, token)
	case "ping":
		resp.Result = map[string]any{}
	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + msg.Method}
	}
	return enc.Encode(&resp)
}

func toolSpec() map[string]any {
	return map[string]any{
		"name": ToolName,
		"description": "Deliver a file from this host to the user as a chat attachment " +
			"(download chip, or inline image). Use instead of pasting large content or links.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":   map[string]any{"type": "string", "description": "Path to the file on this host (absolute, or relative to your working dir)."},
				"name":   map[string]any{"type": "string", "description": "Optional display name for the attachment."},
				"inline": map[string]any{"type": "boolean", "description": "If true, render an image inline instead of a download chip."},
			},
			"required": []string{"path"},
		},
	}
}

func handleToolCall(params json.RawMessage, socketPath, conv, token string) map[string]any {
	var p struct {
		Name      string `json:"name"`
		Arguments struct {
			Path   string `json:"path"`
			Name   string `json:"name"`
			Inline bool   `json:"inline"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return toolError("invalid params: " + err.Error())
	}
	if p.Name != ToolName {
		return toolError("unknown tool: " + p.Name)
	}
	if p.Arguments.Path == "" {
		return toolError("path is required")
	}
	if err := relay(socketPath, SocketRequest{
		Token: token, Conv: conv,
		Path: p.Arguments.Path, Name: p.Arguments.Name, Inline: p.Arguments.Inline,
	}); err != nil {
		return toolError("attach failed: " + err.Error())
	}
	disp := p.Arguments.Name
	if disp == "" {
		disp = p.Arguments.Path
	}
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": "Attached " + disp + " to the chat."}},
	}
}

func toolError(msg string) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": msg}},
		"isError": true,
	}
}

// relay sends one attach request to the main process and waits for ack.
func relay(socketPath string, req SocketRequest) error {
	c, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		return fmt.Errorf("dial relay: %w", err)
	}
	defer c.Close()
	return exchange(c, req)
}

// exchange runs one request/response over an already-dialed conn. Split
// from relay so the send/recv error paths are testable via net.Pipe.
func exchange(c net.Conn, req SocketRequest) error {
	_ = c.SetDeadline(time.Now().Add(3 * time.Minute))
	if err := json.NewEncoder(c).Encode(&req); err != nil {
		return fmt.Errorf("send: %w", err)
	}
	var resp SocketResponse
	if err := json.NewDecoder(c).Decode(&resp); err != nil {
		return fmt.Errorf("recv: %w", err)
	}
	if !resp.OK {
		return fmt.Errorf("%s", resp.Error)
	}
	return nil
}

// RunFromEnv is the entry point for the `mcp-attach` subcommand: reads
// config from the env the parent set and serves on stdin/stdout.
func RunFromEnv() error {
	socket := os.Getenv(EnvSocket)
	conv := os.Getenv(EnvConv)
	token := os.Getenv(EnvToken)
	if socket == "" {
		return fmt.Errorf("mcp-attach: %s not set", EnvSocket)
	}
	return RunStdioServer(os.Stdin, os.Stdout, socket, conv, token)
}

package mcpattach

import (
	"bufio"
	"encoding/json"
	"io"
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

// boundAttach is an AttachFunc with its conversation already bound (from
// the connection's preamble token), so the MCP loop only supplies the
// per-call path/name/inline.
type boundAttach func(path, name string, inline bool) error

// RunMCP runs the MCP server loop over r/w until r hits EOF. attach is
// invoked in-process for each tools/call. If r is already a *bufio.Reader
// it is reused so no bytes buffered ahead (e.g. after a preamble read)
// are lost.
func RunMCP(r io.Reader, w io.Writer, attach boundAttach) error {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReaderSize(r, 1<<20)
	}
	enc := json.NewEncoder(w)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if rerr := handleLine(line, enc, attach); rerr != nil {
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

func handleLine(line []byte, enc *json.Encoder, attach boundAttach) error {
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
		resp.Result = handleToolCall(msg.Params, attach)
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

func handleToolCall(params json.RawMessage, attach boundAttach) map[string]any {
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
	if err := attach(p.Arguments.Path, p.Arguments.Name, p.Arguments.Inline); err != nil {
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

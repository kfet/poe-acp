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

// Handlers carries the per-connection tool implementations, each with
// its conversation already bound (from the connection's preamble token),
// so the MCP loop only supplies the per-call arguments. Both fields must
// be non-nil.
type Handlers struct {
	// Attach delivers a host file to the user as a Poe attachment.
	Attach func(path, name string, inline bool) error
	// Suggest posts tappable follow-up reply chips on the live turn.
	Suggest func(replies []string) error
}

// RunMCP runs the MCP server loop over r/w until r hits EOF. The handler
// funcs in h are invoked in-process for each tools/call. If r is already
// a *bufio.Reader it is reused so no bytes buffered ahead (e.g. after a
// preamble read) are lost.
func RunMCP(r io.Reader, w io.Writer, h Handlers) error {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReaderSize(r, 1<<20)
	}
	enc := json.NewEncoder(w)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if rerr := handleLine(line, enc, h); rerr != nil {
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

func handleLine(line []byte, enc *json.Encoder, h Handlers) error {
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
			"serverInfo":      map[string]any{"name": "poe-acp", "version": "1"},
		}
	case "tools/list":
		resp.Result = map[string]any{"tools": toolSpecs()}
	case "tools/call":
		resp.Result = handleToolCall(msg.Params, h)
	case "ping":
		resp.Result = map[string]any{}
	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + msg.Method}
	}
	return enc.Encode(&resp)
}

// toolSpecs returns the JSON-RPC tool specs advertised to the agent.
func toolSpecs() []any {
	return []any{attachSpec(), suggestSpec()}
}

func attachSpec() map[string]any {
	return map[string]any{
		"name": ToolAttach,
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

func suggestSpec() map[string]any {
	return map[string]any{
		"name": ToolSuggest,
		"description": "Offer the user 2-4 tappable follow-up reply chips at a genuine " +
			"decision point (e.g. Yes / No / Tell me more). Each reply is the exact text " +
			"sent as the user's next message if tapped, so keep them short (a few words). " +
			"Call at most once per turn, right before you finish your reply. Omit when there " +
			"is no natural next choice.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"replies": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "2-4 short reply options, each shown as a chip.",
				},
			},
			"required": []string{"replies"},
		},
	}
}

func handleToolCall(params json.RawMessage, h Handlers) map[string]any {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return toolError("invalid params: " + err.Error())
	}
	switch p.Name {
	case ToolAttach:
		return handleAttach(p.Arguments, h.Attach)
	case ToolSuggest:
		return handleSuggest(p.Arguments, h.Suggest)
	default:
		return toolError("unknown tool: " + p.Name)
	}
}

func handleAttach(args json.RawMessage, attach func(path, name string, inline bool) error) map[string]any {
	var a struct {
		Path   string `json:"path"`
		Name   string `json:"name"`
		Inline bool   `json:"inline"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return toolError("invalid params: " + err.Error())
	}
	if a.Path == "" {
		return toolError("path is required")
	}
	if err := attach(a.Path, a.Name, a.Inline); err != nil {
		return toolError("attach failed: " + err.Error())
	}
	disp := a.Name
	if disp == "" {
		disp = a.Path
	}
	return okText("Attached " + disp + " to the chat.")
}

func handleSuggest(args json.RawMessage, suggest func(replies []string) error) map[string]any {
	var a struct {
		Replies []string `json:"replies"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return toolError("invalid params: " + err.Error())
	}
	if err := suggest(a.Replies); err != nil {
		return toolError("suggest failed: " + err.Error())
	}
	return okText("Suggested replies posted.")
}

func okText(text string) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
	}
}

func toolError(msg string) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": msg}},
		"isError": true,
	}
}

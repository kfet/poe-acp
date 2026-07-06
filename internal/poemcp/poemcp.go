// Package poemcp wires poe-acp's self-hosted MCP tools (attach, suggest)
// onto the generic acp-kit/mcphost Host. It owns everything Poe-specific:
// the `poe` server identity, tool names/descriptions/schemas, the env var
// names and redirector subcommand, and the glue from each tool call to
// the router. The transport, socket ownership, token auth, and MCP loop
// live in acp-kit/mcphost.
package poemcp

import (
	"encoding/json"
	"errors"

	"github.com/kfet/acp-kit/mcphost"
)

// Tool names exposed to the agent by the self-hosted `poe` MCP server.
const (
	ToolAttach  = "attach"
	ToolSuggest = "suggest"
)

// Env var names the main process sets on the spawned redirector (via the
// ACP McpServerStdio.Env), so no secrets land on the command line. The
// conversation id is intentionally NOT passed: it is derived server-side
// from the token by mcphost.
const (
	EnvToken  = "POEACP_MCP_TOKEN"
	EnvSocket = "POEACP_MCP_SOCKET"
)

// Redirector subcommand (with a deprecated legacy alias) and the server
// identity advertised to the agent. The socket file name and serverInfo
// name are kept byte-identical to the pre-extraction implementation.
const (
	Subcommand     = "mcp-serve"
	SubcommandAlt  = "mcp-attach"
	ServerName     = "poe"
	ServerInfoName = "poe-acp"
	SocketName     = "mcp.sock"
	DirPrefix      = "poe-acp-mcp-"
)

// Controller is the subset of the router the tools drive. conv is the
// session key resolved server-side from the connection token.
type Controller interface {
	AttachActive(conv, path, name string, inline bool) error
	SuggestActive(conv string, replies []string) error
}

// HostConfig returns the mcphost.Config for poe-acp's `poe` server.
func HostConfig() mcphost.Config {
	return mcphost.Config{
		DirPrefix:         DirPrefix,
		SocketName:        SocketName,
		RedirSubcommand:   Subcommand,
		ServerName:        ServerName,
		ServerInfoName:    ServerInfoName,
		ServerInfoVersion: "1",
		EnvSocket:         EnvSocket,
		EnvToken:          EnvToken,
	}
}

// RedirConfig returns the mcphost.RedirConfig for the redirector
// subcommand interception in main.
func RedirConfig() mcphost.RedirConfig {
	return mcphost.RedirConfig{
		Subcommand: Subcommand,
		Aliases:    []string{SubcommandAlt},
		EnvSocket:  EnvSocket,
		EnvToken:   EnvToken,
	}
}

// Register registers the attach and suggest tools on h, wiring them to
// ctrl. Tool descriptions and schemas match the pre-extraction wire
// contract exactly.
func Register(h *mcphost.Host, ctrl Controller) {
	h.Tool(ToolAttach,
		"Deliver a file from this host to the user as a chat attachment "+
			"(download chip, or inline image). Use instead of pasting large content or links.",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":   map[string]any{"type": "string", "description": "Path to the file on this host (absolute, or relative to your working dir)."},
				"name":   map[string]any{"type": "string", "description": "Optional display name for the attachment."},
				"inline": map[string]any{"type": "boolean", "description": "If true, render an image inline instead of a download chip."},
			},
			"required": []string{"path"},
		},
		func(conv string, args json.RawMessage) (string, error) {
			var a struct {
				Path   string `json:"path"`
				Name   string `json:"name"`
				Inline bool   `json:"inline"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", errors.New("invalid params: " + err.Error())
			}
			if a.Path == "" {
				return "", errors.New("path is required")
			}
			if err := ctrl.AttachActive(conv, a.Path, a.Name, a.Inline); err != nil {
				return "", errors.New("attach failed: " + err.Error())
			}
			disp := a.Name
			if disp == "" {
				disp = a.Path
			}
			return "Attached " + disp + " to the chat.", nil
		},
	)

	h.Tool(ToolSuggest,
		"Offer the user 2-4 tappable follow-up reply chips at a genuine "+
			"decision point (e.g. Yes / No / Tell me more). Each reply is the exact text "+
			"sent as the user's next message if tapped, so keep them short (a few words). "+
			"Call at most once per turn, right before you finish your reply. Omit when there "+
			"is no natural next choice.",
		map[string]any{
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
		func(conv string, args json.RawMessage) (string, error) {
			var a struct {
				Replies []string `json:"replies"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", errors.New("invalid params: " + err.Error())
			}
			if err := ctrl.SuggestActive(conv, a.Replies); err != nil {
				return "", errors.New("suggest failed: " + err.Error())
			}
			return "Suggested replies posted.", nil
		},
	)
}

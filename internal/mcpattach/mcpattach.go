// Package mcpattach implements a tiny, self-contained MCP server that
// exposes a single `attach` tool to the ACP agent, plus the unix-socket
// relay back to the main poe-acp process that performs the actual Poe
// upload + `file` SSE event.
//
// Transport choices (deliberately minimal, stdlib-only, no MCP SDK):
//   - The agent (fir) spawns `poe-acp mcp-attach` over MCP **stdio**
//     (newline-delimited JSON-RPC 2.0). Stdio needs no port and no
//     capability negotiation, so it works on every deployment.
//   - That subprocess relays an attach request to the main process over
//     a **unix socket** (newline-delimited JSON), authenticated by a
//     per-process token. No network, no extra deps.
package mcpattach

// SocketRequest is one attach relayed from the mcp-attach subprocess to
// the main poe-acp process over the unix socket.
type SocketRequest struct {
	Token  string `json:"token"`  // per-process shared secret
	Conv   string `json:"conv"`   // conversation id this attach belongs to
	Path   string `json:"path"`   // file path on this host
	Name   string `json:"name"`   // display name (optional)
	Inline bool   `json:"inline"` // render inline image vs download chip
}

// SocketResponse is the main process's reply.
type SocketResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// ToolName is the single MCP tool exposed to the agent.
const ToolName = "attach"

// EnvToken / EnvSocket / EnvConv are the env vars the main process sets
// on the spawned subprocess (via the ACP McpServerStdio.Env), so no
// secrets land on the command line.
const (
	EnvToken  = "POEACP_MCP_TOKEN"
	EnvSocket = "POEACP_MCP_SOCKET"
	EnvConv   = "POEACP_MCP_CONV"
)

// Package mcpattach implements a self-hosted MCP server that exposes a
// single `attach` tool to the ACP agent, plus the unix-socket transport
// back to the main poe-acp process that performs the actual Poe upload +
// `file` SSE event.
//
// Transport design (deliberately minimal, stdlib-only, no MCP SDK):
//   - The agent (fir) spawns `poe-acp mcp-attach` over MCP **stdio**
//     (newline-delimited JSON-RPC 2.0). Stdio needs no port and no
//     capability negotiation, so it works on every deployment.
//   - That subprocess is a **dumb redirector**: it dials the main
//     process's unix socket, writes one newline-terminated preamble line
//     `{"token":"<tok>"}`, then io.Copy's stdin↔socket in both
//     directions. It has no MCP knowledge.
//   - The main process owns the MCP state machine: it runs the loop once
//     per accepted socket connection, after the preamble token is
//     resolved to a conversation id. The conv is bound server-side from
//     the token, so a client can never spoof which conversation it
//     attaches to.
//
// Security: each ACP session is minted a fresh random token bound to its
// conversation in a Registry. Only a same-uid process holding that token
// (delivered via the spawned subprocess's env) can attach, and only to
// the one conversation the token maps to.
package mcpattach

import (
	"encoding/hex"
	"sync"
)

// ToolName is the single MCP tool exposed to the agent.
const ToolName = "attach"

// EnvToken / EnvSocket are the env vars the main process sets on the
// spawned subprocess (via the ACP McpServerStdio.Env), so no secrets
// land on the command line. The conversation id is intentionally NOT
// passed: it is derived server-side from the token.
const (
	EnvToken  = "POEACP_MCP_TOKEN"
	EnvSocket = "POEACP_MCP_SOCKET"
)

// AttachFunc performs the actual delivery for one validated tool call.
// conv is resolved server-side from the connection's preamble token, so
// it cannot be spoofed by the client. Returns an error to surface back
// to the agent's tool call.
type AttachFunc func(conv, path, name string, inline bool) error

// Registry maps per-session tokens to conversation ids. The main process
// mints a fresh token for each ACP session (one per MCPServersForSession
// call) and resolves the token when a socket connection presents it.
//
// Each conversation holds at most one live token: re-registering a conv
// rotates its token and drops the previous one, so the map is bounded by
// the number of distinct conversations rather than total sessions ever.
type Registry struct {
	mu      sync.Mutex
	byToken map[string]string // token -> conv
	byConv  map[string]string // conv  -> current token
}

// NewRegistry returns an empty token→conv registry.
func NewRegistry() *Registry {
	return &Registry{
		byToken: make(map[string]string),
		byConv:  make(map[string]string),
	}
}

// Register mints a fresh random token bound to conv and returns it. Any
// previous token for the same conv is invalidated.
func (r *Registry) Register(conv string) string {
	tok := newToken()
	r.mu.Lock()
	if old, ok := r.byConv[conv]; ok {
		delete(r.byToken, old) // rotate: drop the conv's previous token
	}
	r.byToken[tok] = conv
	r.byConv[conv] = tok
	r.mu.Unlock()
	return tok
}

// Resolve returns the conversation bound to token, if any.
func (r *Registry) Resolve(token string) (conv string, ok bool) {
	r.mu.Lock()
	conv, ok = r.byToken[token]
	r.mu.Unlock()
	return conv, ok
}

// newToken returns a 16-byte random hex token. crypto/rand failure is
// fatal-grade and handled in mcpattach_must.go.
func newToken() string {
	b := make([]byte, 16)
	mustRand(b)
	return hex.EncodeToString(b)
}

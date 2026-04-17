// Package poeacp contains the Poe ↔ ACP relay.
//
// See docs/poe-acp-relay/DESIGN.md for the full design. This package is
// the skeleton; sub-packages implement each slice:
//
//   - poeproto  — Poe server-bot protocol (HTTP + SSE)
//   - acpclient — acp.Client implementation + stdio agent process wrapper
//   - router    — conversation_id → ACP session mapping
//   - policy    — permission policy for session/request_permission
//
// The cmd/poe-acp-relay binary wires them together.
package poeacp

// Version of the relay binary. Overridden at link time via -ldflags.
var Version = "0.0.0-dev"

// Package agentcfg assembles the acp-kit client.Config used to spawn the
// ACP agent, declaring poe-acp's own inbound secrets so acp-kit scrubs
// them from the agent's environment before the process ever starts.
//
// This lives in internal/ rather than in cmd/poe-acp/main.go on purpose.
// main.go is excluded from the coverage gate by .covignore (entry-point
// shims are bare assembly), so security wiring placed there is
// unprotected: deleting the secret declarations would silently restore
// full-environment inheritance — handing the agent the relay's bearer
// secret and admin token — with every test still green. Assembling the
// config here keeps the wiring under the 100% gate.
package agentcfg

import "github.com/kfet/acp-kit/client"

// AdminTokenEnv is the environment variable holding the /admin/reexec
// bearer token. It authenticates operator-driven worker swaps; the
// spawned agent, driven by text from arbitrary Poe users, must never see
// it.
const AdminTokenEnv = "ADMIN_TOKEN"

// Params carries the plain spawn parameters plus poe-acp's own inbound
// secrets that must never reach the spawned agent.
type Params struct {
	// Command is the argv of the ACP agent to spawn.
	Command []string
	// Cwd is the agent process working directory (per-session cwd is
	// passed separately per NewSession).
	Cwd string
	// Env is the environment handed to the agent before scrubbing. Pass
	// os.Environ() (optionally with FIR_AGENT_DIR appended). acp-kit
	// drops the declared secrets from it, in both the provided-Env and
	// nil-Env (inherit) paths.
	Env []string
	// ClientMeta carries extra clientCapabilities._meta entries (e.g. the
	// status-line extension advertisement).
	ClientMeta map[string]any

	// AccessKeyEnv is the RESOLVED env var name holding the Poe bearer
	// secret (the value of the --access-key-env flag, not the hardcoded
	// default). Dropped from the agent env by name.
	AccessKeyEnv string
	// AccessKey is the Poe bearer secret's literal value. Dropped by
	// value too, so a copy exported under a bespoke name is also caught.
	AccessKey string
	// AdminToken is the ADMIN_TOKEN value (may be empty). Dropped by
	// value; the ADMIN_TOKEN name is always dropped regardless.
	AdminToken string
}

// New assembles the client.Config, declaring the relay's own secrets on
// SecretEnvNames (by variable name) and Secrets (by literal value).
//
// Only poe-acp's own inbound secrets are declared: the resolved
// access-key env var and ADMIN_TOKEN. Provider credentials the agent
// legitimately needs — ANTHROPIC_API_KEY, OPENAI_API_KEY, POE_API_KEY
// (fir's poe-provider key, a DIFFERENT variable from the relay's bearer
// secret) — are never declared and therefore pass through untouched.
func New(p Params) client.Config {
	return client.Config{
		Command:        p.Command,
		Cwd:            p.Cwd,
		Env:            p.Env,
		ClientMeta:     p.ClientMeta,
		SecretEnvNames: []string{p.AccessKeyEnv, AdminTokenEnv},
		Secrets:        []string{p.AccessKey, p.AdminToken},
	}
}

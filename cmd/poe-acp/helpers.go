// Helpers extracted from main.go so they can be unit-tested in isolation.
// main.go retains only the entry point shim.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
	"github.com/kfet/acp-kit/mcphost"
	"github.com/kfet/acp-kit/remotefs"
	kitsysprompt "github.com/kfet/acp-kit/sysprompt"
	"github.com/kfet/poe-acp/internal/agentpool"
	"github.com/kfet/poe-acp/internal/config"
	"github.com/kfet/poe-acp/internal/paramctl"
	"github.com/kfet/poe-acp/internal/poeproto"
	"github.com/kfet/poe-acp/internal/router"
	"github.com/kfet/poe-acp/internal/skills"
)

// httpClient is overridable in tests so maybeRefetchSettings can be
// driven against an httptest.Server without reaching real Poe.
var httpClient = http.DefaultClient

// buildControls is the SINGLE place the served parameter_controls
// schema is assembled: operator pins are applied, then the schema is
// built. Both the live ParameterControlsProvider callback (what Poe
// actually fetches) and the boot-time hash used for cache invalidation
// go through it, so the two cannot diverge.
//
// Regression guard: an earlier version built the live schema straight
// from agent.Models() while hashing a pinned list. The hash changed,
// Poe dutifully re-fetched — and got an UNPINNED schema back, so
// pinned_models silently did nothing in the UI.
//
// OrderPinned is idempotent, so passing an already-pinned list is safe.
func buildControls(models []client.ModelInfo, pinned []string, hosts []config.Host, defaults router.Options) *poeproto.ParameterControls {
	return paramctl.Build(config.OrderPinned(models, pinned), hosts, defaults)
}

// maybeRefetchSettings hashes the freshly built parameter_controls and
// compares against the last-pushed hash on disk. On change, it POSTs
// to Poe's /bot/fetch_settings/<bot>/<key>/1.1 endpoint to invalidate
// Poe's cache so the UI picks up the new schema. Best-effort: every
// failure is logged and swallowed.
//
// endpointBase, when non-empty, replaces the default api.poe.com host —
// used by tests against an httptest.Server.
func maybeRefetchSettings(ctx context.Context, stateDir, botName, accessKey string, controls *poeproto.ParameterControls, endpointBase string) {
	hashFile := filepath.Join(stateDir, "last_schema_hash")
	h := schemaHash(controls)
	prev, _ := os.ReadFile(hashFile)
	if string(prev) == h {
		log.Printf("settings refetch: schema unchanged (hash=%s), skipping", h[:12])
		return
	}

	if endpointBase == "" {
		endpointBase = "https://api.poe.com"
	}
	endpoint := fmt.Sprintf("%s/bot/fetch_settings/%s/%s/1.1",
		endpointBase, url.PathEscape(botName), url.PathEscape(accessKey))
	rctx, rcancel := context.WithTimeout(ctx, 30*time.Second)
	defer rcancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, endpoint, nil)
	if err != nil {
		log.Printf("settings refetch: build request: %v", err)
		return
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("settings refetch: %v", err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		log.Printf("settings refetch: HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
		return
	}
	var echo struct {
		ParameterControls json.RawMessage `json:"parameter_controls"`
	}
	if err := json.Unmarshal(body, &echo); err == nil {
		if len(echo.ParameterControls) == 0 || bytes.Equal(echo.ParameterControls, []byte("null")) {
			log.Printf("settings refetch: WARNING Poe dropped parameter_controls (schema invalid). Body: %s", truncate(string(body), 400))
			return
		}
	}
	if err := os.WriteFile(hashFile, []byte(h), 0o600); err != nil {
		log.Printf("settings refetch: write hash file: %v", err)
		return
	}
	log.Printf("settings refetch: ok (hash=%s)", h[:12])
}

// schemaHash returns a stable SHA-256 hex digest of the parameter
// controls JSON. JSON marshal output of struct types is deterministic
// (field order fixed), and we don't reorder option lists, so equal
// schemas produce equal hashes.
func schemaHash(pc *poeproto.ParameterControls) string {
	if pc == nil {
		return "nil"
	}
	sum := sha256.Sum256(mustMarshalJSON(pc))
	return hex.EncodeToString(sum[:])
}

// truncate shortens s to at most n runes, appending an ellipsis when
// truncation occurs. Operates on runes (not bytes) so it never splits
// a multi-byte UTF-8 sequence in the middle.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

// userHomeDir is overridable in tests.
var userHomeDir = os.UserHomeDir

func defaultStateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "poe-acp")
	}
	if h, err := userHomeDir(); err == nil {
		return filepath.Join(h, ".local", "state", "poe-acp")
	}
	return filepath.Join(os.TempDir(), "poe-acp")
}

func defaultConfigPath() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "poe-acp", "config.json")
	}
	if h, err := userHomeDir(); err == nil {
		return filepath.Join(h, ".config", "poe-acp", "config.json")
	}
	return filepath.Join(os.TempDir(), "poe-acp", "config.json")
}

func appendEnv(env []string, kv string) []string {
	key := strings.SplitN(kv, "=", 2)[0] + "="
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if !strings.HasPrefix(e, key) {
			out = append(out, e)
		}
	}
	return append(out, kv)
}

// resolveStateDir picks the per-conv state dir root: the flag when set,
// else a `state` sibling of an EXPLICIT config file, else the XDG
// default. Pure — the caller creates it. Shared by the worker and the
// --print-skills path, which both need the builtin-skill extraction root
// to be the same directory (see skills.LoadBuiltin).
func resolveStateDir(stateDirFlag, cfgPath string, cfgExplicit bool) string {
	if stateDirFlag != "" {
		return stateDirFlag
	}
	if cfgExplicit {
		return filepath.Join(filepath.Dir(cfgPath), "state")
	}
	return defaultStateDir()
}

// loadBuiltinSkills / loadDirSkills are overridable for tests.
var (
	loadBuiltinSkills = skills.LoadBuiltin
	loadDirSkills     = skills.LoadDir
)

// systemPromptProvider returns the per-session callback the router
// uses to assemble durable system prompt text. It reads cfg.SystemPromptFile
// each call so operator edits apply to the next new conversation without
// a restart, prepends those contents to the skills catalog, and returns
// "" when DisableSystemPrompt is set (which also makes the router skip
// its transport-contract clause).
//
// Config-load and file-read errors here are logged and treated as empty
// — startup already validated both via config.Load and a fail-fast file
// read in main.go, so this per-session path only surfaces post-boot
// edits and prefers to keep live conversations going.
func systemPromptProvider(cfgPath, stateDir string) func() string {
	return func() string {
		cfg, _, err := config.Load(cfgPath)
		if err != nil {
			log.Printf("system_prompt: re-read %s failed (continuing without operator prompt): %v", cfgPath, err)
		}
		if cfg.DisableSystemPrompt {
			return ""
		}
		_, text, err := readSystemPromptFile(filepath.Dir(cfgPath), cfg.SystemPromptFile)
		if err != nil {
			log.Printf("system_prompt_file: read failed (continuing without operator prompt): %v", err)
		}
		return kitsysprompt.Compose("", text, buildSkillsCatalog(cfgPath, stateDir))
	}
}

// readSystemPromptFile resolves configuredPath against cfgDir (when
// relative), reads the file, and returns the resolved absolute path
// plus its trimmed contents. Returns ("", "", nil) when configuredPath
// is empty so callers can use it unconditionally as a fail-fast probe
// at boot and a best-effort read per session. Leading and trailing
// whitespace are stripped so a trailing newline in the prompt file
// doesn't bleed into the catalog separator.
func readSystemPromptFile(cfgDir, configuredPath string) (resolved, contents string, err error) {
	if configuredPath == "" {
		return "", "", nil
	}
	resolved = configuredPath
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(cfgDir, resolved)
	}
	b, err := os.ReadFile(resolved)
	if err != nil {
		return resolved, "", fmt.Errorf("read %s: %w", resolved, err)
	}
	return resolved, strings.TrimSpace(string(b)), nil
}

// buildSkillsCatalog merges embedded built-in skills with optional
// host-supplied skills from <dirname(cfgPath)>/skills/ and returns a
// fir-style <available_skills> block ready for injection. Best-effort:
// extraction failures degrade to whatever layers succeeded (the relay
// is still usable without a catalog). Host skills with the same name
// as a built-in override the built-in (the disable mechanism).
func buildSkillsCatalog(cfgPath, stateDir string) string {
	builtin, err := loadBuiltinSkills(stateDir)
	if err != nil {
		log.Printf("skills: builtin load failed (continuing): %v", err)
	}
	hostDir := filepath.Join(filepath.Dir(cfgPath), "skills")
	host, err := loadDirSkills(hostDir)
	if err != nil {
		log.Printf("skills: host dir %s: %v (continuing)", hostDir, err)
	}
	merged := skills.Merge([][]skills.Skill{builtin, host}, nil)
	if len(merged) == 0 {
		return ""
	}
	names := make([]string, 0, len(merged))
	for _, s := range merged {
		names = append(names, s.Name)
	}
	log.Printf("skills: %d builtin + %d host -> injected %d (%s)",
		len(builtin), len(host), len(merged), strings.Join(names, ","))
	return skills.FormatCatalog(merged)
}

// agentLiveness is the slice of *client.AgentProc the agent watchdog
// needs. An interface so the watchdog is unit-testable without spawning
// a real agent.
type agentLiveness interface {
	Done() <-chan struct{}
	Err() error
}

// watchAgent is the worker's agent-death policy. The agent child is
// invisible to the supervisor — it is the WORKER's child — so nothing
// else can notice it die. Before this existed, a dropped agent (an ssh
// pipe to a flapping host, an agent crash) left the worker alive and
// every subsequent turn failing with `write |1: broken pipe` until a
// human restarted the unit.
//
// The relay cannot rebuild the agent in place: the *client.AgentProc is
// held by the router, the command broker and the MCP host. So the
// policy is deliberately blunt — exit non-zero and let the supervisor's
// respawn path (which already backs off) fork a fresh worker + agent
// pair.
//
// A deliberate Close (drain, shutdown) is not an outage and is ignored.
// exit is a seam for tests; production passes os.Exit.
func watchAgent(a agentLiveness, restart bool, exit func(int)) {
	go func() {
		<-a.Done()
		err := a.Err()
		if errors.Is(err, client.ErrAgentClosed) {
			return
		}
		if !restart {
			log.Printf("agent exited unexpectedly: %v (agent.restart.enabled=false; turns will report the outage)", err)
			return
		}
		log.Printf("agent exited unexpectedly: %v; exiting for a supervisor respawn", err)
		exit(1)
	}()
}

// hostSpecs turns the operator's `hosts` list into the pool's spec
// table: one entry per host that declares its own `agent_cmd`. Hosts
// without one are absent by design — they keep the pre-pool contract
// (the default agent plus a `_meta.host` hint), which the pool reports
// as agentpool.ErrNotPooled.
func hostSpecs(hosts []config.Host) (map[string]agentpool.Spec, error) {
	var specs map[string]agentpool.Spec
	for _, h := range hosts {
		if !h.Pooled() {
			continue
		}
		if specs == nil {
			specs = make(map[string]agentpool.Spec)
		}
		spec := agentpool.Spec{Host: h.Value, Argv: strings.Fields(h.AgentCmd)}
		// Provision() is "" for an agent that shares this machine's
		// filesystem, which is exactly remotefs.Local (nil).
		if dest := h.Provision(); dest != "" {
			// Already validated at config load; re-checked because a
			// silently-nil provisioner would mean staging attachments
			// onto the wrong machine.
			ssh, err := remotefs.New(dest)
			if err != nil {
				return nil, fmt.Errorf("hosts[%q].ssh_host: %w", h.Value, err)
			}
			spec.Prov = ssh
		}
		specs[h.Value] = spec
	}
	return specs, nil
}

// remoteHostSpec reports whether the pooled host named h runs its agent
// on a different machine than the relay. Used only to warn about the
// MCP server's locality.
func remoteHostSpec(hosts []config.Host, h string) bool {
	for _, c := range hosts {
		if c.Value == h {
			return c.Provision() != ""
		}
	}
	return false
}

// startPooledAgent spawns one pooled host's agent child.
//
// lifetime owns the process (acp-kit binds the child to the context it
// is given), so it must be the worker's root context and never a
// per-conversation one. The template is the default agent's client
// config: a pooled agent gets the same environment, access key, admin
// token and client metadata — only the command line and the MCP wiring
// differ.
//
// The `poe` MCP server is offered ONLY to an agent that shares this
// machine's filesystem: it is reached through a unix socket in this
// process's runtime dir, which an agent on another host cannot dial.
// Handing it the config anyway would give the agent a tool surface that
// fails on every call.
func startPooledAgent(lifetime context.Context, template client.Config, spec agentpool.Spec, mcpHost *mcphost.Host) (*client.AgentProc, error) {
	cfg := template
	cfg.Command = spec.Argv
	cfg.MCPServersForSession = pooledMCPServers(mcpHost, spec)
	agent, err := client.Start(lifetime, cfg)
	if err != nil {
		return nil, err
	}
	// Best-effort catalog probe so `session/set_model` on this host has
	// something to validate against. A host whose catalog differs from
	// the default agent's — or which cannot be probed at all — is NOT
	// an error: the relay's schema is built from the default agent, and
	// a model this host does not have simply fails that one set_model.
	probeCtx, cancel := context.WithTimeout(lifetime, 30*time.Second)
	defer cancel()
	if err := agent.ProbeModels(probeCtx); err != nil {
		log.Printf("agent pool: host %q model probe failed (continuing): %v", spec.Host, err)
	} else {
		models, current := agent.Models()
		log.Printf("agent pool: host %q probed %d models (current=%s)", spec.Host, len(models), current)
	}
	return agent, nil
}

// pooledMCPServers is the per-session MCP wiring a pooled agent gets:
// the relay's own `poe` server when the agent shares this machine's
// filesystem, and nothing at all when it does not (see
// startPooledAgent).
func pooledMCPServers(mcpHost *mcphost.Host, spec agentpool.Spec) func(string) []acp.McpServer {
	if mcpHost == nil || spec.Prov != nil {
		return nil
	}
	return func(cwd string) []acp.McpServer {
		return mcpHost.ServerConfigForSession(filepath.Base(cwd))
	}
}

// agentTargetFunc adapts the pool to router.Config.AgentFor.
//
// ErrNotPooled is the ordinary case, not a failure: that host has no
// agent process of its own, so the router's default target serves it
// and the host travels to the agent as a `_meta.host` hint exactly as
// before per-host processes existed.
func agentTargetFunc(pool *agentpool.Pool[*client.AgentProc]) func(context.Context, string) (router.AgentTarget, error) {
	return func(ctx context.Context, host string) (router.AgentTarget, error) {
		e, err := pool.Get(ctx, host)
		switch {
		case errors.Is(err, agentpool.ErrNotPooled):
			return router.AgentTarget{}, nil
		case err != nil:
			return router.AgentTarget{}, err
		}
		return router.AgentTarget{Agent: e.Agent, Prov: e.Spec.Prov, Pooled: true}, nil
	}
}

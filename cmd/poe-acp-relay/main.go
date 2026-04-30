// Command poe-acp-relay is a Poe server-bot that drives ACP agents (e.g.
// fir --mode acp) as a pure ACP client. See docs/poe-acp-relay/DESIGN.md.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kfet/poe-acp-relay/internal/acpclient"
	"github.com/kfet/poe-acp-relay/internal/authbroker"
	"github.com/kfet/poe-acp-relay/internal/config"
	"github.com/kfet/poe-acp-relay/internal/httpsrv"
	"github.com/kfet/poe-acp-relay/internal/paramctl"
	"github.com/kfet/poe-acp-relay/internal/poeproto"
	"github.com/kfet/poe-acp-relay/internal/policy"
	"github.com/kfet/poe-acp-relay/internal/router"
)

// version is set via -ldflags at build time.
var version = "0.1.0-dev"

func main() {
	var (
		httpAddr     = flag.String("http-addr", ":8080", "Poe HTTP listen address")
		agentCmd     = flag.String("agent-cmd", "fir --mode acp", "ACP agent command (stdio)")
		agentDirFlag = flag.String("agent-dir", "", "FIR_AGENT_DIR passed to the child agent (default: inherit)")
		stateDirFlag = flag.String("state-dir", "", "Per-conv state dir root (default: $XDG_STATE_HOME/poe-acp-relay)")
		configFlag   = flag.String("config", "", "Path to JSON config (default: $XDG_CONFIG_HOME/poe-acp-relay/config.json)")
		permission   = flag.String("permission", "allow-all", "Permission policy: allow-all|read-only|deny-all")
		accessKeyEnv = flag.String("access-key-env", "POEACP_ACCESS_KEY", "Env var holding the Poe bearer secret")
		poePath      = flag.String("poe-path", "/poe", "HTTP path for the Poe protocol endpoint")
		introMsg     = flag.String("introduction", "poe-acp-relay: ACP-backed bot.", "Poe introduction message")
		ttl          = flag.Duration("session-ttl", 2*time.Hour, "Idle TTL before a conv session is evicted")
		gcEvery      = flag.Duration("gc-interval", 5*time.Minute, "GC sweep interval")
		heartbeat    = flag.Duration("heartbeat-interval", 10*time.Second, "SSE heartbeat interval (0 to disable)")
		showVersion  = flag.Bool("version", false, "Print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	log.SetOutput(os.Stderr)
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.Printf("poe-acp-relay %s starting", version)

	pol, err := policy.Parse(*permission)
	if err != nil {
		log.Fatalf("policy: %v", err)
	}

	secret := os.Getenv(*accessKeyEnv)
	if secret == "" {
		log.Fatalf("missing $%s (Poe bearer secret)", *accessKeyEnv)
	}

	cfgPath := *configFlag
	if cfgPath == "" {
		cfgPath = defaultConfigPath()
	}
	cfg, cfgFound, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if cfgFound {
		log.Printf("config: %s (bot=%q model=%q thinking=%q profile=%q)",
			cfgPath, cfg.BotName, cfg.Defaults.Model, cfg.Defaults.Thinking, cfg.Agent.Profile)
	} else {
		log.Printf("config: %s not found, using built-in defaults", cfgPath)
	}

	stateDir := *stateDirFlag
	if stateDir == "" {
		stateDir = defaultStateDir()
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		log.Fatalf("state dir: %v", err)
	}
	log.Printf("state dir: %s", stateDir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-sigCh
		log.Printf("shutdown signal: %v", s)
		cancel()
	}()

	// Agent process
	argv := strings.Fields(*agentCmd)
	env := os.Environ()
	if *agentDirFlag != "" {
		env = appendEnv(env, "FIR_AGENT_DIR="+*agentDirFlag)
	}
	agent, err := acpclient.Start(ctx, acpclient.Config{
		Command: argv,
		Cwd:     stateDir, // agent proc cwd; per-session cwd is passed per NewSession
		Env:     env,
		Policy:  pol,
	})
	if err != nil {
		log.Fatalf("start agent: %v", err)
	}
	defer agent.Close()
	log.Printf("agent started: %s", *agentCmd)

	// Probe the agent for its available models so we can populate the
	// model dropdown in the parameter_controls. Best-effort: if the
	// probe fails (auth missing, slow agent, etc.) we just ship the
	// settings without a model dropdown.
	probeCtx, probeCancel := context.WithTimeout(ctx, 30*time.Second)
	defer probeCancel()
	if err := agent.ProbeModels(probeCtx); err != nil {
		log.Printf("probe models failed (continuing without model dropdown): %v", err)
	} else {
		models, current := agent.Models()
		log.Printf("probed %d models (current=%s)", len(models), current)
	}

	// Resolve operator-facing defaults once: config wins, then probe's
	// CurrentModelId for backward-compat, then built-in fallbacks. The
	// same resolved struct seeds Build() (UI default_values) and
	// Router.Defaults (runtime apply on first turn) so they cannot drift.
	models, current := agent.Models()
	defaults := paramctl.Resolve(cfg.Defaults, models, current)
	log.Printf("resolved defaults: model=%q thinking=%q hide_thinking=%v",
		defaults.Model, defaults.Thinking, defaults.HideThinking)

	// Router
	rtr, err := router.New(router.Config{
		Agent:      agent,
		StateDir:   stateDir,
		SessionTTL: *ttl,
		Defaults:   defaults,
	})
	if err != nil {
		log.Fatalf("router: %v", err)
	}
	stopGC := rtr.RunGC(ctx, *gcEvery)
	defer stopGC()

	// HTTP
	broker := authbroker.New(agent)
	if methods := agent.AuthMethods(); len(methods) > 0 {
		ids := make([]string, 0, len(methods))
		for _, m := range methods {
			ids = append(ids, m.ID)
		}
		log.Printf("auth methods: %v", ids)
	}
	h := httpsrv.New(httpsrv.Config{
		Router: rtr,
		Settings: poeproto.SettingsResponse{
			ResponseVersion:     poeproto.SettingsResponseVersion,
			AllowAttachments:    false,
			IntroductionMessage: *introMsg,
		},
		HeartbeatInterval: *heartbeat,
		ParameterControlsProvider: func() *poeproto.ParameterControls {
			m, _ := agent.Models()
			return paramctl.Build(m, defaults)
		},
		AuthBroker: broker,
	})

	// Auto-invalidate Poe's cached settings response when the schema
	// hash changes between boots. Without this, operators must POST
	// /bot/fetch_settings/<bot>/<key>/1.1 manually after editing the
	// config or after a fir auth change. Skipped when bot_name is
	// unset (operator hasn't opted in).
	if cfg.BotName != "" {
		go maybeRefetchSettings(ctx, stateDir, cfg.BotName, secret,
			paramctl.Build(models, defaults))
	} else {
		log.Printf("config: bot_name unset; Poe settings cache will not auto-refetch")
	}

	mux := http.NewServeMux()
	poeHandler := poeproto.BearerAuth(secret, h)
	mux.Handle(*poePath, poeHandler)
	if *poePath != "/poe" {
		// Also serve at /poe so integration tests and local curl work
		// regardless of deploy-specific path mapping.
		mux.Handle("/poe", poeHandler)
	}
	mux.Handle("/debug/sessions", poeproto.BearerAuth(secret, httpsrv.DebugHandler(rtr)))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "ok sessions=%d\n", rtr.Len())
	})

	srv := &http.Server{
		Addr:              *httpAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("listening on %s", *httpAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	_ = srv.Shutdown(shutdownCtx)
	log.Println("bye")
}

// maybeRefetchSettings hashes the freshly built parameter_controls and
// compares against the last-pushed hash on disk. On change, it POSTs
// to Poe's /bot/fetch_settings/<bot>/<key>/1.1 endpoint to invalidate
// Poe's cache so the UI picks up the new schema. Best-effort: every
// failure is logged and swallowed.
func maybeRefetchSettings(ctx context.Context, stateDir, botName, accessKey string, controls *poeproto.ParameterControls) {
	hashFile := filepath.Join(stateDir, "last_schema_hash")
	h, err := schemaHash(controls)
	if err != nil {
		log.Printf("settings refetch: hash schema: %v", err)
		return
	}
	prev, _ := os.ReadFile(hashFile)
	if string(prev) == h {
		log.Printf("settings refetch: schema unchanged (hash=%s), skipping", h[:12])
		return
	}

	endpoint := fmt.Sprintf("https://api.poe.com/bot/fetch_settings/%s/%s/1.1",
		url.PathEscape(botName), url.PathEscape(accessKey))
	rctx, rcancel := context.WithTimeout(ctx, 30*time.Second)
	defer rcancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, endpoint, nil)
	if err != nil {
		log.Printf("settings refetch: build request: %v", err)
		return
	}
	resp, err := http.DefaultClient.Do(req)
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
	// Verify Poe accepted the parameter_controls (it returns null in
	// the echo body if validation dropped them).
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
func schemaHash(pc *poeproto.ParameterControls) (string, error) {
	if pc == nil {
		return "nil", nil
	}
	b, err := json.Marshal(pc)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func defaultStateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "poe-acp-relay")
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".local", "state", "poe-acp-relay")
	}
	return filepath.Join(os.TempDir(), "poe-acp-relay")
}

func defaultConfigPath() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "poe-acp-relay", "config.json")
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".config", "poe-acp-relay", "config.json")
	}
	return filepath.Join(os.TempDir(), "poe-acp-relay", "config.json")
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

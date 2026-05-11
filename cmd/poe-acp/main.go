// Command poe-acp is a Poe server-bot that drives ACP agents (e.g.
// fir --mode acp) as a pure ACP client. See docs/poe-acp/DESIGN.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kfet/poe-acp/internal/acpclient"
	"github.com/kfet/poe-acp/internal/authbroker"
	"github.com/kfet/poe-acp/internal/config"
	"github.com/kfet/poe-acp/internal/debuglog"
	"github.com/kfet/poe-acp/internal/httpsrv"
	"github.com/kfet/poe-acp/internal/paramctl"
	"github.com/kfet/poe-acp/internal/poeproto"
	"github.com/kfet/poe-acp/internal/policy"
	"github.com/kfet/poe-acp/internal/router"
)

// version is set via -ldflags at build time.
var version = "0.1.0-dev"

func main() {
	var (
		httpAddr     = flag.String("http-addr", ":8080", "Poe HTTP listen address")
		agentCmd     = flag.String("agent-cmd", "fir --mode acp", "ACP agent command (stdio)")
		agentDirFlag = flag.String("agent-dir", "", "FIR_AGENT_DIR passed to the child agent (default: inherit)")
		stateDirFlag = flag.String("state-dir", "", "Per-conv state dir root (default: $XDG_STATE_HOME/poe-acp)")
		configFlag   = flag.String("config", "", "Path to JSON config (default: $XDG_CONFIG_HOME/poe-acp/config.json)")
		permission   = flag.String("permission", "allow-all", "Permission policy: allow-all|read-only|deny-all")
		accessKeyEnv = flag.String("access-key-env", "POEACP_ACCESS_KEY", "Env var holding the Poe bearer secret")
		poePath      = flag.String("poe-path", "/poe", "HTTP path for the Poe protocol endpoint")
		introMsg     = flag.String("introduction", "poe-acp: ACP-backed bot.", "Poe introduction message")
		ttl          = flag.Duration("session-ttl", 2*time.Hour, "Idle TTL before a conv session is evicted")
		gcEvery      = flag.Duration("gc-interval", 5*time.Minute, "GC sweep interval")
		heartbeat    = flag.Duration("heartbeat-interval", 1500*time.Millisecond, "SSE heartbeat / spinner tick interval (0 to disable)")
		allowAtt     = flag.Bool("allow-attachments", true, "Advertise allow_attachments in settings; forwards Poe attachments to the agent as ACP ResourceLink/Resource blocks")
		showVersion  = flag.Bool("version", false, "Print version and exit")
		debugFlag    = flag.Bool("debug", false, "Enable verbose debug logging (also via POEACP_DEBUG=1)")
		printCatalog = flag.Bool("print-catalog", false, "Build the merged skills catalog and print it to stdout, then exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	if *debugFlag {
		debuglog.SetEnabled(true)
	}

	log.SetOutput(os.Stderr)
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.Printf("poe-acp %s starting", version)
	if debuglog.Enabled() {
		log.Printf("debug logging: ON")
	}

	pol, err := policy.Parse(*permission)
	if err != nil {
		log.Fatalf("policy: %v", err)
	}

	secret := os.Getenv(*accessKeyEnv)
	if secret == "" {
		log.Fatalf("missing $%s (Poe bearer secret)", *accessKeyEnv)
	}

	cfgPath := *configFlag
	cfgExplicit := cfgPath != ""
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

	if *printCatalog {
		fmt.Print(buildSkillsCatalog(cfgPath))
		return
	}

	stateDir := *stateDirFlag
	if stateDir == "" {
		if cfgExplicit {
			stateDir = filepath.Join(filepath.Dir(cfgPath), "state")
		} else {
			stateDir = defaultStateDir()
		}
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
	catalog := buildSkillsCatalog(cfgPath)
	rtr, err := router.New(router.Config{
		Agent:        agent,
		StateDir:     stateDir,
		SessionTTL:   *ttl,
		Defaults:     defaults,
		SystemPrompt: catalog,
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
			ResponseVersion:       poeproto.SettingsResponseVersion,
			AllowAttachments:      *allowAtt,
			ExpandTextAttachments: *allowAtt,
			IntroductionMessage:   *introMsg,
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
			paramctl.Build(models, defaults), "")
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

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
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
	kitlog "github.com/kfet/acp-kit/log"
	"github.com/kfet/poe-acp/internal/command"
	"github.com/kfet/poe-acp/internal/config"
	"github.com/kfet/poe-acp/internal/httpsrv"
	"github.com/kfet/poe-acp/internal/mcpattach"
	"github.com/kfet/poe-acp/internal/paramctl"
	"github.com/kfet/poe-acp/internal/poeproto"
	"github.com/kfet/poe-acp/internal/router"
	"github.com/kfet/poe-acp/internal/selfupdate"
	"github.com/kfet/poe-acp/internal/statusline"
)

// version is set via -ldflags at build time.
var version = "0.1.0-dev"

func main() {
	// Subcommands are dispatched before flag parsing. `update` is the
	// canonical self-update path (ETXTBSY-safe atomic binary swap +
	// optional supervisor restart); it supersedes the ad-hoc deploy
	// script and `make deploy` over a running binary.
	if len(os.Args) > 1 && os.Args[1] == "update" {
		os.Exit(runUpdate(os.Args[2:]))
	}

	// `mcp-attach` is the self-hosted stdio MCP server the agent spawns
	// (configured via per-session McpServerStdio). It exposes one
	// `attach` tool and relays calls back to this process over a unix
	// socket. Reads its config from the env the parent set.
	if len(os.Args) > 1 && os.Args[1] == "mcp-attach" {
		if err := mcpattach.RunFromEnv(); err != nil {
			fmt.Fprintln(os.Stderr, "mcp-attach:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	var (
		httpAddr        = flag.String("http-addr", ":8080", "Poe HTTP listen address")
		agentCmd        = flag.String("agent-cmd", "fir --mode acp", "ACP agent command (stdio)")
		agentDirFlag    = flag.String("agent-dir", "", "FIR_AGENT_DIR passed to the child agent (default: inherit)")
		stateDirFlag    = flag.String("state-dir", "", "Per-conv state dir root (default: $XDG_STATE_HOME/poe-acp)")
		configFlag      = flag.String("config", "", "Path to JSON config (default: $XDG_CONFIG_HOME/poe-acp/config.json)")
		accessKeyEnv    = flag.String("access-key-env", "POEACP_ACCESS_KEY", "Env var holding the Poe bearer secret")
		poePath         = flag.String("poe-path", "/poe", "HTTP path for the Poe protocol endpoint")
		introMsg        = flag.String("introduction", "poe-acp: ACP-backed bot.", "Poe introduction message")
		ttl             = flag.Duration("session-ttl", 10*time.Minute, "Idle TTL before a conv session is evicted")
		sessCreateTO    = flag.Duration("session-create-timeout", 60*time.Second, "Bounds session acquisition (list/resume/new); decoupled from the request ctx so a flaky first request still warms the session")
		gcEvery         = flag.Duration("gc-interval", 5*time.Minute, "GC sweep interval")
		heartbeat       = flag.Duration("heartbeat-interval", 1500*time.Millisecond, "SSE heartbeat / spinner tick interval (0 to disable)")
		turnTimeout     = flag.Duration("turn-timeout", 5*time.Minute, "Bounds a prompt turn run on a context decoupled from the request ctx; a pre-output transport drop lets the turn finish so its answer can be buffered for the redrive")
		answerTTL       = flag.Duration("answer-ttl", 2*time.Minute, "How long a buffered (absorbed) turn answer is held for a redrive before discard")
		allowAtt        = flag.Bool("allow-attachments", true, "Advertise allow_attachments in settings; forwards Poe attachments to the agent as ACP ResourceLink/Resource blocks")
		showVersion     = flag.Bool("version", false, "Print version and exit")
		debugFlag       = flag.Bool("debug", false, "Enable verbose debug logging (also via POEACP_DEBUG=1)")
		printCatalog    = flag.Bool("print-catalog", false, "Build the merged skills catalog and print it to stdout, then exit")
		enableMCPAttach = flag.Bool("enable-mcp-attach", false, "Expose a self-hosted MCP `attach` tool to the agent (deliver files to the user as Poe attachments)")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	// Enable debug logging if either source asks for it. Neither
	// disables (--debug=false keeps env-set debug on); that matches
	// the pre-acp-kit debuglog.init() + --debug behaviour.
	kitlog.Register("POEACP_DEBUG")
	if *debugFlag {
		kitlog.SetEnabled(true)
	}

	log.SetOutput(os.Stderr)
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.Printf("poe-acp %s starting", version)
	if kitlog.Enabled() {
		log.Printf("debug logging: ON")
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

	// Fail-fast: a configured system_prompt_file that can't be read at
	// boot is almost always a typo or missing-file mistake the operator
	// wants to know about immediately, not silently at first
	// conversation. The per-session re-read in systemPromptProvider
	// degrades gracefully for edits made after boot.
	if cfg.SystemPromptFile != "" {
		resolved, text, err := readSystemPromptFile(filepath.Dir(cfgPath), cfg.SystemPromptFile)
		if err != nil {
			log.Fatalf("system_prompt_file: %v", err)
		}
		log.Printf("system_prompt_file: %s loaded (%d bytes)", resolved, len(text))
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
	var mcpSocket, mcpDir string
	var mcpReg *mcpattach.Registry
	if *enableMCPAttach {
		mcpReg = mcpattach.NewRegistry()
		base := os.Getenv("XDG_RUNTIME_DIR")
		if base == "" {
			base = os.TempDir()
		}
		// Private 0700 dir (MkdirTemp default perms) so only this uid can
		// reach the socket; the socket file itself is further chmod'd 0600.
		d, derr := os.MkdirTemp(base, "poe-acp-mcp-")
		if derr != nil {
			log.Fatalf("enable-mcp-attach: create socket dir: %v", derr)
		}
		mcpDir = d
		defer os.RemoveAll(mcpDir)
		mcpSocket = filepath.Join(mcpDir, "attach.sock")
	}
	selfExe, exeErr := os.Executable()
	if *enableMCPAttach && exeErr != nil {
		log.Fatalf("enable-mcp-attach: cannot resolve own executable: %v", exeErr)
	}
	clientCfg := client.Config{
		Command: argv,
		Cwd:     stateDir, // agent proc cwd; per-session cwd is passed per NewSession
		Env:     env,
		ClientMeta: map[string]any{
			// Advertise support for the dev.acp-kit.status-line/v1
			// extension so agents that care can emit mood/plan in
			// session/update._meta. See docs/ext/status-line.md.
			statusline.ExtensionID: map[string]any{"version": 1},
		},
	}
	if *enableMCPAttach {
		clientCfg.MCPServersForSession = func(cwd string) []acp.McpServer {
			// Mint a fresh per-session token bound to this conversation.
			// The conv is derived server-side from the token; it is never
			// sent by the client, so it cannot be spoofed.
			tok := mcpReg.Register(filepath.Base(cwd))
			return []acp.McpServer{{Stdio: &acp.McpServerStdio{
				Name:    "poe-attach",
				Command: selfExe,
				Args:    []string{"mcp-attach"},
				Env: []acp.EnvVariable{
					{Name: mcpattach.EnvSocket, Value: mcpSocket},
					{Name: mcpattach.EnvToken, Value: tok},
				},
			}}}
		}
	}
	agent, err := client.Start(ctx, clientCfg)
	if err != nil {
		log.Fatalf("start agent: %v", err)
	}
	defer agent.Close()
	log.Printf("agent started: %s", *agentCmd)
	if _, ok := agent.Caps().Extensions[statusline.ExtensionID]; ok {
		log.Printf("agent advertises %s", statusline.ExtensionID)
	}

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
	broker := command.New(agent)
	rtr, err := router.New(router.Config{
		Agent:                agent,
		StateDir:             stateDir,
		SessionTTL:           *ttl,
		SessionCreateTimeout: *sessCreateTO,
		Defaults:             defaults,
		SystemPromptProvider: systemPromptProvider(cfgPath),
		AuthErrorHint:        broker.OfferLogin,
		Version:              version,
		AgentCmd:             *agentCmd,
		StartTime:            time.Now(),
		AccessKey:            secret,
		MCPAttachEnabled:     *enableMCPAttach,
	})
	if err != nil {
		log.Fatalf("router: %v", err)
	}
	broker.SetController(rtr)
	stopGC := rtr.RunGC(ctx, *gcEvery)
	defer stopGC()

	// MCP attach: start the unix-socket listener that the self-hosted
	// `mcp-attach` subprocess relays to. Must be up before any session
	// is created (i.e. before serving). Token-authenticated.
	if *enableMCPAttach {
		ln, lerr := mcpattach.Listen(mcpSocket, mcpReg.Resolve, func(conv, path, name string, inline bool) error {
			return rtr.AttachActive(conv, path, name, inline)
		})
		if lerr != nil {
			log.Fatalf("mcp-attach listener: %v", lerr)
		}
		defer ln.Close()
		log.Printf("mcp-attach: listening on %s", mcpSocket)
	}

	// HTTP
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
		TurnTimeout:       *turnTimeout,
		AnswerTTL:         *answerTTL,
		ParameterControlsProvider: func() *poeproto.ParameterControls {
			m, _ := agent.Models()
			return paramctl.Build(m, defaults)
		},
		Commands: broker,
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

// runUpdate implements the `poe-acp update` subcommand: download the
// latest (or pinned) release, verify its sha256, and atomically replace
// the running binary. With -restart-cmd set, it runs that command (via
// `sh -c`) afterwards so a supervisor (systemd --user / launchd) picks up
// the new binary — note this drops any in-flight conversation, which is
// inherent to restarting the relay process.
func runUpdate(args []string) int {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	check := fs.Bool("check", false, "report whether an update is available; do not install")
	ver := fs.String("version", "", "install a specific version (e.g. v0.27.0); default: latest")
	repo := fs.String("repo", selfupdate.DefaultRepo, "github owner/repo to update from")
	restartCmd := fs.String("restart-cmd", "", "shell command to run after a successful update (e.g. \"systemctl --user restart poe-acp-sea-fir\")")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	res, err := selfupdate.Run(version, selfupdate.Options{
		Repo:      *repo,
		Version:   *ver,
		CheckOnly: *check,
		Stdout:    os.Stdout,
		Stderr:    os.Stderr,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "update:", err)
		return 1
	}
	if !res.Updated {
		return 0
	}

	if *restartCmd == "" {
		fmt.Print("restart the service to pick up the new binary, e.g.:\n  systemctl --user restart poe-acp-<bot>\n")
		return 0
	}
	fmt.Printf("restarting: %s\n", *restartCmd)
	c := exec.Command("sh", "-c", *restartCmd)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "restart:", err)
		return 1
	}
	return 0
}

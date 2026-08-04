// Command poe-acp is a Poe server-bot that drives ACP agents (e.g.
// fir --mode acp) as a pure ACP client. See docs/poe-acp/DESIGN.md.
package main

import (
	"context"
	"crypto/subtle"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
	kitlog "github.com/kfet/acp-kit/log"
	"github.com/kfet/acp-kit/mcphost"
	"github.com/kfet/poe-acp/internal/agentcfg"
	"github.com/kfet/poe-acp/internal/command"
	"github.com/kfet/poe-acp/internal/config"
	"github.com/kfet/poe-acp/internal/httpsrv"
	"github.com/kfet/poe-acp/internal/paramctl"
	"github.com/kfet/poe-acp/internal/poemcp"
	"github.com/kfet/poe-acp/internal/poeproto"
	"github.com/kfet/poe-acp/internal/router"
	"github.com/kfet/poe-acp/internal/sdnotify"
	"github.com/kfet/poe-acp/internal/selfupdate"
	"github.com/kfet/poe-acp/internal/statusline"
	"github.com/kfet/poe-acp/internal/supervisor"
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

	// `mcp-serve` is the self-hosted stdio MCP server the agent spawns
	// (configured via per-session McpServerStdio). It exposes the `poe`
	// server's tools (attach, suggest) and relays calls back to this
	// process over a unix socket. Reads its config from the env the
	// parent set. `mcp-attach` is accepted as a legacy alias. The dumb
	// redirector lives in acp-kit/mcphost.
	if handled, err := mcphost.MaybeRunRedir(poemcp.RedirConfig()); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, "mcp-serve:", err)
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
		turnTimeout     = flag.Duration("turn-timeout", 0, "OPTIONAL absolute wall-clock ceiling on a single prompt turn. 0 (default) = no ceiling: an actively-progressing turn is NEVER cut, and only -idle-write-timeout (progress-resetting) can cancel a wedged turn. Set >0 only if you deliberately want a hard upper bound regardless of progress")
		idleWriteTO     = flag.Duration("idle-write-timeout", 2*time.Minute, "Per-stream wedged-turn backstop: cancel a turn that writes no agent output within this window (heartbeat keepalives do not reset it; a tool_call update does). The only force-kill path during a graceful drain")
		stallThreshold  = flag.Duration("stall-threshold", 8*time.Second, "Output-silence window before the mid-turn keepalive spinner re-arms via replace_response. Keeps Poe from content-starvation-dropping a long tool-heavy turn. Must stay well under Poe's drop tolerance")
		sseWriteTO      = flag.Duration("sse-write-timeout", 30*time.Second, "Per-write deadline on every SSE wire write. A client that stopped reading (Poe Stop, dead transport) applies TCP backpressure; without this a blocked write — including the terminal done — never returns and the turn never finalizes. Generous enough never to cut a slow-but-live reader mid-frame")
		drainDeadline   = flag.Duration("drain-deadline", supervisor.DefaultDrainDeadline, "OUTER drain bound for a service STOP (systemd stop/restart, supervisor self-upgrade). After SIGTERM a worker drains its in-flight SSE streams to natural completion; if any are still open after this window the worker force-closes them and exits anyway, and the supervisor SIGKILLs a worker that still has not exited "+supervisor.RetireGrace.String()+" later. Something external is waiting on this chain, so it must stay comfortably under the init system's stop timeout (systemd TimeoutStopSec=90s). A SIGHUP worker swap uses -swap-drain-deadline instead")
		swapDrainDL     = flag.Duration("swap-drain-deadline", supervisor.DefaultSwapDrainDeadline, "OUTER drain bound for a worker SWAPPED OUT by a SIGHUP reload. Nothing external is waiting on this chain — the supervisor survives and the new worker is already serving — so the retiring worker is given long enough to finish healthy turns that legitimately run tens of minutes; -idle-write-timeout (progress-resetting) is what reaps a wedged one. This is a leak backstop, not a working bound: on expiry the worker force-closes what is left and exits, and the supervisor SIGKILLs it "+supervisor.RetireGrace.String()+" later")
		answerTTL       = flag.Duration("answer-ttl", 2*time.Minute, "How long a buffered (absorbed) turn answer is held for a redrive before discard")
		allowAtt        = flag.Bool("allow-attachments", true, "Advertise allow_attachments in settings; forwards Poe attachments to the agent as ACP ResourceLink/Resource blocks")
		showVersion     = flag.Bool("version", false, "Print version and exit")
		debugFlag       = flag.Bool("debug", false, "Enable verbose debug logging (also via POEACP_DEBUG=1)")
		printCatalog    = flag.Bool("print-catalog", false, "Build the merged skills catalog and print it to stdout, then exit")
		enableMCPAttach = flag.Bool("enable-mcp-attach", false, "Deprecated alias for the config `poe_mcp` knob: expose the self-hosted `poe` MCP server (attach + suggest tools) to the agent. Prefer setting poe_mcp:true in config.json")
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
	// Clamp both drain bounds ONCE, here, so the worker's own deadline
	// and the supervisor's escalation deadline (drain + RetireGrace) are
	// always derived from the same effective value. Without this,
	// -drain-deadline 0 would leave the worker on the 45s default while
	// the supervisor escalated at 15s — SIGKILLing healthy streams on
	// every reload.
	if *drainDeadline <= 0 {
		*drainDeadline = supervisor.DefaultDrainDeadline
	}
	if *swapDrainDL <= 0 {
		*swapDrainDL = supervisor.DefaultSwapDrainDeadline
	}
	if kitlog.Enabled() {
		log.Printf("debug logging: ON")
	}

	secret := os.Getenv(*accessKeyEnv)
	if secret == "" {
		log.Fatalf("missing $%s (Poe bearer secret)", *accessKeyEnv)
	}
	// Read the admin-reexec token here (not at its /admin/reexec use
	// site) so it can be declared as a secret dropped from the spawned
	// agent's environment before the agent is started.
	adminToken := os.Getenv(agentcfg.AdminTokenEnv)

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

	// Master/worker split. The default invocation is the SUPERVISOR: it
	// binds the listen socket once and forks worker processes, never
	// exiting during an upgrade so the init system's tracked PID is
	// stable (no EADDRINUSE-on-relaunch on launchd or systemd). A worker
	// is detected by POE_ACP_WORKER_FD being set; it recovers the
	// inherited listener and runs all the relay logic below. The
	// supervisor is tiny and never reaches the agent/router setup.
	if !supervisor.IsWorker() {
		runSupervisor(*httpAddr, version, *drainDeadline, *swapDrainDL)
		return
	}
	log.Printf("worker mode (pid=%d, supervisor=%d)", os.Getpid(), os.Getppid())
	// Before anything is exec'd: the listener and both supervisor pipes
	// arrive with O_CLOEXEC cleared, so without this the ACP agent
	// spawned below would inherit a dup of the listening socket and keep
	// the port bound if it were ever orphaned.
	supervisor.SealInheritedFDs()

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

	// Agent process
	argv := strings.Fields(*agentCmd)
	env := os.Environ()
	if *agentDirFlag != "" {
		env = appendEnv(env, "FIR_AGENT_DIR="+*agentDirFlag)
	}
	// Effective MCP enablement: config `poe_mcp` OR the deprecated
	// --enable-mcp-attach flag. Per-bot config is the preferred surface.
	mcpEnabled := *enableMCPAttach || cfg.PoeMCP
	var mcpHost *mcphost.Host
	if mcpEnabled {
		h, herr := mcphost.New(poemcp.HostConfig())
		if herr != nil {
			log.Fatalf("poe-mcp: %v", herr)
		}
		mcpHost = h
		defer mcpHost.Close()
	}
	clientCfg := agentcfg.New(agentcfg.Params{
		Command:      argv,
		Cwd:          stateDir, // agent proc cwd; per-session cwd is passed per NewSession
		Env:          env,
		AccessKeyEnv: *accessKeyEnv,
		AccessKey:    secret,
		AdminToken:   adminToken,
		ClientMeta: map[string]any{
			// Advertise support for the dev.acp-kit.status-line/v1
			// extension so agents that care can emit mood/plan in
			// session/update._meta. See docs/ext/status-line.md.
			statusline.ExtensionID: map[string]any{"version": 1},
		},
	})
	if mcpEnabled {
		clientCfg.MCPServersForSession = func(cwd string) []acp.McpServer {
			// Mint a fresh per-session token bound to this conversation.
			// The conv is derived server-side from the token; it is never
			// sent by the client, so it cannot be spoofed.
			return mcpHost.ServerConfigForSession(filepath.Base(cwd))
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
	log.Printf("resolved defaults: model=%q thinking=%q hide_thinking=%v show_plans=%v show_tools=%v show_tool_details=%v",
		defaults.Model, defaults.Thinking, defaults.HideThinking, defaults.ShowPlans, defaults.ShowTools, defaults.ShowToolDetails)

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
		MCPAttachEnabled:     mcpEnabled,
	})
	if err != nil {
		log.Fatalf("router: %v", err)
	}
	broker.SetController(rtr)
	stopGC := rtr.RunGC(ctx, *gcEvery)
	defer stopGC()

	// MCP server: register the poe tools and start the unix-socket
	// listener that the self-hosted `mcp-serve` subprocess relays to. Must
	// be up before any session is created (i.e. before serving).
	// Token-authenticated.
	if mcpEnabled {
		poemcp.Register(mcpHost, rtr)
		if lerr := mcpHost.Listen(); lerr != nil {
			log.Fatalf("mcp-serve listener: %v", lerr)
		}
		log.Printf("mcp-serve: listening on %s", mcpHost.SocketPath())
	}

	// HTTP
	if methods := agent.AuthMethods(); len(methods) > 0 {
		ids := make([]string, 0, len(methods))
		for _, m := range methods {
			ids = append(ids, m.ID)
		}
		log.Printf("auth methods: %v", ids)
	}
	// Outbound stream shaping (flush coalescing + spinner animation) is
	// per-bot config, resolved in one tested place.
	stream := cfg.Defaults.Stream()
	log.Printf("stream shaping: coalesce=%s grid=%v spinner_animate=%v",
		stream.CoalesceInterval, stream.CoalesceGrid, stream.SpinnerAnimate)
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
		IdleWriteTimeout:  *idleWriteTO,
		StallThreshold:    *stallThreshold,
		SSEWriteTimeout:   *sseWriteTO,
		AnswerTTL:         *answerTTL,
		CoalesceInterval:  stream.CoalesceInterval,
		CoalesceGrid:      stream.CoalesceGrid,
		SpinnerStatic:     !stream.SpinnerAnimate,
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
		fmt.Fprintf(w, "ok pid=%d sessions=%d\n", os.Getpid(), rtr.Len())
	})

	// Worker: recover the serving listener handed down by the supervisor
	// (it owns the bound socket; we never bind).
	ln, err := supervisor.WorkerListener()
	if err != nil {
		log.Fatalf("worker listener: %v", err)
	}

	// If the supervisor dies, the parent-liveness pipe EOFs; relinquish
	// the socket so the init system can relaunch the supervisor cleanly
	// rather than orphan-locking the port.
	supervisor.WatchParent(func() {
		log.Printf("supervisor gone; worker exiting to free socket")
		os.Exit(0)
	})

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// SIGTERM = stop accepting, drain in-flight streams to natural
	// completion (the per-stream idle-write backstop cuts a wedged turn),
	// then exit cleanly — BOUNDED so a stream the idle-write backstop
	// cannot clear can never make this worker immortal.
	//
	// WHICH bound depends on why we are being retired, and only the
	// supervisor knows that: it writes a DrainOrder to this worker's
	// control pipe just before the SIGTERM. A swap (SIGHUP reload) grants
	// -swap-drain-deadline; anything else — including a SIGTERM systemd
	// delivers straight to the cgroup on a service stop — falls back to
	// -drain-deadline, the externally bounded contract.
	ctl := supervisor.WorkerControl()
	drainCh := make(chan struct{})
	var drainOrder supervisor.DrainOrder
	var drainOnce sync.Once
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)
	go func() {
		for range term {
			order, err := ctl.DrainOrder(*drainDeadline)
			if err != nil {
				log.Printf("worker: WARN %v; assuming a %s", err, order)
			}
			drainOnce.Do(func() {
				drainOrder = order
				log.Printf("SIGTERM: %s of in-flight streams (idle-write backstop active)", order)
				close(drainCh)
			})
		}
	}()

	// A SIGHUP delivered directly to a WORKER (the pid /healthz reports,
	// and the obvious thing for an operator to signal) used to be fatal:
	// Go's default disposition for SIGHUP terminates the process, so
	// every in-flight SSE stream was dropped instantly — the exact
	// opposite of a graceful reload. The supervisor mediates worker
	// lifecycle, so forward the intent instead of dying.
	hupW := make(chan os.Signal, 1)
	signal.Notify(hupW, syscall.SIGHUP)
	go func() {
		for range hupW {
			log.Printf("SIGHUP: forwarding worker-swap request to supervisor")
			if err := supervisor.SignalSupervisor(syscall.SIGHUP); err != nil {
				log.Printf("worker: %v", err)
			}
		}
	}()

	// Optional admin trigger for shell-less automated update flows. A
	// worker cannot upgrade itself; it asks the supervisor. POST
	// /admin/reexec -> SIGHUP supervisor (drained worker swap, the common
	// path); ?scope=supervisor -> SIGUSR2 supervisor (self-upgrade, rare).
	if adminTok := adminToken; adminTok != "" {
		mux.HandleFunc("/admin/reexec", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(tok), []byte(adminTok)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			sig := syscall.SIGHUP
			scope := "worker-swap"
			if r.URL.Query().Get("scope") == "supervisor" {
				sig = syscall.SIGUSR2
				scope = "supervisor-self-upgrade"
			}
			log.Printf("/admin/reexec: requesting %s from supervisor", scope)
			if err := supervisor.SignalSupervisor(sig); err != nil {
				http.Error(w, "signal supervisor: "+err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusAccepted)
		})
		log.Printf("admin reexec endpoint enabled at /admin/reexec")
	}

	go func() {
		log.Printf("worker listening on %s (pid=%d)", ln.Addr(), os.Getpid())
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	// Tell the supervisor this worker's Serve is live so it may drain a
	// previous worker. The kernel accept queue covers the gap until
	// Serve runs, so no connection is ever refused.
	if err := supervisor.NotifyReady(); err != nil {
		log.Printf("worker: notify supervisor ready failed: %v", err)
	} else {
		log.Printf("worker: signalled supervisor ready")
	}

	<-drainCh
	// Bounded drain: http.Server.Shutdown lets a legitimately streaming
	// turn finish undisturbed, and the per-stream idle-write backstop
	// cuts a wedged turn. If BOTH miss — a handler stuck in a way the
	// idle clock cannot see — the deadline force-closes what is left and
	// this worker exits anyway, instead of joining the pile of undead
	// generations systemd eventually SIGKILLs.
	res, derr := supervisor.DrainServer(srv, drainOrder.Deadline, func() {
		abandoned := h.InFlight()
		log.Printf("WARN %s drain deadline %s exceeded: force-closing %d in-flight stream(s) — raise %s to give them longer",
			drainOrder.Kind, drainOrder.Deadline, len(abandoned), drainOrder.Kind.Flag())
		for _, s := range abandoned {
			log.Printf("WARN   abandoned stream: conv=%s user=%s msg=%s age=%s",
				s.ConvID, s.UserID, s.MessageID, s.Age.Round(time.Second))
		}
		// Graceful abandon: seal each still-registered stream at the
		// Poe protocol level (error with allow_retry + terminal done)
		// BEFORE DrainServer's srv.Close() truncates it. Poe then sees
		// a well-formed terminal error instead of a cut connection, and
		// may redrive the same message_id against the new worker, which
		// resumes the same fir session by conversation cwd.
		if n := h.SealInFlight(httpsrv.AbandonMessage, httpsrv.DefaultSealTimeout); n > 0 {
			log.Printf("worker: sealed %d abandoned stream(s) with a retryable error", n)
		}
		// Cleanup below (agent shutdown, MCP host close) must not become
		// the new place we hang: exit regardless once it has had its
		// grace. Left armed deliberately — the process is exiting on
		// this path either way, so the timer is moot if cleanup wins.
		_ = supervisor.ForceExitAfter(supervisor.ForceExitGrace, func() {
			log.Printf("WARN post-drain cleanup did not finish in %s; force-exiting", supervisor.ForceExitGrace)
			os.Exit(0)
		})
	})
	if res == supervisor.DrainForced {
		log.Printf("worker: %s drain FORCED after %s, exiting", drainOrder.Kind, drainOrder.Deadline)
	} else {
		if derr != nil {
			log.Printf("worker: drain completed with error: %v", derr)
		}
		log.Printf("worker: drain complete, exiting")
	}
	// No force-exit backstop on the graceful path: if cleanup below
	// wedges, the supervisor's own escalation (drain-deadline +
	// RetireGrace) SIGKILLs this worker's process group, so it still
	// cannot become an undead generation.
	cancel() // stop the agent + GC via deferred Close()
}

// worker pairs a forked worker process with the channel its reaper closes
// when the process exits. Retirement needs both: the pid to signal and a
// death signal to bound the wait on.
type worker struct {
	proc *os.Process
	dead chan struct{}
}

// runSupervisor is the master process: it binds the listen socket once
// and forks/manages worker processes. It is intentionally tiny and rarely
// changes, so the init system's tracked PID stays stable across worker
// upgrades — making EADDRINUSE-on-relaunch structurally impossible on both
// launchd and systemd. It does not return: it os.Exit()s on shutdown, or
// re-execs itself in place on a supervisor self-upgrade.
//
// drainDeadline and swapDrainDeadline are the two worker-side bounds (a
// service stop vs a SIGHUP worker swap — see supervisor/drainorder.go).
// The supervisor hands the applicable one to each worker it retires and
// allows that worker the same value plus supervisor.RetireGrace before
// escalating to SIGKILL, so a wedged worker generation can never survive
// its retirement.
func runSupervisor(addr, version string, drainDeadline, swapDrainDeadline time.Duration) {
	sup, err := supervisor.New(supervisor.Config{Addr: addr})
	if err != nil {
		log.Fatalf("supervisor: %v", err)
	}
	log.Printf("supervisor %s on %s (pid=%d)", version, sup.Addr(), os.Getpid())
	// The two retirement contracts. A stop is externally bounded (systemd
	// TimeoutStopSec); a swap is not — the supervisor keeps serving via
	// the new worker while the old one finishes its tails.
	stopOrder := supervisor.DrainOrder{Kind: supervisor.DrainStop, Deadline: drainDeadline}.Normalize()
	swapOrder := supervisor.DrainOrder{Kind: supervisor.DrainSwap, Deadline: swapDrainDeadline}.Normalize()

	// SIGUSR1 from a freshly spawned worker => it is ready to serve.
	readySig := make(chan os.Signal, 8)
	signal.Notify(readySig, syscall.SIGUSR1)
	readyCh := make(chan struct{}, 8)
	go func() {
		for range readySig {
			select {
			case readyCh <- struct{}{}:
			default:
			}
		}
	}()

	// Worker exits funnel here as pids (one waiter goroutine per worker).
	exitCh := make(chan int, 8)

	// retire drains w under order and escalates to SIGKILL if it has not
	// exited within order.RetireDeadline(). Blocks until w's exit is
	// observed (or the escalation has run), so callers that need
	// quiescence — self-reexec, shutdown — can call it inline.
	retire := func(w *worker, order supervisor.DrainOrder) {
		res, err := sup.Retire(w.proc, w.dead, order, nil)
		switch {
		case err != nil:
			log.Printf("supervisor: WARN retire worker %d (%s): %v", w.proc.Pid, res, err)
		case res == supervisor.RetireKilled:
			log.Printf("supervisor: WARN worker %d still had streams open %s after SIGTERM; SIGKILLed", w.proc.Pid, order.RetireDeadline())
		default:
			log.Printf("supervisor: worker %d drained and exited", w.proc.Pid)
		}
	}

	// logPileup surfaces worker generations that were told to retire and
	// have not exited yet. A steady-state relay shows none; a non-empty
	// list at swap time is the leak signature from the 2026-07-26 incident.
	logPileup := func() {
		if r := sup.Retiring(); len(r) > 0 {
			for _, w := range r {
				log.Printf("supervisor: WARN worker %d still retiring after %s", w.Pid, time.Since(w.Since).Round(time.Second))
			}
		}
	}

	// spawnReady forks a worker and blocks until it signals ready or dies.
	spawnReady := func() (*worker, error) {
		// Drain stale ready tokens so a previous worker's signal can't
		// prematurely satisfy this wait.
		for {
			select {
			case <-readyCh:
				continue
			default:
			}
			break
		}
		p, err := sup.Spawn()
		if err != nil {
			return nil, err
		}
		dead := make(chan struct{})
		go func() {
			_, _ = p.Wait()
			close(dead)
			// The worker is reaped: drop its control pipe before
			// announcing the exit, so nothing can try to hand an order to
			// a pid that is gone.
			sup.ReleaseWorker(p.Pid)
			exitCh <- p.Pid
		}()
		switch supervisor.WaitReady(readyCh, dead, 90*time.Second, nil) {
		case supervisor.ReadyOK:
			return &worker{proc: p, dead: dead}, nil
		case supervisor.ReadyDied:
			return nil, fmt.Errorf("worker %d exited before ready", p.Pid)
		default:
			// Never-ready worker: it is alive, holds a dup of the listener
			// and competes on the accept queue. Retire it (SIGTERM, then
			// a process-group SIGKILL on the deadline) instead of
			// abandoning it — an untracked live worker is precisely the
			// leak this whole change exists to prevent. It has no streams
			// to finish, so it retires under the STOP contract.
			go retire(&worker{proc: p, dead: dead}, stopOrder)
			return nil, fmt.Errorf("worker %d not ready within budget", p.Pid)
		}
	}

	current, err := spawnReady()
	if err != nil {
		log.Fatalf("supervisor: initial worker: %v", err)
	}
	sup.SetCurrent(current.proc)
	log.Printf("supervisor: worker pid=%d serving", current.proc.Pid)

	// Now that a worker is serving, tell systemd we are up (Type=notify).
	// No-op when NOTIFY_SOCKET is unset (launchd / bare process).
	if sent, err := sdnotify.Ready(); err != nil {
		log.Printf("sdnotify: readiness notify failed: %v", err)
	} else if sent {
		log.Printf("sdnotify: notified systemd ready")
	}

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	usr2 := make(chan os.Signal, 1)
	signal.Notify(usr2, syscall.SIGUSR2)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	for {
		select {
		case <-hup:
			log.Printf("SIGHUP: worker swap")
			logPileup()
			nw, err := spawnReady()
			if err != nil {
				log.Printf("supervisor: swap aborted, keeping worker %d: %v", current.proc.Pid, err)
				continue
			}
			old := current
			current = nw
			sup.SetCurrent(nw.proc)
			log.Printf("supervisor: worker %d serving; retiring old worker %d (%s)", nw.proc.Pid, old.proc.Pid, swapOrder)
			// Retire off the loop: the old worker keeps draining its
			// in-flight streams while the supervisor stays responsive.
			// This is the SWAP contract — nothing external is waiting on
			// it, so a healthy multi-minute turn is left alone.
			go retire(old, swapOrder)
		case <-usr2:
			log.Printf("SIGUSR2: supervisor self-upgrade (retiring worker %d first)", current.proc.Pid)
			// A self-upgrade is NOT a swap: no replacement worker is
			// serving and the supervisor blocks here, so this drain is
			// externally bounded exactly like a stop.
			retire(current, stopOrder)
			log.Printf("supervisor: quiescent; re-exec self")
			if err := sup.SelfReexec(); err != nil {
				log.Fatalf("supervisor: self-reexec: %v", err)
			}
		case s := <-stop:
			log.Printf("supervisor: %v; retiring worker %d then exiting", s, current.proc.Pid)
			retire(current, stopOrder)
			// Anything still retiring from an earlier swap gets the same
			// treatment: no worker generation outlives its supervisor.
			// That also bounds a swap drain that a stop landed in the
			// middle of — it keeps its 30m order, but it never gets to
			// use it.
			for _, k := range sup.KillRetiring() {
				log.Printf("supervisor: WARN worker %d still retiring after %s at shutdown; SIGKILL", k.Pid, time.Since(k.Since).Round(time.Second))
				if k.Err != nil {
					log.Printf("supervisor: %v", k.Err)
				}
			}
			log.Printf("supervisor: bye")
			os.Exit(0)
		case pid := <-exitCh:
			if pid != current.proc.Pid {
				log.Printf("supervisor: reaped worker %d", pid)
				continue
			}
			log.Printf("supervisor: serving worker %d died unexpectedly; respawning", pid)
			nw, err := spawnReady()
			if err != nil {
				log.Fatalf("supervisor: respawn failed: %v", err)
			}
			current = nw
			sup.SetCurrent(nw.proc)
			log.Printf("supervisor: worker pid=%d serving", nw.proc.Pid)
		}
	}
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

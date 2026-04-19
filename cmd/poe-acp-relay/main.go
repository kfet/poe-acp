// Command poe-acp-relay is a Poe server-bot that drives ACP agents (e.g.
// fir --mode acp) as a pure ACP client. See docs/poe-acp-relay/DESIGN.md.
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

	"github.com/kfet/fir/external/poeacp/internal/acpclient"
	"github.com/kfet/fir/external/poeacp/internal/httpsrv"
	"github.com/kfet/fir/external/poeacp/internal/poeproto"
	"github.com/kfet/fir/external/poeacp/internal/policy"
	"github.com/kfet/fir/external/poeacp/internal/router"
)

// version is set via -ldflags at build time.
var version = "0.1.0-dev"

func main() {
	var (
		httpAddr     = flag.String("http-addr", ":8080", "Poe HTTP listen address")
		agentCmd     = flag.String("agent-cmd", "fir --mode acp", "ACP agent command (stdio)")
		agentDirFlag = flag.String("agent-dir", "", "FIR_AGENT_DIR passed to the child agent (default: inherit)")
		stateDirFlag = flag.String("state-dir", "", "Per-conv state dir root (default: $XDG_STATE_HOME/poe-acp-relay)")
		permission   = flag.String("permission", "allow-all", "Permission policy: allow-all|read-only|deny-all")
		accessKeyEnv = flag.String("access-key-env", "POEACP_ACCESS_KEY", "Env var holding the Poe bearer secret")
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

	// Router
	rtr, err := router.New(router.Config{
		Agent:      agent,
		StateDir:   stateDir,
		SessionTTL: *ttl,
	})
	if err != nil {
		log.Fatalf("router: %v", err)
	}
	stopGC := rtr.RunGC(ctx, *gcEvery)
	defer stopGC()

	// HTTP
	h := httpsrv.New(httpsrv.Config{
		Router: rtr,
		Settings: poeproto.SettingsResponse{
			AllowAttachments:    false,
			IntroductionMessage: *introMsg,
		},
		HeartbeatInterval: *heartbeat,
		CommandsProvider: func() []string {
			cmds := agent.Commands()
			names := make([]string, 0, len(cmds))
			for _, c := range cmds {
				names = append(names, c.Name)
			}
			return names
		},
	})

	mux := http.NewServeMux()
	mux.Handle("/poe", poeproto.BearerAuth(secret, h))
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

func defaultStateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "poe-acp-relay")
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".local", "state", "poe-acp-relay")
	}
	return filepath.Join(os.TempDir(), "poe-acp-relay")
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

// Command poe-acp-relay is a Poe server-bot that drives ACP agents (e.g.
// fir --mode acp) as a pure ACP client. See docs/poe-acp-relay/DESIGN.md.
//
// Usage:
//
//	poe-acp-relay [flags]
//
// This is the M0 skeleton: wiring compiles and the binary starts, but the
// Poe HTTP frontend is only a stub until end-to-end M1/M2 work lands.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kfet/fir/internal/poeacp"
	"github.com/kfet/fir/internal/poeacp/acpclient"
	"github.com/kfet/fir/internal/poeacp/httpsrv"
	"github.com/kfet/fir/internal/poeacp/poeproto"
	"github.com/kfet/fir/internal/poeacp/policy"
	"github.com/kfet/fir/internal/poeacp/router"
)

func main() {
	var (
		httpAddr    = flag.String("http-addr", ":8080", "Poe HTTP listen address")
		agentCmd    = flag.String("agent-cmd", "fir --mode acp", "ACP agent command (stdio)")
		agentCwd    = flag.String("agent-cwd", "", "Working directory for the agent (default: TMPDIR)")
		permission  = flag.String("permission", "allow-all", "Permission policy: allow-all|read-only|deny-all")
		accessKey   = flag.String("access-key-env", "POEACP_ACCESS_KEY", "Env var holding the Poe bearer secret")
		showVersion = flag.Bool("version", false, "Print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(poeacp.Version)
		return
	}

	log.SetOutput(os.Stderr)
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.Printf("poe-acp-relay %s starting", poeacp.Version)

	pol, err := policy.Parse(*permission)
	if err != nil {
		log.Fatalf("policy: %v", err)
	}

	secret := os.Getenv(*accessKey)
	if secret == "" {
		log.Fatalf("missing $%s (Poe bearer secret)", *accessKey)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("shutdown signal received")
		cancel()
	}()

	argv := strings.Fields(*agentCmd)
	agent, err := acpclient.Start(ctx, acpclient.Config{
		Command: argv,
		Cwd:     *agentCwd,
		Policy:  pol,
	})
	if err != nil {
		log.Fatalf("start agent: %v", err)
	}
	defer agent.Close()
	log.Printf("agent started: %s", *agentCmd)

	r := router.New(agent, *agentCwd)
	handler := &httpsrv.Handler{
		Router: r,
		Settings: poeproto.SettingsResponse{
			AllowAttachments:    false,
			IntroductionMessage: "poe-acp-relay: ACP-backed bot.",
		},
	}

	mux := http.NewServeMux()
	mux.Handle("/poe", poeproto.BearerAuth(secret, handler))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "ok sessions=%d\n", r.Len())
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

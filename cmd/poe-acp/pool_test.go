package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
	"github.com/kfet/acp-kit/mcphost"
	"github.com/kfet/acp-kit/remotefs"
	"github.com/kfet/poe-acp/internal/agentpool"
	"github.com/kfet/poe-acp/internal/config"
	"github.com/kfet/poe-acp/internal/poemcp"
)

// stubAgentEnv makes a child invocation of this test binary serve ACP on
// stdio instead of running the test suite, so the pooled-agent start
// path can be exercised against a REAL *client.AgentProc rather than a
// fake — the handshake and the model probe are the parts that go wrong.
// stubNoModelsEnv makes it answer session/new but reject the model
// probe, which is the "host whose catalog we cannot read" case.
const (
	stubAgentEnv    = "POE_ACP_STUB_POOLED_AGENT"
	stubNoModelsEnv = "POE_ACP_STUB_NO_MODELS"
)

func TestMain(m *testing.M) {
	if os.Getenv(stubAgentEnv) == "1" {
		runStubAgent()
		return
	}
	os.Exit(m.Run())
}

// runStubAgent is a minimal ACP agent: it handshakes, accepts a session,
// and either advertises one model or refuses the probe.
func runStubAgent() {
	handler := func(_ context.Context, method string, _ json.RawMessage) (any, *acp.RequestError) {
		switch method {
		case acp.AgentMethodInitialize:
			return map[string]any{
				"protocolVersion":   acp.ProtocolVersionNumber,
				"agentCapabilities": map[string]any{},
			}, nil
		case acp.AgentMethodSessionNew:
			if os.Getenv(stubNoModelsEnv) == "1" {
				return nil, acp.NewInternalError(errors.New("no provider configured"))
			}
			return map[string]any{
				"sessionId": "stub-sess",
				"models": map[string]any{
					"availableModels": []any{map[string]any{"modelId": "m1", "name": "Model One"}},
					"currentModelId":  "m1",
				},
			}, nil
		}
		return nil, acp.NewMethodNotFound(method)
	}
	_ = acp.NewConnection(handler, os.Stdout, os.Stdin)
	select {} // the parent kills us
}

func TestHostSpecs(t *testing.T) {
	t.Parallel()
	specs, err := hostSpecs([]config.Host{
		{Value: "local", Name: "here"},
		{Value: "boxy"}, // hint host: served by the default agent
		{Value: "miki", AgentCmd: "ssh -T miki .local/bin/fir --mode acp"},
		{Value: "beta", AgentCmd: "fir --mode acp", SSHHost: config.LocalHost},
	})
	if err != nil {
		t.Fatalf("hostSpecs: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("specs = %#v, want only the two pooled hosts", specs)
	}
	miki := specs["miki"]
	if got := strings.Join(miki.Argv, " "); got != "ssh -T miki .local/bin/fir --mode acp" {
		t.Fatalf("miki argv = %q", got)
	}
	if miki.Prov == nil {
		t.Fatal("miki must provision over ssh")
	}
	if specs["beta"].Prov != nil {
		t.Fatal("an ssh_host of \"local\" means the relay's own filesystem")
	}
}

func TestHostSpecs_NoneWhenNothingIsPooled(t *testing.T) {
	t.Parallel()
	specs, err := hostSpecs([]config.Host{{Value: "local"}, {Value: "boxy"}})
	if err != nil || specs != nil {
		t.Fatalf("specs = %#v err = %v, want no pool at all", specs, err)
	}
}

func TestHostSpecs_BadSSHDestination(t *testing.T) {
	t.Parallel()
	_, err := hostSpecs([]config.Host{{Value: "miki", AgentCmd: "fir --mode acp", SSHHost: "bad host"}})
	if err == nil || !strings.Contains(err.Error(), "ssh_host") {
		t.Fatalf("err = %v", err)
	}
}

func TestRemoteHostSpec(t *testing.T) {
	t.Parallel()
	hosts := []config.Host{
		{Value: "miki", AgentCmd: "ssh -T miki fir --mode acp"},
		{Value: "beta", AgentCmd: "fir --mode acp", SSHHost: config.LocalHost},
	}
	if !remoteHostSpec(hosts, "miki") {
		t.Error("miki runs its agent on another machine")
	}
	if remoteHostSpec(hosts, "beta") {
		t.Error("beta's agent shares the relay's filesystem")
	}
	if remoteHostSpec(hosts, "nope") {
		t.Error("unknown host must not be reported remote")
	}
}

// agentTargetFunc must translate the pool's three answers: a pooled
// host, "not mine" (which means the default target), and a failure.
func TestAgentTargetFunc(t *testing.T) {
	t.Parallel()
	pool, err := agentpool.New(agentpool.Config[*client.AgentProc]{
		Specs: map[string]agentpool.Spec{"broken": {Argv: []string{"fir"}}},
		Start: func(agentpool.Spec) (*client.AgentProc, error) {
			return nil, errors.New("ssh: host unreachable")
		},
		Logf: t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	fn := agentTargetFunc(pool)

	// Not pooled: the zero target tells the router to use its default.
	tgt, err := fn(t.Context(), "boxy")
	if err != nil || tgt.Agent != nil || tgt.Pooled {
		t.Fatalf("target = %#v err = %v, want the zero target", tgt, err)
	}
	// A host whose agent will not start surfaces the failure.
	if _, err := fn(t.Context(), "broken"); err == nil ||
		!strings.Contains(err.Error(), "host unreachable") {
		t.Fatalf("err = %v", err)
	}
}

// The success path, against a real agent process: a pooled host's
// target carries its own agent and its own provisioner, and the router
// is told to skip the `_meta.host` hint.
func TestAgentTargetFunc_PooledHostStartsARealAgent(t *testing.T) {
	t.Parallel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	ssh, err := remotefs.New("miki")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	template := client.Config{Env: append(os.Environ(), stubAgentEnv+"=1"), Stderr: io.Discard}
	pool, err := agentpool.New(agentpool.Config[*client.AgentProc]{
		Specs: map[string]agentpool.Spec{"miki": {Argv: []string{exe}, Prov: ssh}},
		Start: func(spec agentpool.Spec) (*client.AgentProc, error) {
			return startPooledAgent(ctx, template, spec, nil)
		},
		Logf: t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	tgt, err := agentTargetFunc(pool)(ctx, "miki")
	if err != nil {
		t.Fatalf("resolve pooled host: %v", err)
	}
	if tgt.Agent == nil || !tgt.Pooled || tgt.Prov != ssh {
		t.Fatalf("target = %#v", tgt)
	}
	models, current := tgt.Agent.(*client.AgentProc).Models()
	if len(models) != 1 || current != "m1" {
		t.Fatalf("models = %v current = %q, want the probed catalog", models, current)
	}
}

// A host whose catalog cannot be read is NOT a startup failure: the
// relay's schema comes from the default agent, and a model this host
// lacks simply fails that one set_model.
func TestStartPooledAgent_ProbeFailureIsNotFatal(t *testing.T) {
	t.Parallel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	template := client.Config{
		Env:    append(os.Environ(), stubAgentEnv+"=1", stubNoModelsEnv+"=1"),
		Stderr: io.Discard,
	}
	agent, err := startPooledAgent(ctx, template, agentpool.Spec{Host: "miki", Argv: []string{exe}}, nil)
	if err != nil {
		t.Fatalf("startPooledAgent: %v", err)
	}
	defer func() { _ = agent.Close() }()
	if models, _ := agent.Models(); len(models) != 0 {
		t.Fatalf("models = %v, want none", models)
	}
}

// A command that cannot be executed at all is reported, not swallowed.
func TestStartPooledAgent_SpawnFailure(t *testing.T) {
	t.Parallel()
	_, err := startPooledAgent(t.Context(), client.Config{Stderr: io.Discard},
		agentpool.Spec{Host: "miki", Argv: []string{"/nonexistent/agent-binary"}}, nil)
	if err == nil {
		t.Fatal("want a spawn error")
	}
}

// The `poe` MCP server is reached through a unix socket in this
// process's runtime dir, so it is offered only to a pooled agent that
// shares this machine's filesystem. A remote one would get a tool
// surface that fails on every call.
func TestPooledMCPServers(t *testing.T) {
	t.Parallel()
	if got := pooledMCPServers(nil, agentpool.Spec{}); got != nil {
		t.Error("no MCP host configured, want no server list")
	}
	host, err := mcphost.New(poemcp.HostConfig())
	if err != nil {
		t.Fatalf("mcphost.New: %v", err)
	}
	defer host.Close()

	ssh, err := remotefs.New("miki")
	if err != nil {
		t.Fatal(err)
	}
	if got := pooledMCPServers(host, agentpool.Spec{Host: "miki", Prov: ssh}); got != nil {
		t.Error("a remote pooled agent must not be handed the local socket")
	}
	fn := pooledMCPServers(host, agentpool.Spec{Host: "beta"})
	if fn == nil {
		t.Fatal("a local pooled agent must get the poe MCP server")
	}
	servers := fn("/state/convs/c1")
	if len(servers) != 1 || servers[0].Stdio == nil || servers[0].Stdio.Name != poemcp.ServerName {
		t.Fatalf("servers = %#v", servers)
	}
}

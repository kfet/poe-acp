// Package acpclient wraps acp-go-sdk's ClientSideConnection for use by the
// Poe relay. It manages a single stdio child agent process (e.g. fir --mode
// acp) and implements the acp.Client interface so the agent can issue
// server-initiated calls back into the relay (session updates, permission
// requests, fs reads/writes).
//
// This is a skeleton; it compiles and exposes the shape of the final API,
// but does not yet drive a real agent end-to-end. See M1 milestone in
// docs/poe-acp-relay/DESIGN.md.
package acpclient

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	acp "github.com/coder/acp-go-sdk"
)

// SessionUpdateSink receives streaming updates for a single ACP session.
// The router implements this and routes updates to the corresponding open
// Poe SSE response.
type SessionUpdateSink interface {
	OnUpdate(ctx context.Context, n acp.SessionNotification) error
}

// PermissionPolicy decides how to respond to session/request_permission.
type PermissionPolicy interface {
	Decide(ctx context.Context, req acp.RequestPermissionRequest) acp.RequestPermissionResponse
}

// Config configures an AgentProc.
type Config struct {
	// Command is the argv used to spawn the agent (e.g. []string{"fir", "--mode", "acp"}).
	Command []string
	// Cwd is the working directory for the child process.
	Cwd string
	// Env is the environment for the child. If nil, os.Environ() is used.
	Env []string
	// Policy decides permission responses.
	Policy PermissionPolicy
	// Stderr is where the child's stderr is forwarded. If nil, os.Stderr.
	Stderr io.Writer
}

// AgentProc wraps a single stdio-connected ACP agent process and the ACP
// client-side connection driving it.
type AgentProc struct {
	cfg Config

	cmd  *exec.Cmd
	conn *acp.ClientSideConnection

	mu    sync.Mutex
	sinks map[acp.SessionId]SessionUpdateSink // active session sinks
}

// Start launches the agent process, performs Initialize, and returns a
// ready-to-use AgentProc.
func Start(ctx context.Context, cfg Config) (*AgentProc, error) {
	if len(cfg.Command) == 0 {
		return nil, fmt.Errorf("acpclient: empty Command")
	}
	if cfg.Policy == nil {
		return nil, fmt.Errorf("acpclient: nil Policy")
	}
	if cfg.Cwd == "" {
		cfg.Cwd = os.TempDir()
	}

	cmd := exec.CommandContext(ctx, cfg.Command[0], cfg.Command[1:]...) //nolint:gosec // user-configured command
	cmd.Dir = cfg.Cwd
	if cfg.Env != nil {
		cmd.Env = cfg.Env
	}
	if cfg.Stderr != nil {
		cmd.Stderr = cfg.Stderr
	} else {
		cmd.Stderr = os.Stderr
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start agent: %w", err)
	}

	a := &AgentProc{
		cfg:   cfg,
		cmd:   cmd,
		sinks: make(map[acp.SessionId]SessionUpdateSink),
	}
	a.conn = acp.NewClientSideConnection(a, stdin, stdout)

	if _, err := a.conn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientCapabilities: acp.ClientCapabilities{
			Fs:       acp.FileSystemCapability{ReadTextFile: true, WriteTextFile: true},
			Terminal: false,
		},
	}); err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("acp initialize: %w", err)
	}
	return a, nil
}

// NewSession creates a new ACP session and wires the given sink to receive
// its updates. Returns the ACP session id.
func (a *AgentProc) NewSession(ctx context.Context, cwd string, sink SessionUpdateSink) (acp.SessionId, error) {
	resp, err := a.conn.NewSession(ctx, acp.NewSessionRequest{
		Cwd:        cwd,
		McpServers: []acp.McpServer{},
	})
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	a.sinks[resp.SessionId] = sink
	a.mu.Unlock()
	return resp.SessionId, nil
}

// Prompt sends a user message to the session. Returns the stop reason.
func (a *AgentProc) Prompt(ctx context.Context, sid acp.SessionId, text string) (acp.StopReason, error) {
	resp, err := a.conn.Prompt(ctx, acp.PromptRequest{
		SessionId: sid,
		Prompt:    []acp.ContentBlock{acp.TextBlock(text)},
	})
	if err != nil {
		return "", err
	}
	return resp.StopReason, nil
}

// Cancel requests cancellation of an in-flight prompt for a session.
func (a *AgentProc) Cancel(ctx context.Context, sid acp.SessionId) error {
	return a.conn.Cancel(ctx, acp.CancelNotification{SessionId: sid})
}

// Close terminates the agent process.
func (a *AgentProc) Close() error {
	if a.cmd != nil && a.cmd.Process != nil {
		_ = a.cmd.Process.Kill()
		_ = a.cmd.Wait()
	}
	return nil
}

// ---- acp.Client implementation (server-initiated calls from the agent) ----

var _ acp.Client = (*AgentProc)(nil)

func (a *AgentProc) sinkFor(sid acp.SessionId) SessionUpdateSink {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sinks[sid]
}

// SessionUpdate fans out to the per-session sink.
func (a *AgentProc) SessionUpdate(ctx context.Context, params acp.SessionNotification) error {
	if s := a.sinkFor(params.SessionId); s != nil {
		return s.OnUpdate(ctx, params)
	}
	return nil
}

// RequestPermission delegates to the configured policy.
func (a *AgentProc) RequestPermission(ctx context.Context, params acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	return a.cfg.Policy.Decide(ctx, params), nil
}

// ReadTextFile reads from disk relative to the agent's cwd.
func (a *AgentProc) ReadTextFile(ctx context.Context, params acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	if !filepath.IsAbs(params.Path) {
		return acp.ReadTextFileResponse{}, fmt.Errorf("path must be absolute: %s", params.Path)
	}
	b, err := os.ReadFile(params.Path) //nolint:gosec // path is agent-driven within its own cwd
	if err != nil {
		return acp.ReadTextFileResponse{}, err
	}
	content := string(b)
	if params.Line != nil || params.Limit != nil {
		lines := strings.Split(content, "\n")
		start := 0
		if params.Line != nil && *params.Line > 0 {
			start = *params.Line - 1
			if start > len(lines) {
				start = len(lines)
			}
		}
		end := len(lines)
		if params.Limit != nil && *params.Limit > 0 && start+*params.Limit < end {
			end = start + *params.Limit
		}
		content = strings.Join(lines[start:end], "\n")
	}
	return acp.ReadTextFileResponse{Content: content}, nil
}

// WriteTextFile writes to disk. Skeleton: gated to agent cwd in the future.
func (a *AgentProc) WriteTextFile(ctx context.Context, params acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	if !filepath.IsAbs(params.Path) {
		return acp.WriteTextFileResponse{}, fmt.Errorf("path must be absolute: %s", params.Path)
	}
	if err := os.MkdirAll(filepath.Dir(params.Path), 0o755); err != nil {
		return acp.WriteTextFileResponse{}, err
	}
	return acp.WriteTextFileResponse{}, os.WriteFile(params.Path, []byte(params.Content), 0o644) //nolint:gosec // ditto
}

// Terminal methods — not used by the relay (Terminal capability advertised as false).

func (a *AgentProc) CreateTerminal(ctx context.Context, params acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, fmt.Errorf("terminal not supported")
}
func (a *AgentProc) TerminalOutput(ctx context.Context, params acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, fmt.Errorf("terminal not supported")
}
func (a *AgentProc) ReleaseTerminal(ctx context.Context, params acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, fmt.Errorf("terminal not supported")
}
func (a *AgentProc) WaitForTerminalExit(ctx context.Context, params acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, fmt.Errorf("terminal not supported")
}
func (a *AgentProc) KillTerminalCommand(ctx context.Context, params acp.KillTerminalCommandRequest) (acp.KillTerminalCommandResponse, error) {
	return acp.KillTerminalCommandResponse{}, fmt.Errorf("terminal not supported")
}

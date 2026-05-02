// Package acpclient wraps acp-go-sdk's low-level Connection for use by the
// Poe relay. It manages a single stdio child agent process (e.g. fir --mode
// acp) and dispatches inbound (server-initiated) ACP calls — session
// updates, permission requests, fs reads/writes — back to the relay.
//
// One AgentProc runs one ACP child process. It can serve many sessions
// concurrently — each NewSession/ResumeSession registers a per-session
// sink that receives the stream of session/update notifications.
//
// We talk to acp.Connection directly (rather than acp.ClientSideConnection)
// so we can issue the unstable session/list and session/resume methods that
// the SDK doesn't model. The standard methods are sent via acp.SendRequest
// with the SDK's typed request/response structs.
//
// Security: the fs methods (ReadTextFile / WriteTextFile) currently do not
// sandbox paths to the session's cwd. That is adequate for the v1 use case
// (one trusted agent binary per relay process) but should be tightened
// before exposing the relay to untrusted agents. See DESIGN.md "Future".
package acpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

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

// Caps captures the agent capabilities the relay cares about, parsed
// from the initialize response.
type Caps struct {
	// LoadSession is the standard agentCapabilities.loadSession bool.
	LoadSession bool
	// ListSessions reflects agentCapabilities.sessionCapabilities.list
	// (unstable RFD).
	ListSessions bool
	// ResumeSession reflects agentCapabilities.sessionCapabilities.resume
	// (unstable RFD).
	ResumeSession bool
	// EmbeddedContext reflects
	// agentCapabilities.promptCapabilities.embeddedContext: when true,
	// the relay may emit ContentBlock::Resource (with TextResourceContents)
	// in prompt requests instead of a bare ResourceLink, avoiding an
	// agent-side fetch.
	EmbeddedContext bool
}

// SessionInfo is one entry from a session/list response.
type SessionInfo struct {
	SessionId string  `json:"sessionId"`
	Cwd       string  `json:"cwd,omitempty"`
	Title     *string `json:"title,omitempty"`
	UpdatedAt string  `json:"updatedAt,omitempty"`
}

// listSessionsRequest mirrors the unstable RFD for session/list.
type listSessionsRequest struct {
	Cwd string `json:"cwd,omitempty"`
}

type listSessionsResponse struct {
	Sessions []SessionInfo `json:"sessions"`
}

// resumeSessionRequest mirrors the unstable RFD for session/resume.
type resumeSessionRequest struct {
	SessionId  string          `json:"sessionId"`
	Cwd        string          `json:"cwd,omitempty"`
	McpServers []acp.McpServer `json:"mcpServers,omitempty"`
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
// connection driving it.
type AgentProc struct {
	cfg Config

	cmd  *exec.Cmd
	conn *acp.Connection
	caps Caps

	mu     sync.Mutex
	sinks  map[acp.SessionId]SessionUpdateSink // active session sinks
	models *acp.SessionModelState              // cached model list (nil until first NewSession or Probe)

	authMethods []AuthMethod // parsed from initialize response
}

// Start launches the agent process, performs Initialize (capturing caps),
// and returns a ready-to-use AgentProc.
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
	a.conn = acp.NewConnection(a.dispatch, stdin, stdout)

	// Use a raw map for the response so we can read the unstable
	// sessionCapabilities sub-object that the SDK's typed struct drops.
	initParams := acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientCapabilities: acp.ClientCapabilities{
			Fs:       acp.FileSystemCapability{ReadTextFile: true, WriteTextFile: true},
			Terminal: false,
		},
	}
	raw, err := acp.SendRequest[json.RawMessage](a.conn, ctx, acp.AgentMethodInitialize, initParams)
	if err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("acp initialize: %w", err)
	}
	a.caps = parseCaps(raw)
	a.authMethods = parseAuthMethods(raw)
	return a, nil
}

// parseAuthMethods extracts the authMethods array from a raw initialize
// response. Reads only the fields the relay actually uses; extra _meta
// is ignored.
func parseAuthMethods(raw json.RawMessage) []AuthMethod {
	var env struct {
		AuthMethods []AuthMethod `json:"authMethods"`
	}
	_ = json.Unmarshal(raw, &env)
	return env.AuthMethods
}

// parseCaps extracts agentCapabilities.{loadSession,sessionCapabilities.{list,resume}}
// from a raw initialize response. Missing fields default to false.
func parseCaps(raw json.RawMessage) Caps {
	var env struct {
		AgentCapabilities struct {
			LoadSession         bool `json:"loadSession"`
			SessionCapabilities struct {
				List   *json.RawMessage `json:"list"`
				Resume *json.RawMessage `json:"resume"`
			} `json:"sessionCapabilities"`
			PromptCapabilities struct {
				EmbeddedContext bool `json:"embeddedContext"`
			} `json:"promptCapabilities"`
		} `json:"agentCapabilities"`
	}
	_ = json.Unmarshal(raw, &env)
	return Caps{
		LoadSession:     env.AgentCapabilities.LoadSession,
		ListSessions:    env.AgentCapabilities.SessionCapabilities.List != nil,
		ResumeSession:   env.AgentCapabilities.SessionCapabilities.Resume != nil,
		EmbeddedContext: env.AgentCapabilities.PromptCapabilities.EmbeddedContext,
	}
}

// Caps returns the agent's advertised capabilities (parsed at Initialize).
func (a *AgentProc) Caps() Caps { return a.caps }

// NewSession creates a new ACP session and wires the given sink to receive
// its updates. Returns the ACP session id.
func (a *AgentProc) NewSession(ctx context.Context, cwd string, sink SessionUpdateSink) (acp.SessionId, error) {
	resp, err := acp.SendRequest[acp.NewSessionResponse](a.conn, ctx, acp.AgentMethodSessionNew, acp.NewSessionRequest{
		Cwd:        cwd,
		McpServers: []acp.McpServer{},
	})
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	a.sinks[resp.SessionId] = sink
	if resp.Models != nil {
		a.models = resp.Models
	}
	a.mu.Unlock()
	return resp.SessionId, nil
}

// SetModel calls the unstable session/set_model RPC.
func (a *AgentProc) SetModel(ctx context.Context, sid acp.SessionId, modelID string) error {
	_, err := acp.SendRequest[acp.SetSessionModelResponse](a.conn, ctx, acp.AgentMethodSessionSetModel, acp.SetSessionModelRequest{
		SessionId: sid,
		ModelId:   acp.ModelId(modelID),
	})
	return err
}

// SetConfigOption calls the (fir-specific) session/set_config_option RPC.
// Used for thinking_level and similar dropdown-style knobs.
type setSessionConfigOptionRequest struct {
	SessionId acp.SessionId `json:"sessionId"`
	ConfigId  string        `json:"configId"`
	Value     string        `json:"value"`
}

func (a *AgentProc) SetConfigOption(ctx context.Context, sid acp.SessionId, configID, value string) error {
	_, err := acp.SendRequest[json.RawMessage](a.conn, ctx, "session/set_config_option", setSessionConfigOptionRequest{
		SessionId: sid,
		ConfigId:  configID,
		Value:     value,
	})
	return err
}

// ModelInfo is one entry in the agent's available-models list.
type ModelInfo struct {
	ID   string // "provider/modelID"
	Name string // human-readable label
}

// Models returns a snapshot of the agent's last-seen available models.
// Empty until a session has been created (or ProbeModels has run).
func (a *AgentProc) Models() (models []ModelInfo, currentID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.models == nil {
		return nil, ""
	}
	out := make([]ModelInfo, 0, len(a.models.AvailableModels))
	for _, m := range a.models.AvailableModels {
		out = append(out, ModelInfo{ID: string(m.ModelId), Name: string(m.Name)})
	}
	return out, string(a.models.CurrentModelId)
}

// ProbeModels creates a throwaway session in the agent's cwd to read its
// available-models list, then cancels it. The cached snapshot is returned
// from Models() afterwards. Idempotent: a no-op if Models() already has
// a list.
func (a *AgentProc) ProbeModels(ctx context.Context) error {
	a.mu.Lock()
	if a.models != nil {
		a.mu.Unlock()
		return nil
	}
	a.mu.Unlock()

	probeCwd, err := os.MkdirTemp("", "poeacp-probe-*")
	if err != nil {
		return fmt.Errorf("probe: mkdir tmp: %w", err)
	}
	defer os.RemoveAll(probeCwd)

	// Use a noop sink — we don't care about updates.
	sid, err := a.NewSession(ctx, probeCwd, noopSink{})
	if err != nil {
		return fmt.Errorf("probe: new session: %w", err)
	}
	// Drop the sink; the probe session is never prompted so it stays
	// idle in the agent for the relay's lifetime (no session/delete RPC
	// exists). Cost: one map entry in fir's sessions map.
	a.mu.Lock()
	delete(a.sinks, sid)
	a.mu.Unlock()
	return nil
}

type noopSink struct{}

func (noopSink) OnUpdate(context.Context, acp.SessionNotification) error { return nil }

// ListSessions calls the unstable session/list. Caller must check Caps().ListSessions first.
func (a *AgentProc) ListSessions(ctx context.Context, cwd string) ([]SessionInfo, error) {
	resp, err := acp.SendRequest[listSessionsResponse](a.conn, ctx, "session/list", listSessionsRequest{Cwd: cwd})
	if err != nil {
		return nil, err
	}
	return resp.Sessions, nil
}

// ResumeSession calls the unstable session/resume and registers the sink
// for the resumed session. Caller must check Caps().ResumeSession first.
// The given sid is the agent-returned identifier (as listed by ListSessions).
func (a *AgentProc) ResumeSession(ctx context.Context, cwd string, sid acp.SessionId, sink SessionUpdateSink) error {
	_, err := acp.SendRequest[json.RawMessage](a.conn, ctx, "session/resume", resumeSessionRequest{
		SessionId:  string(sid),
		Cwd:        cwd,
		McpServers: []acp.McpServer{},
	})
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.sinks[sid] = sink
	a.mu.Unlock()
	return nil
}

// Prompt sends a user message to the session. Returns the stop reason.
// The prompt is a sequence of ACP content blocks; callers build these
// from the latest user text plus any attachments.
func (a *AgentProc) Prompt(ctx context.Context, sid acp.SessionId, prompt []acp.ContentBlock) (acp.StopReason, error) {
	resp, err := acp.SendRequest[acp.PromptResponse](a.conn, ctx, acp.AgentMethodSessionPrompt, acp.PromptRequest{
		SessionId: sid,
		Prompt:    prompt,
	})
	if err != nil {
		return "", err
	}
	return resp.StopReason, nil
}

// Cancel requests cancellation of an in-flight prompt for a session.
func (a *AgentProc) Cancel(ctx context.Context, sid acp.SessionId) error {
	return a.conn.SendNotification(ctx, acp.AgentMethodSessionCancel, acp.CancelNotification{SessionId: sid})
}

// AuthMethod describes one authentication method advertised by the agent
// in the initialize response. Mirrors the RFD auth-methods schema we care
// about. Extra _meta fields the relay doesn't use are ignored.
type AuthMethod struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Type        string `json:"type,omitempty"` // "agent" | "env_var" | "terminal" | ""
}

// AuthResult is the outcome of an Authenticate call.
type AuthResult struct {
	// State is one of "needs_redirect", "ok", "cancelled", or "" if the
	// agent's response carried no _meta.auth state field (legacy / non-
	// interactive responses).
	State string
	// ID is the opaque pending-login id the agent returns on call 1.
	// Echo it back on call 2 / cancel so the agent can disambiguate
	// concurrent pending logins for the same methodID.
	ID string
	// URL is the auth URL the user should visit (state="needs_redirect").
	URL string
	// Instructions is optional human-readable text alongside URL.
	Instructions string
}

// Authenticate invokes the ACP authenticate RPC. Modes:
//
//   - id == "" && redirect == "" && !cancel : start an interactive login.
//     The agent returns a fresh id (and URL) in AuthResult.
//   - id != "" && redirect != ""            : submit the pasted redirect.
//   - id != "" && cancel == true            : cancel that pending login.
//
// methodID must match the id advertised in the initialize response (e.g.
// "oauth-anthropic"). Requires an agent that supports the
// _meta.auth.interactive extension; older agents will run the legacy
// blocking flow and may return an empty AuthResult.
func (a *AgentProc) Authenticate(ctx context.Context, methodID, id, redirect string, cancel bool) (AuthResult, error) {
	authMeta := map[string]any{"interactive": true}
	if id != "" {
		authMeta["id"] = id
	}
	if redirect != "" {
		authMeta["redirect"] = redirect
	}
	if cancel {
		authMeta["cancel"] = true
	}
	params := map[string]any{
		"methodId": methodID,
		"_meta":    map[string]any{"auth": authMeta},
	}
	raw, err := acp.SendRequest[json.RawMessage](a.conn, ctx, acp.AgentMethodAuthenticate, params)
	if err != nil {
		return AuthResult{}, err
	}
	return parseAuthResult(raw), nil
}

// parseAuthResult extracts state/id/url/instructions from a raw authenticate
// response. Missing fields → zero values.
func parseAuthResult(raw json.RawMessage) AuthResult {
	var env struct {
		Meta struct {
			Auth struct {
				State        string `json:"state"`
				ID           string `json:"id"`
				URL          string `json:"url"`
				Instructions string `json:"instructions"`
			} `json:"auth"`
		} `json:"_meta"`
	}
	_ = json.Unmarshal(raw, &env)
	return AuthResult{
		State:        env.Meta.Auth.State,
		ID:           env.Meta.Auth.ID,
		URL:          env.Meta.Auth.URL,
		Instructions: env.Meta.Auth.Instructions,
	}
}

// AuthMethods returns the auth methods the agent advertised at Initialize.
// Empty if the agent didn't advertise any (or initialize hasn't run yet).
func (a *AgentProc) AuthMethods() []AuthMethod {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]AuthMethod, len(a.authMethods))
	copy(out, a.authMethods)
	return out
}

// Close terminates the agent process. Returns after the process has
// exited (or been force-killed).
func (a *AgentProc) Close() error {
	if a.cmd == nil || a.cmd.Process == nil {
		return nil
	}
	// Try a gentle stop first; fall through to Kill after a short grace.
	_ = a.cmd.Process.Signal(os.Interrupt)
	done := make(chan error, 1)
	go func() { done <- a.cmd.Wait() }()
	select {
	case <-done:
		return nil
	case <-time.After(2 * time.Second):
		_ = a.cmd.Process.Kill()
		<-done
		return nil
	}
}

// ---- Inbound dispatch (server-initiated calls from the agent) ----

// dispatch routes inbound JSON-RPC requests to the appropriate handler.
// Mirrors the SDK's ClientSideConnection.handle but lives here so we can
// own the underlying Connection.
func (a *AgentProc) dispatch(ctx context.Context, method string, params json.RawMessage) (any, *acp.RequestError) {
	switch method {
	case acp.ClientMethodSessionUpdate:
		var p acp.SessionNotification
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, acp.NewInvalidParams(map[string]any{"error": err.Error()})
		}
		if err := a.sessionUpdate(ctx, p); err != nil {
			return nil, toReqErr(err)
		}
		return nil, nil
	case acp.ClientMethodSessionRequestPermission:
		var p acp.RequestPermissionRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, acp.NewInvalidParams(map[string]any{"error": err.Error()})
		}
		return a.cfg.Policy.Decide(ctx, p), nil
	case acp.ClientMethodFsReadTextFile:
		var p acp.ReadTextFileRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, acp.NewInvalidParams(map[string]any{"error": err.Error()})
		}
		resp, err := a.readTextFile(p)
		if err != nil {
			return nil, toReqErr(err)
		}
		return resp, nil
	case acp.ClientMethodFsWriteTextFile:
		var p acp.WriteTextFileRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, acp.NewInvalidParams(map[string]any{"error": err.Error()})
		}
		if err := a.writeTextFile(p); err != nil {
			return nil, toReqErr(err)
		}
		return acp.WriteTextFileResponse{}, nil
	default:
		// Terminal methods and any unknown call: we never advertised
		// the capability, so the agent shouldn't be calling these.
		return nil, acp.NewMethodNotFound(method)
	}
}

func toReqErr(err error) *acp.RequestError {
	return acp.NewInternalError(map[string]any{"error": err.Error()})
}

func (a *AgentProc) sinkFor(sid acp.SessionId) SessionUpdateSink {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sinks[sid]
}

// sessionUpdate fans out to the per-session sink.
func (a *AgentProc) sessionUpdate(ctx context.Context, params acp.SessionNotification) error {
	if s := a.sinkFor(params.SessionId); s != nil {
		return s.OnUpdate(ctx, params)
	}
	return nil
}

// readTextFile reads from disk relative to the agent's cwd.
func (a *AgentProc) readTextFile(params acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
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

// writeTextFile writes to disk. Skeleton: gated to agent cwd in the future.
func (a *AgentProc) writeTextFile(params acp.WriteTextFileRequest) error {
	if !filepath.IsAbs(params.Path) {
		return fmt.Errorf("path must be absolute: %s", params.Path)
	}
	if err := os.MkdirAll(filepath.Dir(params.Path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(params.Path, []byte(params.Content), 0o644) //nolint:gosec // ditto
}

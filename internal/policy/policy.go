// Package policy implements permission policies for session/request_permission
// calls issued by the ACP agent.
package policy

import (
	"context"
	"fmt"
	"strings"

	acp "github.com/coder/acp-go-sdk"
)

// Policy decides how to answer a permission request.
type Policy interface {
	Decide(ctx context.Context, req acp.RequestPermissionRequest) acp.RequestPermissionResponse
}

// Parse resolves a policy name (allow-all, read-only, deny-all) to an impl.
func Parse(name string) (Policy, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "allow-all", "allow":
		return AllowAll{}, nil
	case "read-only", "readonly":
		return ReadOnly{}, nil
	case "deny-all", "deny":
		return DenyAll{}, nil
	default:
		return nil, fmt.Errorf("unknown policy %q (want allow-all|read-only|deny-all)", name)
	}
}

// pick selects the first option that looks like "allow" / "deny"; falls back
// to the first option. Returns a Selected outcome.
func pick(req acp.RequestPermissionRequest, want string) acp.RequestPermissionResponse {
	var chosen acp.PermissionOptionId
	for _, o := range req.Options {
		n := strings.ToLower(o.Name)
		k := strings.ToLower(string(o.Kind))
		if strings.Contains(n, want) || strings.Contains(k, want) {
			chosen = o.OptionId
			break
		}
	}
	if chosen == "" && len(req.Options) > 0 {
		chosen = req.Options[0].OptionId
	}
	return acp.RequestPermissionResponse{
		Outcome: acp.RequestPermissionOutcome{
			Selected: &acp.RequestPermissionOutcomeSelected{OptionId: chosen},
		},
	}
}

// AllowAll approves every request by picking an "allow"-shaped option.
type AllowAll struct{}

func (AllowAll) Decide(_ context.Context, req acp.RequestPermissionRequest) acp.RequestPermissionResponse {
	return pick(req, "allow")
}

// DenyAll rejects every request by picking a "reject"-shaped option.
type DenyAll struct{}

func (DenyAll) Decide(_ context.Context, req acp.RequestPermissionRequest) acp.RequestPermissionResponse {
	return pick(req, "reject")
}

// ReadOnly allows read-like tool calls and rejects everything else. v1
// heuristic: inspect the tool call title for write/bash/edit verbs.
type ReadOnly struct{}

func (ReadOnly) Decide(_ context.Context, req acp.RequestPermissionRequest) acp.RequestPermissionResponse {
	title := ""
	if req.ToolCall.Title != nil {
		title = strings.ToLower(*req.ToolCall.Title)
	}
	writeish := []string{"write", "edit", "bash", "exec", "run", "delete", "rm "}
	for _, w := range writeish {
		if strings.Contains(title, w) {
			return pick(req, "reject")
		}
	}
	return pick(req, "allow")
}

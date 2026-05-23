package main

import (
	"context"
	"strings"
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

func TestParsePermission(t *testing.T) {
	allow := acp.PermissionOption{OptionId: "a", Name: "allow", Kind: "allow_once"}
	reject := acp.PermissionOption{OptionId: "r", Name: "reject", Kind: "reject_once"}
	title := "Write file foo"
	req := acp.RequestPermissionRequest{
		Options:  []acp.PermissionOption{allow, reject},
		ToolCall: acp.ToolCallUpdate{Title: &title},
	}

	// Each alias maps to the option id chosen for a write-shaped tool call:
	// allow-shaped names pick "a", reject/read-only-shaped pick "r".
	cases := map[string]string{
		"":          "a",
		"allow-all": "a",
		"allow":     "a",
		"read-only": "r",
		"readonly":  "r",
		"deny-all":  "r",
		"deny":      "r",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			p, err := parsePermission(in)
			if err != nil {
				t.Fatalf("parsePermission(%q): %v", in, err)
			}
			resp := p.Decide(context.Background(), req)
			got := ""
			if resp.Outcome.Selected != nil {
				got = string(resp.Outcome.Selected.OptionId)
			}
			if got != want {
				t.Fatalf("got %q want %q", got, want)
			}
		})
	}

	if _, err := parsePermission("bogus"); err == nil || !strings.Contains(err.Error(), "unknown policy") {
		t.Fatalf("bogus: err = %v", err)
	}
}

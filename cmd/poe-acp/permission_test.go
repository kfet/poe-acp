package main

import (
	"context"
	"strings"
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

func TestParsePermission(t *testing.T) {
	cases := []struct {
		in      string
		ok      bool
		want    string // option id chosen for a write-shaped tool call
		hasOpts bool
	}{
		{"", true, "a", true},
		{"allow-all", true, "a", true},
		{"allow", true, "a", true},
		{"read-only", true, "r", true}, // write-shaped title rejected
		{"readonly", true, "r", true},
		{"deny-all", true, "r", true},
		{"deny", true, "r", true},
	}
	allow := acp.PermissionOption{OptionId: "a", Name: "allow", Kind: "allow_once"}
	reject := acp.PermissionOption{OptionId: "r", Name: "reject", Kind: "reject_once"}
	title := "Write file foo"
	req := acp.RequestPermissionRequest{
		Options:  []acp.PermissionOption{allow, reject},
		ToolCall: acp.ToolCallUpdate{Title: &title},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			p, err := parsePermission(c.in)
			if (err == nil) != c.ok {
				t.Fatalf("err = %v, ok = %v", err, c.ok)
			}
			if err != nil {
				return
			}
			resp := p.Decide(context.Background(), req)
			got := ""
			if resp.Outcome.Selected != nil {
				got = string(resp.Outcome.Selected.OptionId)
			}
			if got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}

	_, err := parsePermission("bogus")
	if err == nil || !strings.Contains(err.Error(), "unknown policy") {
		t.Fatalf("bogus: err = %v", err)
	}
}

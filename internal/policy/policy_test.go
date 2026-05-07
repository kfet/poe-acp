package policy

import (
	"context"
	"strings"
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

func ptr[T any](v T) *T { return &v }

func opts(items ...string) []acp.PermissionOption {
	out := make([]acp.PermissionOption, len(items))
	for i, n := range items {
		out[i] = acp.PermissionOption{
			OptionId: acp.PermissionOptionId("id-" + n),
			Name:     n,
			Kind:     acp.PermissionOptionKind(strings.ToLower(n)),
		}
	}
	return out
}

func decide(p Policy, title string, names ...string) string {
	r := p.Decide(context.Background(), acp.RequestPermissionRequest{
		ToolCall: acp.RequestPermissionToolCall{Title: ptr(title)},
		Options:  opts(names...),
	})
	if r.Outcome.Selected == nil {
		return ""
	}
	return string(r.Outcome.Selected.OptionId)
}

func TestParse(t *testing.T) {
	for _, n := range []string{"", "allow-all", "ALLOW"} {
		p, err := Parse(n)
		if err != nil {
			t.Fatalf("Parse(%q) err: %v", n, err)
		}
		if _, ok := p.(AllowAll); !ok {
			t.Errorf("Parse(%q) = %T, want AllowAll", n, p)
		}
	}
	for _, n := range []string{"read-only", "ReadOnly"} {
		p, err := Parse(n)
		if err != nil {
			t.Fatalf("Parse(%q) err: %v", n, err)
		}
		if _, ok := p.(ReadOnly); !ok {
			t.Errorf("Parse(%q) = %T", n, p)
		}
	}
	for _, n := range []string{"deny-all", "DENY"} {
		p, err := Parse(n)
		if err != nil {
			t.Fatalf("Parse(%q) err: %v", n, err)
		}
		if _, ok := p.(DenyAll); !ok {
			t.Errorf("Parse(%q) = %T", n, p)
		}
	}
	if _, err := Parse("nope"); err == nil {
		t.Fatal("expected error")
	}
}

func TestAllowAll(t *testing.T) {
	if got := decide(AllowAll{}, "Read file", "Allow", "Reject"); got != "id-Allow" {
		t.Fatalf("got %q", got)
	}
	// Falls back to first option when none match "allow".
	if got := decide(AllowAll{}, "x", "Reject", "Other"); got != "id-Reject" {
		t.Fatalf("fallback got %q", got)
	}
	// Empty options => empty selected.
	r := AllowAll{}.Decide(context.Background(), acp.RequestPermissionRequest{})
	if r.Outcome.Selected == nil || r.Outcome.Selected.OptionId != "" {
		t.Fatalf("want empty option id, got %+v", r.Outcome.Selected)
	}
}

func TestDenyAll(t *testing.T) {
	if got := decide(DenyAll{}, "x", "Allow", "Reject"); got != "id-Reject" {
		t.Fatalf("got %q", got)
	}
}

func TestReadOnly(t *testing.T) {
	// Title with write-ish word => reject.
	for _, title := range []string{"Write file", "Edit foo", "Bash: ls", "exec it", "rm -rf", "delete x", "run x"} {
		if got := decide(ReadOnly{}, title, "Allow", "Reject"); got != "id-Reject" {
			t.Errorf("title=%q got %q want reject", title, got)
		}
	}
	// Read-ish or no title => allow.
	if got := decide(ReadOnly{}, "Read file", "Allow", "Reject"); got != "id-Allow" {
		t.Errorf("read got %q", got)
	}
	// Nil title.
	r := ReadOnly{}.Decide(context.Background(), acp.RequestPermissionRequest{Options: opts("Allow", "Reject")})
	if r.Outcome.Selected.OptionId != "id-Allow" {
		t.Errorf("nil title: got %v", r.Outcome.Selected.OptionId)
	}
}

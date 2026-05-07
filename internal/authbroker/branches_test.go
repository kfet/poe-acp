package authbroker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kfet/poe-acp/internal/acpclient"
)

func TestStart_AuthenticateError(t *testing.T) {
	f := newFake()
	f.err = errors.New("agent down")
	b := New(f)
	_, err := b.Handle(context.Background(), "c1", "/login anthropic")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestStart_CancelledState(t *testing.T) {
	f := newFake()
	f.res = acpclient.AuthResult{State: "cancelled"}
	b := New(f)
	out, _ := b.Handle(context.Background(), "c1", "/login anthropic")
	if !strings.Contains(out.Text, "cancelled") {
		t.Fatalf("got %q", out.Text)
	}
}

func TestStart_UnexpectedState(t *testing.T) {
	f := newFake()
	f.res = acpclient.AuthResult{State: "weird"}
	b := New(f)
	out, _ := b.Handle(context.Background(), "c1", "/login anthropic")
	if !strings.Contains(out.Text, "unexpected state") {
		t.Fatalf("got %q", out.Text)
	}
}

func TestStart_SecondConcurrentLogin(t *testing.T) {
	f := newFake()
	f.res = acpclient.AuthResult{State: "needs_redirect", URL: "https://x"}
	b := New(f)
	_, _ = b.Handle(context.Background(), "c1", "/login anthropic")
	// Force a /cancel-login? No - that would clear. Use a different conv? No we want same conv.
	// But pending captures next text as paste. So the only way to trigger
	// the "already in progress" branch is to inject pending state before start().
	// Manually call start directly with same conv id while pending exists for that conv
	// — but Handle would route through complete(). Instead, set pending for a *different*
	// methodID and call start with same conv via internal call.
	out, err := b.start(context.Background(), "c1", "anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Text, "already in progress") {
		t.Fatalf("got %q", out.Text)
	}
}

func TestStart_NeedsRedirectWithInstructions(t *testing.T) {
	f := newFake()
	f.res = acpclient.AuthResult{State: "needs_redirect", URL: "https://x", Instructions: "Do this"}
	b := New(f)
	out, _ := b.Handle(context.Background(), "c1", "/login anthropic")
	if !strings.Contains(out.Text, "Do this") {
		t.Fatalf("instructions missing: %q", out.Text)
	}
	if out.Instructions != "Do this" {
		t.Fatalf("Instructions field = %q", out.Instructions)
	}
}

func TestComplete_EmptyPaste(t *testing.T) {
	f := newFake()
	f.res = acpclient.AuthResult{State: "needs_redirect", URL: "https://x"}
	b := New(f)
	_, _ = b.Handle(context.Background(), "c1", "/login anthropic")
	// Whitespace-only paste is trimmed to "".
	out, err := b.Handle(context.Background(), "c1", "   ")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Text, "Empty paste") {
		t.Fatalf("got %q", out.Text)
	}
	// Pending must remain (so user can paste again).
	if !b.HasPending("c1") {
		t.Fatal("pending should remain after empty paste")
	}
}

func TestComplete_CancelledState(t *testing.T) {
	f := newFake()
	f.res = acpclient.AuthResult{State: "needs_redirect", URL: "https://x"}
	b := New(f)
	_, _ = b.Handle(context.Background(), "c1", "/login anthropic")
	f.res = acpclient.AuthResult{State: "cancelled"}
	out, _ := b.Handle(context.Background(), "c1", "paste")
	if !strings.Contains(out.Text, "cancelled") {
		t.Fatalf("got %q", out.Text)
	}
}

func TestComplete_UnexpectedState(t *testing.T) {
	f := newFake()
	f.res = acpclient.AuthResult{State: "needs_redirect", URL: "https://x"}
	b := New(f)
	_, _ = b.Handle(context.Background(), "c1", "/login anthropic")
	f.res = acpclient.AuthResult{State: "weird"}
	out, _ := b.Handle(context.Background(), "c1", "paste")
	if !strings.Contains(out.Text, "unexpected state") {
		t.Fatalf("got %q", out.Text)
	}
}

func TestCancel_AgentError(t *testing.T) {
	f := newFake()
	f.res = acpclient.AuthResult{State: "needs_redirect", URL: "https://x"}
	b := New(f)
	_, _ = b.Handle(context.Background(), "c1", "/login anthropic")
	f.err = errors.New("oops")
	out, _ := b.Handle(context.Background(), "c1", "/cancel-login")
	if !strings.Contains(out.Text, "agent reported") {
		t.Fatalf("got %q", out.Text)
	}
}

func TestList_WithDescription(t *testing.T) {
	f := &fakeAuth{methods: []acpclient.AuthMethod{
		{ID: "oauth-x", Name: "X", Description: "the desc", Type: "agent"},
	}}
	b := New(f)
	out := b.list()
	if !strings.Contains(out.Text, "the desc") {
		t.Fatalf("desc missing: %q", out.Text)
	}
}

func TestResolveMethodID_NoMethods(t *testing.T) {
	b := New(&fakeAuth{})
	if _, err := b.resolveMethodID("anything"); err == nil {
		t.Fatal("expected error")
	}
}

func TestHandle_LoginWithEmptyProvider(t *testing.T) {
	// "/login " (trailing whitespace only) lists methods (whitespace
	// trimmed → exact "/login" match).
	b := New(newFake())
	out, err := b.Handle(context.Background(), "c1", "/login   ")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Text, "Available login methods") {
		t.Fatalf("expected listing, got %q", out.Text)
	}
}

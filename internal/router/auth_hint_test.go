package router

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// authErr is the prompt error fir returns when no provider is connected.
func authErr() error {
	return &acp.RequestError{
		Code:    authRequiredCode,
		Message: "Authentication required",
		Data:    map[string]any{"error": "no model selected. Use /login or set an API key environment variable"},
	}
}

func runPromptWithErr(t *testing.T, promptErr error, hint func() string) *captureSink {
	t.Helper()
	agent := newFakeAgent(func(_ context.Context, _ *fakeAgent, _ acp.SessionId, _ string) (acp.StopReason, error) {
		return "", promptErr
	})
	r, err := New(Config{Agent: agent, StateDir: t.TempDir(), SessionTTL: time.Hour, AuthErrorHint: hint})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sink := &captureSink{}
	_ = r.Prompt(context.Background(), "c1", "u", []Turn{{Role: "user", Content: "hi"}}, Options{}, sink)
	return sink
}

func TestRouter_AuthErrorHint_ReplacesRawError(t *testing.T) {
	sink := runPromptWithErr(t, authErr(), func() string { return "connect a provider via !login" })
	if got := sink.text.String(); !strings.Contains(got, "!login") {
		t.Fatalf("expected hint text, got %q", got)
	}
	if sink.errText != "" {
		t.Fatalf("raw error leaked to sink.Error: %q", sink.errText)
	}
	if !sink.done {
		t.Fatal("done not called")
	}
}

func TestRouter_AuthErrorHint_NilFallsBackToError(t *testing.T) {
	sink := runPromptWithErr(t, authErr(), nil)
	if sink.text.Len() != 0 {
		t.Fatalf("unexpected text on fallback: %q", sink.text.String())
	}
	if !strings.Contains(sink.errText, "acp prompt") {
		t.Fatalf("expected raw error, got %q", sink.errText)
	}
}

func TestRouter_AuthErrorHint_EmptyFallsBackToError(t *testing.T) {
	sink := runPromptWithErr(t, authErr(), func() string { return "" })
	if !strings.Contains(sink.errText, "acp prompt") {
		t.Fatalf("expected raw error when hint empty, got %q", sink.errText)
	}
}

func TestRouter_NonAuthError_NotIntercepted(t *testing.T) {
	// hint is set, but the error is not the auth error → raw error path.
	cases := []error{
		errors.New("boom"), // not a RequestError
		&acp.RequestError{Code: -32099, Message: "Other"},          // wrong code
		&acp.RequestError{Code: authRequiredCode, Message: "Nope"}, // right code, wrong message
	}
	for _, e := range cases {
		sink := runPromptWithErr(t, e, func() string { return "should-not-appear" })
		if strings.Contains(sink.text.String(), "should-not-appear") {
			t.Fatalf("hint wrongly applied for %v", e)
		}
		if !strings.Contains(sink.errText, "acp prompt") {
			t.Fatalf("expected raw error for %v, got %q", e, sink.errText)
		}
	}
}

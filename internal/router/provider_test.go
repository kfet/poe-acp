package router

import (
	"testing"

	"github.com/kfet/acp-kit/client"
)

func TestProviderOf(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"anthropic/claude-opus-5", "anthropic"},
		{"sakana/shinka-1", "sakana"},
		{"kimi-k2", OtherProvider},
		{"", OtherProvider},
		{"/leading-slash", OtherProvider},
	}
	for _, tc := range cases {
		if got := ProviderOf(tc.in); got != tc.want {
			t.Errorf("ProviderOf(%q) = %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestSanitiseProviderAndParamName(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"", OtherProvider},
		{"anthropic", "anthropic"},
		{"Foo.Bar", "foo_bar"},
		{"a-b_9", "a_b_9"},
	}
	for _, tc := range cases {
		if got := SanitiseProvider(tc.in); got != tc.want {
			t.Errorf("SanitiseProvider(%q) = %q want %q", tc.in, got, tc.want)
		}
		if got, want := ProviderParamName(tc.in), "model_"+tc.want; got != want {
			t.Errorf("ProviderParamName(%q) = %q want %q", tc.in, got, want)
		}
	}
}

// TestDefaultModelForProvider pins the single-sourced rule that both
// paramctl.Build (UI default_value) and resolveModel (runtime
// provider-only fallback) depend on.
func TestDefaultModelForProvider(t *testing.T) {
	t.Parallel()
	catalog := []client.ModelInfo{
		{ID: "anthropic/claude-opus-5"},
		{ID: "anthropic/claude-sonnet-4-5"},
		{ID: "sakana/shinka-1"},
		{ID: "kimi-k2"},
	}
	cases := []struct {
		name         string
		models       []client.ModelInfo
		provider     string
		defaultModel string
		want         string
	}{
		{"default lives in provider", catalog, "anthropic", "anthropic/claude-sonnet-4-5", "anthropic/claude-sonnet-4-5"},
		{"default belongs elsewhere → first of provider", catalog, "sakana", "anthropic/claude-opus-5", "sakana/shinka-1"},
		{"no default → first of provider", catalog, "anthropic", "", "anthropic/claude-opus-5"},
		{"default not in catalog → first of provider", catalog, "anthropic", "anthropic/phantom", "anthropic/claude-opus-5"},
		{"other bucket", catalog, OtherProvider, "anthropic/claude-opus-5", "kimi-k2"},
		{"unknown provider", catalog, "nosuch", "anthropic/claude-opus-5", ""},
		{"empty catalog", nil, "anthropic", "anthropic/claude-opus-5", ""},
		{"empty model id is not a match for an empty default", []client.ModelInfo{{ID: ""}}, OtherProvider, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DefaultModelForProvider(tc.models, tc.provider, tc.defaultModel); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

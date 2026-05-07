package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kfet/poe-acp/internal/poeproto"
)

func TestSchemaHash(t *testing.T) {
	if got, _ := schemaHash(nil); got != "nil" {
		t.Fatalf("nil: %q", got)
	}
	pc := &poeproto.ParameterControls{APIVersion: "2"}
	a, err := schemaHash(pc)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := schemaHash(pc)
	if a != b || len(a) != 64 {
		t.Fatalf("hash unstable: %q vs %q", a, b)
	}
}

func TestTruncate(t *testing.T) {
	cases := map[string]struct {
		s    string
		n    int
		want string
	}{
		"zero":       {"x", 0, ""},
		"short":      {"hi", 10, "hi"},
		"exact":      {"hello", 5, "hello"},
		"trim ascii": {"hellothere", 5, "hello…"},
		"multibyte":  {strings.Repeat("☃", 10), 5, strings.Repeat("☃", 5) + "…"},
	}
	for name, c := range cases {
		if got := truncate(c.s, c.n); got != c.want {
			t.Errorf("%s: got %q want %q", name, got, c.want)
		}
	}
}

func TestDefaultStateDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/x")
	if got := defaultStateDir(); got != "/x/poe-acp" {
		t.Errorf("xdg: %q", got)
	}
	t.Setenv("XDG_STATE_HOME", "")
	defer swap(&userHomeDir, func() (string, error) { return "/h", nil })()
	if got := defaultStateDir(); got != "/h/.local/state/poe-acp" {
		t.Errorf("home: %q", got)
	}
	userHomeDir = func() (string, error) { return "", errors.New("nope") }
	if got := defaultStateDir(); !strings.HasSuffix(got, "/poe-acp") {
		t.Errorf("tmp: %q", got)
	}
}

func TestDefaultConfigPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/x")
	if got := defaultConfigPath(); got != "/x/poe-acp/config.json" {
		t.Errorf("xdg: %q", got)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	defer swap(&userHomeDir, func() (string, error) { return "/h", nil })()
	if got := defaultConfigPath(); got != "/h/.config/poe-acp/config.json" {
		t.Errorf("home: %q", got)
	}
	userHomeDir = func() (string, error) { return "", errors.New("nope") }
	if got := defaultConfigPath(); !strings.HasSuffix(got, "/poe-acp/config.json") {
		t.Errorf("tmp: %q", got)
	}
}

func TestAppendEnv(t *testing.T) {
	got := appendEnv([]string{"A=1", "B=2", "A=replaced"}, "A=3")
	if len(got) != 2 || got[0] != "B=2" || got[1] != "A=3" {
		t.Fatalf("got %v", got)
	}
}

func TestBuildSkillsCatalog(t *testing.T) {
	dir := t.TempDir()
	// No host skills dir → just builtin.
	cat := buildSkillsCatalog(filepath.Join(dir, "config.json"))
	if !strings.Contains(cat, "<available_skills>") {
		t.Fatalf("missing block: %s", cat)
	}
	// Add a host skill.
	host := filepath.Join(dir, "skills", "extra")
	if err := os.MkdirAll(host, 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("---\nname: extra\ndescription: host one\n---\n")
	if err := os.WriteFile(filepath.Join(host, "SKILL.md"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	cat = buildSkillsCatalog(filepath.Join(dir, "config.json"))
	if !strings.Contains(cat, "extra") {
		t.Fatalf("host skill missing: %s", cat)
	}
}

func TestMaybeRefetchSettings_DefaultEndpoint(t *testing.T) {
	// Empty endpointBase → uses api.poe.com. Make Do fail to short-circuit.
	defer swap(&httpClient, &http.Client{Transport: errTransport{}})()
	stateDir := t.TempDir()
	pc := &poeproto.ParameterControls{APIVersion: "2"}
	maybeRefetchSettings(context.Background(), stateDir, "bot", "k", pc, "")
}

func TestTruncate_ShortRunes(t *testing.T) {
	// Byte length > n but rune count <= n: returns s as-is.
	s := strings.Repeat("☃", 5) // 15 bytes, 5 runes
	if got := truncate(s, 10); got != s {
		t.Fatalf("got %q", got)
	}
}

func TestMaybeRefetchSettings_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"parameter_controls":{"api_version":"2"}}`))
	}))
	defer srv.Close()
	defer swap(&httpClient, srv.Client())()

	stateDir := t.TempDir()
	pc := &poeproto.ParameterControls{APIVersion: "2"}
	maybeRefetchSettings(context.Background(), stateDir, "bot", "k", pc, srv.URL)
	hashFile := filepath.Join(stateDir, "last_schema_hash")
	if _, err := os.Stat(hashFile); err != nil {
		t.Fatalf("hash not written: %v", err)
	}
	// Second call: schema unchanged → no-op (still succeeds).
	maybeRefetchSettings(context.Background(), stateDir, "bot", "k", pc, srv.URL)
}

func TestMaybeRefetchSettings_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	defer swap(&httpClient, srv.Client())()
	stateDir := t.TempDir()
	pc := &poeproto.ParameterControls{APIVersion: "2"}
	maybeRefetchSettings(context.Background(), stateDir, "bot", "k", pc, srv.URL)
	if _, err := os.Stat(filepath.Join(stateDir, "last_schema_hash")); err == nil {
		t.Fatal("hash should not be written on HTTP error")
	}
}

func TestMaybeRefetchSettings_PoeDroppedControls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"parameter_controls":null}`))
	}))
	defer srv.Close()
	defer swap(&httpClient, srv.Client())()
	stateDir := t.TempDir()
	pc := &poeproto.ParameterControls{APIVersion: "2"}
	maybeRefetchSettings(context.Background(), stateDir, "bot", "k", pc, srv.URL)
	if _, err := os.Stat(filepath.Join(stateDir, "last_schema_hash")); err == nil {
		t.Fatal("hash should not be written when Poe drops controls")
	}
}

func TestMaybeRefetchSettings_DoError(t *testing.T) {
	defer swap(&httpClient, &http.Client{Transport: errTransport{}})()
	stateDir := t.TempDir()
	pc := &poeproto.ParameterControls{APIVersion: "2"}
	maybeRefetchSettings(context.Background(), stateDir, "bot", "k", pc, "http://localhost:1")
}

type errTransport struct{}

func (errTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("dial-fail")
}

func TestMaybeRefetchSettings_BadEndpoint(t *testing.T) {
	stateDir := t.TempDir()
	pc := &poeproto.ParameterControls{APIVersion: "2"}
	// Invalid scheme → http.NewRequest fails.
	maybeRefetchSettings(context.Background(), stateDir, "bot", "k", pc, "ht\ttp://bad")
}

func TestMaybeRefetchSettings_HashWriteError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"parameter_controls":{"api_version":"2"}}`))
	}))
	defer srv.Close()
	defer swap(&httpClient, srv.Client())()
	// State dir is a file (not a directory) → writeFile fails.
	dir := t.TempDir()
	stateAsFile := filepath.Join(dir, "state")
	if err := os.WriteFile(stateAsFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	pc := &poeproto.ParameterControls{APIVersion: "2"}
	maybeRefetchSettings(context.Background(), stateAsFile, "bot", "k", pc, srv.URL)
}

// guard: ensure URL-escaped names don't break.
func TestMaybeRefetchSettings_URLEscape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"parameter_controls":{"api_version":"2"}}`))
	}))
	defer srv.Close()
	defer swap(&httpClient, srv.Client())()
	stateDir := t.TempDir()
	pc := &poeproto.ParameterControls{APIVersion: "2"}
	// Names with characters needing escaping must not break URL build.
	maybeRefetchSettings(context.Background(), stateDir, "bot name", "k/y", pc, srv.URL)
}

func swap[T any](dst *T, v T) func() {
	old := *dst
	*dst = v
	return func() { *dst = old }
}

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kfet/poe-acp/internal/poeproto"
	"github.com/kfet/poe-acp/internal/skills"
)

func TestSchemaHash(t *testing.T) {
	if got := schemaHash(nil); got != "nil" {
		t.Fatalf("nil: %q", got)
	}
	pc := &poeproto.ParameterControls{APIVersion: "2"}
	a := schemaHash(pc)
	b := schemaHash(pc)
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

func TestSkillsCatalogProviderSeesHostSkillsAddedAfterStartup(t *testing.T) {
	dir := t.TempDir()
	provider := systemPromptProvider(filepath.Join(dir, "config.json"))
	if got := provider(); strings.Contains(got, "host later") {
		t.Fatalf("host skill appeared before it existed: %s", got)
	}

	host := filepath.Join(dir, "skills", "later")
	if err := os.MkdirAll(host, 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("---\nname: later\ndescription: host later\n---\n")
	if err := os.WriteFile(filepath.Join(host, "SKILL.md"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	got := provider()
	if !strings.Contains(got, "later") || !strings.Contains(got, "host later") {
		t.Fatalf("provider did not pick up new host skill: %s", got)
	}
}

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// writeCfgWithPrompt creates a config.json + a prompt file in the same
// dir and returns the config path. The configured system_prompt_file is
// relative ("prompt.md") so the test also exercises path resolution.
func writeCfgWithPrompt(t *testing.T, prompt string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "prompt.md"), []byte(prompt), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"system_prompt_file":"prompt.md"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

func TestSystemPromptProviderPrependsPromptFile(t *testing.T) {
	got := systemPromptProvider(writeCfgWithPrompt(t, "OP-INSTRUCTIONS"))()
	op := strings.Index(got, "OP-INSTRUCTIONS")
	cat := strings.Index(got, "<available_skills>")
	if op < 0 || cat < 0 || op > cat {
		t.Fatalf("operator prompt should precede catalog: op=%d cat=%d full=%s", op, cat, got)
	}
}

func TestSystemPromptProviderDisableShortCircuits(t *testing.T) {
	// disable wins even when a prompt file is configured.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "prompt.md"), []byte("X"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"system_prompt_file":"prompt.md","disable_system_prompt":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := systemPromptProvider(cfgPath)(); got != "" {
		t.Fatalf("disabled should return empty, got %q", got)
	}
}

func TestSystemPromptProviderRereadsPromptFilePerCall(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptPath, []byte("V1"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"system_prompt_file":"prompt.md"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := systemPromptProvider(cfgPath)
	if got := provider(); !strings.Contains(got, "V1") {
		t.Fatalf("v1: %s", got)
	}
	if err := os.WriteFile(promptPath, []byte("V2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := provider(); !strings.Contains(got, "V2") || strings.Contains(got, "V1") {
		t.Fatalf("v2: %s", got)
	}
}

func TestSystemPromptProviderResolvesAbsolutePromptFile(t *testing.T) {
	promptDir := t.TempDir()
	promptPath := filepath.Join(promptDir, "abs.md")
	if err := os.WriteFile(promptPath, []byte("ABS-PROMPT"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := writeCfg(t, fmt.Sprintf(`{"system_prompt_file":%q}`, promptPath))
	got := systemPromptProvider(cfgPath)()
	if !strings.Contains(got, "ABS-PROMPT") {
		t.Fatalf("absolute prompt path not honoured: %s", got)
	}
}

func TestSystemPromptProviderMissingPromptFileFallsBackToCatalog(t *testing.T) {
	// system_prompt_file points at a non-existent file → log + treat as
	// empty; catalog still renders (boot already fails fast via main.go).
	cfgPath := writeCfg(t, `{"system_prompt_file":"nope.md"}`)
	got := systemPromptProvider(cfgPath)()
	if !strings.Contains(got, "<available_skills>") {
		t.Fatalf("catalog missing when prompt file absent: %s", got)
	}
}

func TestSystemPromptProviderNoPromptFileStillReturnsCatalog(t *testing.T) {
	// Bare config (no system_prompt_file at all) → just the catalog.
	cfgPath := writeCfg(t, `{}`)
	got := systemPromptProvider(cfgPath)()
	if !strings.Contains(got, "<available_skills>") {
		t.Fatalf("catalog missing on bare config: %s", got)
	}
}

func TestSystemPromptProviderMissingConfigStillReturnsCatalog(t *testing.T) {
	got := systemPromptProvider(filepath.Join(t.TempDir(), "nope.json"))()
	if !strings.Contains(got, "<available_skills>") {
		t.Fatalf("catalog missing when config absent: %s", got)
	}
}

func TestSystemPromptProviderBrokenConfigStillReturnsCatalog(t *testing.T) {
	got := systemPromptProvider(writeCfg(t, `{not json`))()
	if !strings.Contains(got, "<available_skills>") {
		t.Fatalf("catalog missing on broken config: %s", got)
	}
}

func TestSystemPromptProviderDisableWinsOverMissingFile(t *testing.T) {
	// disable=true + configured file that doesn't exist → empty, no
	// fall-through to a file read whose error would be logged.
	cfgPath := writeCfg(t, `{"system_prompt_file":"nope.md","disable_system_prompt":true}`)
	if got := systemPromptProvider(cfgPath)(); got != "" {
		t.Fatalf("disable should short-circuit before file read, got %q", got)
	}
}

func TestReadSystemPromptFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "p.md"), []byte("  body \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Empty configured path → all-zero, no error.
	resolved, contents, err := readSystemPromptFile(dir, "")
	if resolved != "" || contents != "" || err != nil {
		t.Fatalf("empty: resolved=%q contents=%q err=%v", resolved, contents, err)
	}
	// Relative path → trims contents, resolves against dir.
	resolved, contents, err = readSystemPromptFile(dir, "p.md")
	if err != nil || contents != "body" || resolved != filepath.Join(dir, "p.md") {
		t.Fatalf("relative: resolved=%q contents=%q err=%v", resolved, contents, err)
	}
	// Absolute path → trims, used as-is.
	abs := filepath.Join(dir, "p.md")
	resolved, contents, err = readSystemPromptFile("/somewhere/else", abs)
	if err != nil || contents != "body" || resolved != abs {
		t.Fatalf("absolute: resolved=%q contents=%q err=%v", resolved, contents, err)
	}
	// Missing file → resolved path still reported (for error context).
	resolved, _, err = readSystemPromptFile(dir, "nope.md")
	if err == nil || resolved != filepath.Join(dir, "nope.md") {
		t.Fatalf("missing: resolved=%q err=%v", resolved, err)
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

func TestBuildSkillsCatalog_LoaderErrors(t *testing.T) {
	defer swap(&loadBuiltinSkills, func() ([]skills.Skill, error) {
		return nil, errors.New("builtin-fail")
	})()
	defer swap(&loadDirSkills, func(string) ([]skills.Skill, error) {
		return nil, errors.New("host-fail")
	})()
	// Both loaders fail → merged is empty → returns "".
	if got := buildSkillsCatalog(filepath.Join(t.TempDir(), "config.json")); got != "" {
		t.Fatalf("expected empty catalog on loader failure, got %q", got)
	}
}

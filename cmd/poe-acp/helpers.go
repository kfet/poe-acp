// Helpers extracted from main.go so they can be unit-tested in isolation.
// main.go retains only the entry point shim.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kfet/poe-acp/internal/poeproto"
	"github.com/kfet/poe-acp/internal/skills"
)

// httpClient is overridable in tests so maybeRefetchSettings can be
// driven against an httptest.Server without reaching real Poe.
var httpClient = http.DefaultClient

// maybeRefetchSettings hashes the freshly built parameter_controls and
// compares against the last-pushed hash on disk. On change, it POSTs
// to Poe's /bot/fetch_settings/<bot>/<key>/1.1 endpoint to invalidate
// Poe's cache so the UI picks up the new schema. Best-effort: every
// failure is logged and swallowed.
//
// endpointBase, when non-empty, replaces the default api.poe.com host —
// used by tests against an httptest.Server.
func maybeRefetchSettings(ctx context.Context, stateDir, botName, accessKey string, controls *poeproto.ParameterControls, endpointBase string) {
	hashFile := filepath.Join(stateDir, "last_schema_hash")
	h := schemaHash(controls)
	prev, _ := os.ReadFile(hashFile)
	if string(prev) == h {
		log.Printf("settings refetch: schema unchanged (hash=%s), skipping", h[:12])
		return
	}

	if endpointBase == "" {
		endpointBase = "https://api.poe.com"
	}
	endpoint := fmt.Sprintf("%s/bot/fetch_settings/%s/%s/1.1",
		endpointBase, url.PathEscape(botName), url.PathEscape(accessKey))
	rctx, rcancel := context.WithTimeout(ctx, 30*time.Second)
	defer rcancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, endpoint, nil)
	if err != nil {
		log.Printf("settings refetch: build request: %v", err)
		return
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("settings refetch: %v", err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		log.Printf("settings refetch: HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
		return
	}
	var echo struct {
		ParameterControls json.RawMessage `json:"parameter_controls"`
	}
	if err := json.Unmarshal(body, &echo); err == nil {
		if len(echo.ParameterControls) == 0 || bytes.Equal(echo.ParameterControls, []byte("null")) {
			log.Printf("settings refetch: WARNING Poe dropped parameter_controls (schema invalid). Body: %s", truncate(string(body), 400))
			return
		}
	}
	if err := os.WriteFile(hashFile, []byte(h), 0o600); err != nil {
		log.Printf("settings refetch: write hash file: %v", err)
		return
	}
	log.Printf("settings refetch: ok (hash=%s)", h[:12])
}

// schemaHash returns a stable SHA-256 hex digest of the parameter
// controls JSON. JSON marshal output of struct types is deterministic
// (field order fixed), and we don't reorder option lists, so equal
// schemas produce equal hashes.
func schemaHash(pc *poeproto.ParameterControls) string {
	if pc == nil {
		return "nil"
	}
	sum := sha256.Sum256(mustMarshalJSON(pc))
	return hex.EncodeToString(sum[:])
}

// truncate shortens s to at most n runes, appending an ellipsis when
// truncation occurs. Operates on runes (not bytes) so it never splits
// a multi-byte UTF-8 sequence in the middle.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

// userHomeDir is overridable in tests.
var userHomeDir = os.UserHomeDir

func defaultStateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "poe-acp")
	}
	if h, err := userHomeDir(); err == nil {
		return filepath.Join(h, ".local", "state", "poe-acp")
	}
	return filepath.Join(os.TempDir(), "poe-acp")
}

func defaultConfigPath() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "poe-acp", "config.json")
	}
	if h, err := userHomeDir(); err == nil {
		return filepath.Join(h, ".config", "poe-acp", "config.json")
	}
	return filepath.Join(os.TempDir(), "poe-acp", "config.json")
}

func appendEnv(env []string, kv string) []string {
	key := strings.SplitN(kv, "=", 2)[0] + "="
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if !strings.HasPrefix(e, key) {
			out = append(out, e)
		}
	}
	return append(out, kv)
}

// loadBuiltinSkills / loadDirSkills are overridable for tests.
var (
	loadBuiltinSkills = skills.LoadBuiltin
	loadDirSkills     = skills.LoadDir
)

// buildSkillsCatalog merges embedded built-in skills with optional
// host-supplied skills from <dirname(cfgPath)>/skills/ and returns a
// fir-style <available_skills> block ready for injection. Best-effort:
// extraction failures degrade to whatever layers succeeded (the relay
// is still usable without a catalog). Host skills with the same name
// as a built-in override the built-in (the disable mechanism).
func buildSkillsCatalog(cfgPath string) string {
	builtin, err := loadBuiltinSkills()
	if err != nil {
		log.Printf("skills: builtin load failed (continuing): %v", err)
	}
	hostDir := filepath.Join(filepath.Dir(cfgPath), "skills")
	host, err := loadDirSkills(hostDir)
	if err != nil {
		log.Printf("skills: host dir %s: %v (continuing)", hostDir, err)
	}
	merged := skills.Merge([][]skills.Skill{builtin, host}, nil)
	if len(merged) == 0 {
		return ""
	}
	names := make([]string, 0, len(merged))
	for _, s := range merged {
		names = append(names, s.Name)
	}
	log.Printf("skills: %d builtin + %d host -> injected %d (%s)",
		len(builtin), len(host), len(merged), strings.Join(names, ","))
	return skills.FormatCatalog(merged)
}

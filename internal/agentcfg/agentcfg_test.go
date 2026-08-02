package agentcfg

import (
	"strings"
	"testing"
)

// refScrub mirrors acp-kit v0.4.0 client.Config's documented scrub
// contract (client/agent.go scrubEnv): drop any entry whose variable
// NAME is in names, or whose VALUE literally equals a non-empty entry in
// secrets. It lets these poe-acp tests prove — without spawning a real
// agent process — that the names/values New() declares yield the right
// drop/survive outcome when acp-kit applies them.
func refScrub(env, names, secrets []string) []string {
	dropName := map[string]bool{}
	for _, n := range names {
		dropName[n] = true
	}
	dropVal := map[string]bool{}
	for _, s := range secrets {
		if s != "" {
			dropVal[s] = true
		}
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, val, _ := strings.Cut(kv, "=")
		if dropName[name] || dropVal[val] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func envHas(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

func TestNew_DeclaresRelaySecrets(t *testing.T) {
	cfg := New(Params{
		AccessKeyEnv: "MY_ACCESS_KEY_ENV",
		AccessKey:    "poe-bearer-secret",
		AdminToken:   "admin-tok",
	})

	// By-name declarations: the RESOLVED access-key env var name (not the
	// hardcoded default) and ADMIN_TOKEN.
	wantNames := map[string]bool{"MY_ACCESS_KEY_ENV": false, AdminTokenEnv: false}
	for _, n := range cfg.SecretEnvNames {
		if _, ok := wantNames[n]; ok {
			wantNames[n] = true
		}
	}
	for n, seen := range wantNames {
		if !seen {
			t.Errorf("SecretEnvNames missing %q; got %v", n, cfg.SecretEnvNames)
		}
	}

	// By-value declarations: the bearer secret and admin token literals.
	if !contains(cfg.Secrets, "poe-bearer-secret") {
		t.Errorf("Secrets missing access-key value; got %v", cfg.Secrets)
	}
	if !contains(cfg.Secrets, "admin-tok") {
		t.Errorf("Secrets missing admin-token value; got %v", cfg.Secrets)
	}

	// The default env var name must NOT be assumed when a different name
	// was resolved — otherwise a bot run with --access-key-env=OTHER
	// would leak its real bearer secret.
	if contains(cfg.SecretEnvNames, "POEACP_ACCESS_KEY") {
		t.Errorf("SecretEnvNames wrongly hardcodes the default name; got %v", cfg.SecretEnvNames)
	}
}

func TestNew_Passthrough(t *testing.T) {
	meta := map[string]any{"ext": 1}
	env := []string{"A=1"}
	cfg := New(Params{
		Command:    []string{"fir", "--mode", "acp"},
		Cwd:        "/state",
		Env:        env,
		ClientMeta: meta,
	})
	if len(cfg.Command) != 3 || cfg.Command[0] != "fir" {
		t.Errorf("Command not passed through: %v", cfg.Command)
	}
	if cfg.Cwd != "/state" {
		t.Errorf("Cwd not passed through: %q", cfg.Cwd)
	}
	if len(cfg.Env) != 1 || cfg.Env[0] != "A=1" {
		t.Errorf("Env not passed through: %v", cfg.Env)
	}
	if cfg.ClientMeta["ext"] != 1 {
		t.Errorf("ClientMeta not passed through: %v", cfg.ClientMeta)
	}
}

// TestNew_ScrubDropsSecretsPreservesProviders is the load-bearing test:
// it runs a synthetic environment through acp-kit's documented scrub
// contract using ONLY what New() declares, and proves the two relay
// secrets are dropped (by name AND under a bespoke name, by value) while
// provider credentials the agent legitimately needs survive.
func TestNew_ScrubDropsSecretsPreservesProviders(t *testing.T) {
	const (
		accessKeyEnv = "POEACP_ACCESS_KEY"
		accessKey    = "s3cr3t-bearer"
		adminToken   = "admin-s3cr3t"
	)
	cfg := New(Params{
		AccessKeyEnv: accessKeyEnv,
		AccessKey:    accessKey,
		AdminToken:   adminToken,
	})

	env := []string{
		accessKeyEnv + "=" + accessKey,     // relay bearer, by name
		"ADMIN_TOKEN=" + adminToken,        // admin token, by name
		"BESPOKE_COPY=" + accessKey,        // bearer copied under another name (by value)
		"ANOTHER_ADMIN=" + adminToken,      // admin token copied (by value)
		"ANTHROPIC_API_KEY=sk-ant-abc",     // provider — must survive
		"OPENAI_API_KEY=sk-openai-xyz",     // provider — must survive
		"POE_API_KEY=fir-poe-provider-key", // fir's poe provider — must survive
		"FIR_AGENT_DIR=/home/agent/.fir",   // agent config — must survive
		"PATH=/usr/bin",                    // benign — must survive
	}

	scrubbed := refScrub(env, cfg.SecretEnvNames, cfg.Secrets)

	// Dropped.
	for _, gone := range []string{
		accessKeyEnv + "=" + accessKey,
		"ADMIN_TOKEN=" + adminToken,
		"BESPOKE_COPY=" + accessKey,
		"ANOTHER_ADMIN=" + adminToken,
	} {
		if envHas(scrubbed, gone) {
			t.Errorf("secret NOT scrubbed: %q still present", gone)
		}
	}

	// Preserved — the survival guarantee matters as much as the drop.
	for _, keep := range []string{
		"ANTHROPIC_API_KEY=sk-ant-abc",
		"OPENAI_API_KEY=sk-openai-xyz",
		"POE_API_KEY=fir-poe-provider-key",
		"FIR_AGENT_DIR=/home/agent/.fir",
		"PATH=/usr/bin",
	} {
		if !envHas(scrubbed, keep) {
			t.Errorf("legitimate var wrongly scrubbed: %q missing", keep)
		}
	}
}

// TestNew_EmptyAdminTokenNoBlanketDrop guards the empty-value case: a bot
// with no ADMIN_TOKEN set must not turn the empty Secrets entry into a
// filter that drops every empty-valued variable.
func TestNew_EmptyAdminTokenNoBlanketDrop(t *testing.T) {
	cfg := New(Params{
		AccessKeyEnv: "POEACP_ACCESS_KEY",
		AccessKey:    "bearer",
		AdminToken:   "", // not set
	})
	env := []string{
		"EMPTY_ONE=",
		"POE_API_KEY=provider",
	}
	scrubbed := refScrub(env, cfg.SecretEnvNames, cfg.Secrets)
	if !envHas(scrubbed, "EMPTY_ONE=") {
		t.Error("empty-valued var wrongly scrubbed by empty admin token")
	}
	if !envHas(scrubbed, "POE_API_KEY=provider") {
		t.Error("provider var wrongly scrubbed")
	}
	// ADMIN_TOKEN is still dropped by name even when its value is empty.
	if !contains(cfg.SecretEnvNames, AdminTokenEnv) {
		t.Errorf("ADMIN_TOKEN must always be dropped by name; got %v", cfg.SecretEnvNames)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// Package config loads the relay's JSON config file. The file holds
// "what kind of bot is this" knobs (defaults shown to users, the bot's
// Poe name, agent profile selection) — separate from ops-level CLI
// flags (listen address, state dir, permission policy).
//
// Schema is intentionally small. Unknown keys fail loudly at boot
// (DisallowUnknownFields) so typos are caught immediately rather than
// silently ignored. Missing file is fine — empty Config means "use
// built-in defaults", which preserves zero-config installs.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// Config is the on-disk shape. Add fields with care — every field is
// part of the operator-facing contract.
type Config struct {
	// BotName is the Poe bot's slug as registered with Poe. Required to
	// auto-invalidate Poe's cached settings response when the relay's
	// schema changes (POST /bot/fetch_settings/<bot>/<key>/1.1). If
	// empty, the relay skips the refetch and operators must trigger it
	// manually after schema-affecting changes.
	BotName string `json:"bot_name,omitempty"`

	// Defaults are the values shown in the Poe Options panel and
	// applied on the first turn of every new conversation.
	Defaults Defaults `json:"defaults,omitempty"`

	// Agent is reserved for per-agent profile selection and inline
	// config-control overrides. Parsed today, used in a follow-up.
	Agent Agent `json:"agent,omitempty"`
}

// Defaults pins per-conversation parameter defaults independently of
// the agent's own current configuration. Stable across restarts so
// Poe's cached settings response stays valid.
type Defaults struct {
	// Model is the "<provider>/<modelId>" string applied via
	// session/set_model. Must appear in the agent's probed model list
	// at runtime; if not, the relay logs a warning and omits the
	// dropdown's default_value (UI shows first option, runtime falls
	// through to the agent's own default).
	Model string `json:"model,omitempty"`
	// Thinking is one of "off","minimal","low","medium","high","xhigh","max". Empty
	// string means "use built-in default" (currently "medium").
	Thinking string `json:"thinking,omitempty"`
	// HideThinking suppresses agent_thought_chunk in the SSE stream.
	// nil means "use built-in default" (currently true).
	HideThinking *bool `json:"hide_thinking,omitempty"`
}

// Agent groups agent-profile knobs. Reserved.
type Agent struct {
	// Profile names a built-in agent profile (e.g. "fir"). Empty =
	// auto-detect from --agent-cmd. Used to pick which set_config_option
	// controls the relay exposes. Reserved; today only "fir" is wired.
	Profile string `json:"profile,omitempty"`
}

// Load reads and parses a config file. A non-existent path returns an
// empty Config and ok=false — callers should treat that as "no config,
// use defaults" rather than an error. Any other failure (parse error,
// permission denied, unknown field) is returned verbatim.
func Load(path string) (cfg Config, ok bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Config{}, false, nil
		}
		return Config{}, false, fmt.Errorf("open config %s: %w", path, err)
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, false, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, false, fmt.Errorf("validate config %s: %w", path, err)
	}
	return cfg, true, nil
}

// Validate checks field-level invariants. Cross-field checks against
// the agent's runtime state (e.g. "is Defaults.Model in the probed
// list?") happen in main.go after the probe completes.
func (c Config) Validate() error {
	switch c.Defaults.Thinking {
	case "", "off", "minimal", "low", "medium", "high", "xhigh", "max":
	default:
		return fmt.Errorf("defaults.thinking: invalid %q (want off|minimal|low|medium|high|xhigh|max)", c.Defaults.Thinking)
	}
	return nil
}

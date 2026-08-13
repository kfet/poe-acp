// Package config loads the relay's JSON config file. The file holds
// "what kind of bot is this" knobs (defaults shown to users, the bot's
// Poe name, agent profile selection) — separate from ops-level CLI
// flags (listen address, state dir).
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
	"time"
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

	// SystemPromptFile, when non-empty, names a file whose contents
	// are injected as a durable system prompt into every new ACP
	// session (before the skills catalog). Relative paths are
	// resolved against the directory of this config file; absolute
	// paths are used as-is. The file is read per new conversation
	// so edits take effect on the next new chat without a relay
	// restart. Use this for substantial prompts — Markdown lives
	// well in a file, escaped in JSON it doesn't. The result goes
	// into the authoritative system slot.
	SystemPromptFile string `json:"system_prompt_file,omitempty"`

	// DisableSystemPrompt skips system-prompt injection entirely
	// (both the operator prompt file and the skills catalog) and
	// also suppresses the router's transport-contract clause. Use
	// only if you have a reason to want raw, unguided agent output.
	DisableSystemPrompt bool `json:"disable_system_prompt,omitempty"`

	// PoeMCP enables the self-hosted `poe` MCP server exposed to the
	// agent (tool: attach) — the per-bot, config-file way to
	// turn the feature on without touching the CLI flags. Effective
	// enablement is this OR the --enable-mcp-attach flag (kept as a
	// deprecated alias), so existing flag-based deployments keep working.
	PoeMCP bool `json:"poe_mcp,omitempty"`
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
	// ShowPlans renders the agent's current plan (ACP `plan`
	// session/update) as a transient checklist inside the mid-turn
	// keepalive frame. nil means "use built-in default" (currently
	// true).
	ShowPlans *bool `json:"show_plans,omitempty"`
	// ShowTools emits one durable transcript line per ACP `tool_call`
	// so a tool-heavy turn leaves a trace of what the agent did. nil
	// means "use built-in default" (currently true).
	ShowTools *bool `json:"show_tools,omitempty"`
	// CoalesceMs, when > 0, buffers outbound SSE `text` events and
	// flushes them on a shared wall-clock grid instead of emitting one
	// frame per agent chunk. Frames-per-turn is a direct proxy for
	// mobile radio wakeups: a packet every 20–100ms is too frequent for
	// LTE/5G cDRX micro-sleep (which needs ~100–320ms quiet gaps) and
	// too slow to be an efficient bulk transfer. Coalescing upstream is
	// monotone — Poe cannot relay frames it was never given — and the
	// relay→Poe hop is wired, so the buffering is energetically free.
	// nil means "use built-in default" (DefaultCoalesceMs = 3000); an
	// explicit 0 disables coalescing and restores 1:1 chunk→frame.
	CoalesceMs *int `json:"coalesce_ms,omitempty"`
	// CoalesceGrid aligns flushes to absolute wall-clock instants
	// (multiples of coalesce_ms since the epoch) rather than a
	// per-stream timer. With several bots on several hosts, independent
	// timers interleave and the phone's modem never gets a quiet gap;
	// the wall clock is shared state needing no coordination protocol,
	// so every stream everywhere lands in the same wake window. nil
	// means "use built-in default" (currently true). Only has effect
	// when CoalesceMs > 0.
	CoalesceGrid *bool `json:"coalesce_grid,omitempty"`
	// SpinnerAnimate animates the keepalive spinner's dots on every
	// heartbeat tick. Animation changes the payload every tick, which
	// defeats the identical-frame dedupe (and any coalescing Poe itself
	// might do) — turning it off collapses the keepalive down to genuine
	// state changes plus a liveness floor, which is why it is off by
	// default (see DefaultSpinnerAnimate). nil means "use built-in
	// default" (currently false).
	SpinnerAnimate *bool `json:"spinner_animate,omitempty"`
	// ShowToolDetails renders each tool call's `content` blocks under
	// its durable line, plus one group per COMPLETED/FAILED
	// `tool_call_update` carrying the result text. Bounded per block
	// (head/tail with an elision marker). Only has effect when
	// ShowTools is also on. nil means "use built-in default"
	// (currently true).
	ShowToolDetails *bool `json:"show_tool_details,omitempty"`
}

// DefaultCoalesceMs is the built-in fallback for
// `defaults.coalesce_ms`. Coalescing is ON by default at 3s: the
// dominant client is the Poe mobile app, where frames-per-turn drives
// radio wakeups — a 3s flush grid lets the modem's cDRX micro-sleep
// actually engage between frames, and a week of production use across
// four bots confirmed the app is markedly better with it. The relay→Poe
// hop is wired and coalescing is monotone, so the buffering costs
// nothing but output latency. An operator who wants 1:1 chunk→frame
// must set an explicit `"coalesce_ms": 0`.
const DefaultCoalesceMs = 3000

// DefaultCoalesceGrid is the built-in fallback for
// `defaults.coalesce_grid`. Grid alignment is the whole point of the
// feature once coalescing is on (see Defaults.CoalesceGrid), so it is
// on by default; an operator must opt OUT to get per-stream timers.
const DefaultCoalesceGrid = true

// DefaultSpinnerAnimate is the built-in fallback for
// `defaults.spinner_animate`. Off by default: an animated spinner
// mutates the keepalive payload every heartbeat tick, defeating the
// identical-frame dedupe and waking the phone's radio for no
// information. A static spinner still proves liveness. An operator who
// wants the dots to move must set an explicit
// `"spinner_animate": true`.
const DefaultSpinnerAnimate = false

// Stream is the resolved outbound stream-shaping configuration handed
// to the HTTP layer. It exists so the nil-means-default resolution
// lives in one tested place instead of in main.go.
type Stream struct {
	// CoalesceInterval is the text-flush period; 0 disables coalescing.
	CoalesceInterval time.Duration
	// CoalesceGrid aligns flushes to wall-clock multiples of the period.
	CoalesceGrid bool
	// SpinnerAnimate advances the spinner dots on every heartbeat tick.
	SpinnerAnimate bool
}

// Stream resolves the stream-shaping knobs against their built-in
// defaults. A zero Defaults yields the mobile-friendly shape: 3s grid
// coalescing, static spinner. Assumes a Config that passed Validate
// (which is what bounds CoalesceMs — see MaxCoalesceMs).
func (d Defaults) Stream() Stream {
	s := Stream{
		CoalesceInterval: DefaultCoalesceMs * time.Millisecond,
		CoalesceGrid:     DefaultCoalesceGrid,
		SpinnerAnimate:   DefaultSpinnerAnimate,
	}
	if d.CoalesceMs != nil {
		s.CoalesceInterval = time.Duration(*d.CoalesceMs) * time.Millisecond
	}
	if d.CoalesceGrid != nil {
		s.CoalesceGrid = *d.CoalesceGrid
	}
	if d.SpinnerAnimate != nil {
		s.SpinnerAnimate = *d.SpinnerAnimate
	}
	return s
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
// access denied, unknown field) is returned verbatim.
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

// MaxCoalesceMs bounds `defaults.coalesce_ms`. The knob trades output
// latency for radio wakeups, and past a few seconds the trade stops
// making sense: a 10-minute buffer starves mid-turn output completely,
// and a value large enough to overflow time.Duration's nanosecond int64
// would wrap negative and silently degrade to "off" — the worst failure
// mode, since the operator would see no error and no effect. 60s is far
// beyond any defensible setting while
// keeping the arithmetic nowhere near the overflow boundary (3s is the
// built-in default).
const MaxCoalesceMs = 60_000

// Validate checks field-level invariants. Cross-field checks against
// the agent's runtime state (e.g. "is Defaults.Model in the probed
// list?") happen in main.go after the probe completes.
func (c Config) Validate() error {
	if c.Defaults.CoalesceMs != nil && (*c.Defaults.CoalesceMs < 0 || *c.Defaults.CoalesceMs > MaxCoalesceMs) {
		return fmt.Errorf("defaults.coalesce_ms: invalid %d (want 0..%d, 0 = disabled)",
			*c.Defaults.CoalesceMs, MaxCoalesceMs)
	}
	switch c.Defaults.Thinking {
	case "", "off", "minimal", "low", "medium", "high", "xhigh", "max":
	default:
		return fmt.Errorf("defaults.thinking: invalid %q (want off|minimal|low|medium|high|xhigh|max)", c.Defaults.Thinking)
	}
	return nil
}

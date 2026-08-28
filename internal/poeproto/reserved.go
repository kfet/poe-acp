package poeproto

import (
	"regexp"
	"strings"
)

// Reserved Poe parameter names — the canonical list.
//
// The relay declares these as `parameter_controls` (built in
// internal/paramctl). They double as RESERVED flag names: Poe's chat
// client parses "--<name>" tokens in message text and validates them
// against these controls' strict-enum dropdowns, so a freeform value is
// rejected and Poe drops the whole message *before it reaches the bot*.
// The bot must therefore never EMIT one verbatim — EscapeReservedFlags
// defuses them.
//
// Defining the names here, in the cycle-free Poe-protocol package that
// paramctl already imports, keeps the schema builder and the output
// escaper in lockstep: add a control with a new parameter_name here and
// both the schema and the escaper pick it up. (poeproto cannot import
// paramctl/router, so this is the lowest shared point.)
const (
	ParamModel        = "model"
	ParamProvider     = "provider"
	ParamThinking     = "thinking"
	ParamHideThinking = "hide_thinking"
	ParamShowPlans    = "show_plans"
	ParamShowTools    = "show_tools"
	// ParamShowToolDetails renders each tool call's content blocks and
	// its terminal result under the durable tool line. Only has effect
	// when ParamShowTools is also on.
	ParamShowToolDetails = "show_tool_details"
	// ParamHost selects which ssh host a NEW conversation's agent
	// session runs on (see internal/config Hosts). Declared only when
	// the operator configured a curated host list, so it is reserved
	// only for those bots — hence the separate
	// EscapeReservedFlagsWithHost entry point.
	ParamHost = "host"
	// ProviderParamPrefix + a sanitised provider id forms the per-provider
	// model dropdown's parameter_name (e.g. "model_anthropic").
	ProviderParamPrefix = "model_"
)

// reservedAlternatives is the regexp alternation body shared by both
// escapers: every ALWAYS-declared reserved parameter name.
const reservedAlternatives = ProviderParamPrefix + `[A-Za-z0-9_]+` +
	`|` + ParamModel +
	`|` + ParamProvider +
	`|` + ParamHideThinking +
	`|` + ParamShowPlans +
	`|` + ParamShowToolDetails +
	`|` + ParamShowTools +
	`|` + ParamThinking

// reservedFlagRe matches a "--<reserved>" flag token — a fixed reserved
// name or any per-provider model_<provider>. Assembled from the constants
// above so it tracks the declared schema automatically. The trailing \b
// stops "--model" from matching inside "--model_anthropic" (which the
// model_<provider> branch handles).
var reservedFlagRe = regexp.MustCompile(`--(` + reservedAlternatives + `)\b`)

// reservedFlagHostRe is reservedFlagRe plus "--host", used only by bots
// that declare the Host dropdown. `host` is a common word in shell text
// ("ssh --host x"), and Poe only rejects a flag it can match against a
// declared control, so unconfigured bots must keep emitting it verbatim.
var reservedFlagHostRe = regexp.MustCompile(`--(` + ParamHost + `|` + reservedAlternatives + `)\b`)

const zeroWidthSpace = "\u200b"

// EscapeReservedFlags inserts a zero-width space after the leading "--"
// of every reserved flag token in s, so Poe's chat client no longer
// parses it as a parameter (the text reads identically). Tokens in s must
// be whole; streaming callers must buffer a partial trailing token.
func EscapeReservedFlags(s string) string { return escape(reservedFlagRe, s) }

// EscapeReservedFlagsWithHost is EscapeReservedFlags for bots that also
// declare the Host dropdown, so "--host" is reserved for them too.
func EscapeReservedFlagsWithHost(s string) string { return escape(reservedFlagHostRe, s) }

func escape(re *regexp.Regexp, s string) string {
	if !strings.Contains(s, "--") {
		return s
	}
	return re.ReplaceAllString(s, "--"+zeroWidthSpace+"${1}")
}

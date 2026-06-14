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
	// ProviderParamPrefix + a sanitised provider id forms the per-provider
	// model dropdown's parameter_name (e.g. "model_anthropic").
	ProviderParamPrefix = "model_"
)

// reservedFlagRe matches a "--<reserved>" flag token — a fixed reserved
// name or any per-provider model_<provider>. Assembled from the constants
// above so it tracks the declared schema automatically. The trailing \b
// stops "--model" from matching inside "--model_anthropic" (which the
// model_<provider> branch handles).
var reservedFlagRe = regexp.MustCompile(
	`--(` + ProviderParamPrefix + `[A-Za-z0-9_]+` +
		`|` + ParamModel +
		`|` + ParamProvider +
		`|` + ParamHideThinking +
		`|` + ParamThinking +
		`)\b`)

const zeroWidthSpace = "\u200b"

// EscapeReservedFlags inserts a zero-width space after the leading "--"
// of every reserved flag token in s, so Poe's chat client no longer
// parses it as a parameter (the text reads identically). Tokens in s must
// be whole; streaming callers must buffer a partial trailing token.
func EscapeReservedFlags(s string) string {
	if !strings.Contains(s, "--") {
		return s
	}
	return reservedFlagRe.ReplaceAllString(s, "--"+zeroWidthSpace+"${1}")
}

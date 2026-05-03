// Package debuglog is a tiny gated logger for opt-in verbose tracing.
//
// Enabled when the environment variable POEACP_DEBUG is set to a
// truthy value (1/true/yes/on, case-insensitive), or when main calls
// SetEnabled(true) after parsing the --debug flag.
//
// All output goes to the standard log package so it shares the
// timestamp prefix configured in main.
package debuglog

import (
	"log"
	"os"
	"strings"
	"sync/atomic"
)

var enabled atomic.Bool

func init() {
	if truthy(os.Getenv("POEACP_DEBUG")) {
		enabled.Store(true)
	}
}

// SetEnabled forces the debug state on/off. Used by main when --debug
// is passed so the flag works independently of the env var.
func SetEnabled(on bool) { enabled.Store(on) }

// Enabled reports whether debug logging is on.
func Enabled() bool { return enabled.Load() }

// Logf logs a debug message when enabled. Prefix is "[dbg] ".
func Logf(format string, args ...any) {
	if !enabled.Load() {
		return
	}
	log.Printf("[dbg] "+format, args...)
}

func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on", "y", "t":
		return true
	}
	return false
}

package poeproto

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

// LogReject must survive a scanner burst on the public Funnel path
// without writing a line per hit: one line gets through, the rest are
// counted and reported on the next line that does.
func TestLogRejectRateLimitsAndCountsSuppressed(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	lastRejectLog.Store(0)
	rejectDropped.Store(0)

	for i := 0; i < 50; i++ {
		LogReject(401, "unauthorized", "10.0.0.1:1234")
	}
	got := strings.Count(buf.String(), "WARN reject:")
	if got != 1 {
		t.Fatalf("burst of 50 wrote %d lines, want 1:\n%s", got, buf.String())
	}
	if n := rejectDropped.Load(); n != 49 {
		t.Fatalf("suppressed count = %d, want 49", n)
	}

	// Next line that gets through must disclose the suppressed volume.
	buf.Reset()
	lastRejectLog.Store(0)
	LogReject(401, "unauthorized", "10.0.0.1:1234")
	if s := buf.String(); !strings.Contains(s, "+49 suppressed") {
		t.Fatalf("follow-up line lost the suppressed count: %q", s)
	}
}

package debuglog

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
)

func TestTruthy(t *testing.T) {
	for _, s := range []string{"1", "true", "TRUE", "yes", " on ", "Y", "t"} {
		if !truthy(s) {
			t.Errorf("truthy(%q)=false want true", s)
		}
	}
	for _, s := range []string{"", "0", "no", "off", "weird"} {
		if truthy(s) {
			t.Errorf("truthy(%q)=true want false", s)
		}
	}
}

func TestSetEnabledAndLogf(t *testing.T) {
	prev := enabled.Load()
	t.Cleanup(func() { SetEnabled(prev) })

	var buf bytes.Buffer
	oldOut := log.Writer()
	oldFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(oldOut); log.SetFlags(oldFlags) })

	SetEnabled(false)
	if Enabled() {
		t.Fatal("Enabled should be false")
	}
	Logf("nope %d", 1)
	if buf.Len() != 0 {
		t.Fatalf("expected no output, got %q", buf.String())
	}

	SetEnabled(true)
	if !Enabled() {
		t.Fatal("Enabled should be true")
	}
	Logf("hello %s", "world")
	if !strings.Contains(buf.String(), "[dbg] hello world") {
		t.Fatalf("missing dbg log: %q", buf.String())
	}
}

// TestInitFromEnv exercises the package init() path indirectly: spawning
// a sub-test process with POEACP_DEBUG set so the package is freshly
// initialised with the env var present.
func TestInitFromEnv(t *testing.T) {
	if os.Getenv("POEACP_DEBUG_TEST_CHILD") == "1" {
		// Child: package init already ran with POEACP_DEBUG=1.
		if !Enabled() {
			os.Stderr.WriteString("not-enabled")
			os.Exit(2)
		}
		os.Exit(0)
	}
	// Parent: re-run this test in a subprocess with the env var set.
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	cmd := newCmd(exe, "-test.run=TestInitFromEnv", "-test.v")
	cmd.Env = append(os.Environ(), "POEACP_DEBUG=1", "POEACP_DEBUG_TEST_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("subprocess failed: %v: %s", err, out)
	}
}

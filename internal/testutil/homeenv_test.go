package testutil

import (
	"os"
	"testing"
)

// An exported CODEX_HOME names the developer's live Codex config; inside the
// isolation it must be unset so Codex paths resolve under the temp HOME, and
// the cleanup must restore it.
func TestIsolateHome_ClearsCodexHome(t *testing.T) {
	t.Setenv("CODEX_HOME", "/live/codex")
	cleanup := IsolateHome()
	if v, ok := os.LookupEnv("CODEX_HOME"); ok {
		cleanup()
		t.Fatalf("CODEX_HOME = %q inside IsolateHome, want unset", v)
	}
	cleanup()
	if got := os.Getenv("CODEX_HOME"); got != "/live/codex" {
		t.Fatalf("CODEX_HOME after cleanup = %q, want /live/codex", got)
	}
}

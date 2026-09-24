package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The send path compares CodexTurnMarker across a send. It must move with each
// new turn and must not move when a completed turn's evidence is consumed,
// which masks the generation but not the sequence.
func TestCodexTurnMarkerFollowsTurnSequence(t *testing.T) {
	inboxTestHome(t)
	const id = "codex-turn-marker"
	if got := CodexTurnMarker(id); got != "" {
		t.Fatalf("marker without a hook file = %q, want empty", got)
	}
	if err := os.MkdirAll(GetHooksDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(generation string, started, completed uint64) {
		t.Helper()
		b, err := json.Marshal(map[string]any{
			"status":                     "waiting",
			"ts":                         time.Now().Unix(),
			"codex_started_generation":   generation,
			"codex_completed_generation": generation,
			"codex_started_sequence":     started,
			"codex_completed_sequence":   completed,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(GetHooksDir(), id+".json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("thread:turn-a", 1, 1)
	first := CodexTurnMarker(id)
	if first == "" {
		t.Fatal("marker for a recorded turn is empty")
	}
	if consumed, err := consumeCodexCompletionEvidence(id, "thread:turn-a"); err != nil || !consumed {
		t.Fatalf("consume turn A: consumed=%v err=%v", consumed, err)
	}
	if got := CodexTurnMarker(id); got != first {
		t.Fatalf("consuming turn evidence moved the marker: %q -> %q", first, got)
	}
	write("thread:turn-b", 2, 2)
	if got := CodexTurnMarker(id); got == first {
		t.Fatalf("a new turn left the marker at %q", got)
	}
}

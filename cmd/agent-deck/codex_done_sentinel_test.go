package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// A Codex worker asserts completion exactly like a Claude worker: the final
// line of its final assistant message is the sentinel. Codex has no Stop-hook
// transcript; its agent-turn-complete notify carries that message as
// last-assistant-message.
func TestCodexNotifyDoneSignal(t *testing.T) {
	payload := func(msg string) []byte {
		return []byte(`{"type":"agent-turn-complete","thread-id":"t","turn-id":"u","last-assistant-message":` + jsonString(msg) + `}`)
	}
	cases := []struct {
		name, msg       string
		ok              bool
		status, summary string
	}{
		{"ok", "Done.\n===AGENTDECK_DONE=== status=ok summary=shipped it", true, "ok", "shipped it"},
		{"fail", "Blocked.\n===AGENTDECK_DONE=== status=fail summary=no creds\n", true, "fail", "no creds"},
		{"no sentinel", "Still working on it.", false, "", ""},
		{"quoted mid-message", "I will end with:\n===AGENTDECK_DONE=== status=ok summary=later\nbut not yet.", false, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sig, ok := codexNotifyDoneSignal(payload(tc.msg))
			if ok != tc.ok || sig.Status != tc.status || sig.Summary != tc.summary {
				t.Fatalf("got %+v ok=%v, want status=%q summary=%q ok=%v", sig, ok, tc.status, tc.summary, tc.ok)
			}
		})
	}
	if _, ok := codexNotifyDoneSignal([]byte(`{"type":"agent-turn-complete"}`)); ok {
		t.Fatal("payload without last-assistant-message must not complete")
	}
}

// End to end: codex-notify with a sentinel-bearing turn completion produces the
// same finished inbox event a Claude Stop hook does.
func TestCodexNotifySentinelReachesParentInbox(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	for _, key := range []string{"XDG_CACHE_HOME", "XDG_STATE_HOME", "AGENT_DECK_HOME", "CLAUDE_CONFIG_DIR", "CODEX_SESSION_ID", "AGENTDECK_HOOK_GENERATION"} {
		t.Setenv(key, "")
	}
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	const profile = "codex_done_test"
	t.Setenv("AGENTDECK_PROFILE", profile)
	session.ClearUserConfigCache()
	t.Cleanup(session.ClearUserConfigCache)

	storage, err := session.NewStorageWithProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	const thread = "01a0cc41-3ddb-7d51-9a8e-6b1f2c3d4e5f"
	// A real main thread has a rollout from turn start; notify drops turn-ends
	// for threads without one (helper threads).
	rolloutDir := filepath.Join(home, ".codex", "sessions", "2026", "10", "05")
	if err := os.MkdirAll(rolloutDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"type":"session_meta","payload":{"id":"` + thread + `","cwd":"` + home + `","originator":"codex-tui","thread_source":"user"}}` + "\n"
	if err := os.WriteFile(filepath.Join(rolloutDir, "rollout-2026-10-05T00-00-00-"+thread+".jsonl"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	parent := &session.Instance{ID: "parent-codex-done", Title: "parent", ProjectPath: home, GroupPath: session.DefaultGroupPath, Tool: "claude", Status: session.StatusIdle, CreatedAt: now}
	child := &session.Instance{ID: "child-codex-done", Title: "codex worker", ProjectPath: home, GroupPath: session.DefaultGroupPath, Tool: "codex", ParentSessionID: parent.ID, CodexSessionID: thread, Status: session.StatusRunning, CreatedAt: now}
	if err := storage.SaveWithGroups([]*session.Instance{parent, child}, nil); err != nil {
		t.Fatal(err)
	}

	t.Setenv("AGENTDECK_INSTANCE_ID", child.ID)
	origArgs := os.Args
	t.Cleanup(func() { os.Args = origArgs })
	os.Args = []string{"agent-deck", "codex-notify", `{"type":"agent-turn-complete","thread-id":"` + thread + `","turn-id":"turn-1",` +
		`"last-assistant-message":"All green.\n===AGENTDECK_DONE=== status=ok summary=codex finished"}`}
	handleCodexNotify()

	d := session.NewTransitionDaemon()
	d.SyncOnce(context.Background())
	d.Flush()

	events, err := session.DrainInboxForParent(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.ChildSessionID == child.ID && ev.Kind == "finished" && ev.DoneStatus == "ok" && ev.DoneSummary == "codex finished" {
			return
		}
	}
	t.Fatalf("no finished event for codex child; drained %+v", events)
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

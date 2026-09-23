package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

func TestCodexHookOverlayRequiresOwnedUserRollout(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	session.ClearUserConfigCache()
	t.Cleanup(session.ClearUserConfigCache)
	accountHome := t.TempDir()
	if err := session.SaveUserConfig(&session.UserConfig{Profiles: map[string]session.ProfileSettings{
		"work": {Codex: session.ProfileCodexSettings{ConfigDir: accountHome}},
	}}); err != nil {
		t.Fatal(err)
	}
	inst := &session.Instance{ID: "web-codex", Tool: "codex", Command: "codex", Account: "work", ProjectPath: t.TempDir(), Status: session.StatusRunning}
	sess := toMenuSession(inst)
	hook := &session.HookStatus{SessionID: "candidate", Status: "waiting", Event: "agent-turn-complete", UpdatedAt: time.Now()}
	applyHookStatusToMenuSession(sess, hook, time.Now())
	if sess.Status != session.StatusRunning {
		t.Fatal("missing rollout changed web status")
	}
	path := filepath.Join(accountHome, "sessions", "2026", "09", "22", "rollout-test-candidate.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"subagent", "user"} {
		data, err := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{
			"id": "candidate", "cwd": inst.ProjectPath, "thread_source": source, "source": "cli",
		}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
		applyHookStatusToMenuSession(sess, hook, time.Now())
		want := session.StatusRunning
		if source == "user" {
			want = session.StatusWaiting
		}
		if sess.Status != want {
			t.Fatalf("%s status = %q, want %q", source, sess.Status, want)
		}
	}
}

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

func seedCodexNotifySession(t *testing.T, instanceID, sessionID string) (*session.Storage, *session.Instance) {
	t.Helper()
	home := os.Getenv("HOME")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	t.Setenv("AGENTDECK_PROFILE", "_test-notify-validation")
	session.ClearUserConfigCache()
	t.Cleanup(session.ClearUserConfigCache)
	storage, err := session.NewStorageWithProfile("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	inst := &session.Instance{ID: instanceID, Title: "notify", ProjectPath: home, Tool: "codex", Command: "codex", Status: session.StatusRunning, CreatedAt: time.Now()}
	row := &statedb.InstanceRow{ID: inst.ID, Title: inst.Title, ProjectPath: inst.ProjectPath, Tool: inst.Tool, Command: inst.Command, Status: "running", CreatedAt: inst.CreatedAt}
	if err := storage.GetDB().SaveInstance(row); err != nil {
		t.Fatal(err)
	}
	writeNotifyRollout(t, os.Getenv("CODEX_HOME"), sessionID, map[string]any{"id": sessionID, "cwd": inst.ProjectPath, "thread_source": "user", "source": "cli"})
	return storage, inst
}

func writeNotifyRollout(t *testing.T, home, id string, payload map[string]any) string {
	t.Helper()
	path := filepath.Join(home, "sessions", "2026", "09", "22", "rollout-test-"+id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{"type": "session_meta", "payload": payload})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCodexNotifyRejectsInvalidCandidatesWithoutChangingFiles(t *testing.T) {
	for _, kind := range []string{"missing", "malformed", "wrong_id", "wrong_project", "subagent", "temporary"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("AGENTDECK_HOOKS_DIR", filepath.Join(t.TempDir(), "hooks"))
			storage, inst := seedCodexNotifySession(t, "notify", "real-new")
			writeCodexHookStatus(inst.ID, "waiting", "real-new", "agent-turn-complete", "real-turn")
			path := filepath.Join(getHooksDir(), inst.ID+".json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			payload := map[string]any{"id": "invalid", "cwd": inst.ProjectPath, "thread_source": "user", "source": "cli"}
			switch kind {
			case "wrong_id":
				payload["id"] = "other"
			case "wrong_project":
				payload["cwd"] = t.TempDir()
			case "subagent":
				payload["thread_source"] = "subagent"
			case "temporary":
				payload["ephemeral"] = true
			}
			if kind != "missing" {
				rollout := writeNotifyRollout(t, os.Getenv("CODEX_HOME"), "invalid", payload)
				if kind == "malformed" {
					if err := os.WriteFile(rollout, []byte(`{"type":`), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, status := range []string{"running", "waiting"} {
				writeCodexHookStatus(inst.ID, status, "invalid", "agent-turn-complete", "title-turn")
				after, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(before, after) {
					t.Fatal("invalid notification changed hook or completion evidence")
				}
				if got := session.ReadHookSessionAnchor(inst.ID); got != "real-new" {
					t.Fatalf("anchor changed: %q", got)
				}
			}
			// Exercise the owning DB binder using the actual surviving hook,
			// then reload the profile as restart/another daemon would.
			var raw hookStatusFile
			if err := json.Unmarshal(before, &raw); err != nil {
				t.Fatal(err)
			}
			hook := &session.HookStatus{Status: raw.Status, SessionID: raw.SessionID, Event: raw.Event, UpdatedAt: time.Unix(raw.Timestamp, 0), CodexStartedGeneration: raw.CodexStartedGeneration, CodexCompletedGeneration: raw.CodexCompletedGeneration, CodexStartedSessionID: raw.CodexStartedSessionID, CodexCompletedSessionID: raw.CodexCompletedSessionID}
			inst.UpdateHookStatusWithDB(hook, storage.GetDB())
			reloaded, err := storage.Load()
			if err != nil {
				t.Fatal(err)
			}
			if reloaded[0].CodexSessionID != "real-new" || raw.CodexCompletedGeneration != "real-new:real-turn" {
				t.Fatal("binding or completion lost across reload")
			}
		})
	}
}

func TestCodexNotifyReconsidersUnflushedRollout(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AGENTDECK_HOOKS_DIR", filepath.Join(t.TempDir(), "hooks"))
	_, inst := seedCodexNotifySession(t, "delayed", "old")
	writeCodexHookStatus(inst.ID, "waiting", "old", "agent-turn-complete", "old-turn")
	writeCodexHookStatus(inst.ID, "running", "new", "turn.started", "new-turn")
	if got := session.ReadHookSessionAnchor(inst.ID); got != "old" {
		t.Fatalf("unflushed binding = %q", got)
	}
	writeNotifyRollout(t, os.Getenv("CODEX_HOME"), "new", map[string]any{"id": "new", "cwd": inst.ProjectPath, "source": "cli"})
	writeCodexHookStatus(inst.ID, "waiting", "new", "agent-turn-complete", "new-turn")
	if got := session.ReadHookSessionAnchor(inst.ID); got != "new" {
		t.Fatalf("flushed binding = %q", got)
	}
}

func TestCodexNotifyUsesRecordedAccountAndProfile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AGENTDECK_HOOKS_DIR", filepath.Join(t.TempDir(), "hooks"))
	storage, inst := seedCodexNotifySession(t, "account-owner", "new")
	accountHome := t.TempDir()
	cfg := &session.UserConfig{Profiles: map[string]session.ProfileSettings{"account": {Codex: session.ProfileCodexSettings{ConfigDir: accountHome}}}}
	if err := session.SaveUserConfig(cfg); err != nil {
		t.Fatal(err)
	}
	row, err := storage.GetDB().LoadInstanceByID(inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	row.Account = "account"
	if err := storage.GetDB().SaveInstance(row); err != nil {
		t.Fatal(err)
	}
	// The ambient home has a valid-looking same-ID rollout, but the selected
	// account has not flushed it yet. It must not qualify.
	writeCodexHookStatus(inst.ID, "waiting", "new", "agent-turn-complete", "turn")
	if got := session.ReadHookSessionAnchor(inst.ID); got != "" {
		t.Fatalf("ambient home accepted: %q", got)
	}
	writeNotifyRollout(t, accountHome, "new", map[string]any{"id": "new", "cwd": inst.ProjectPath, "source": "cli"})
	writeCodexHookStatus(inst.ID, "waiting", "new", "agent-turn-complete", "turn")
	if got := session.ReadHookSessionAnchor(inst.ID); got != "new" {
		t.Fatalf("account home rejected: %q", got)
	}
	t.Setenv("AGENTDECK_PROFILE", "_test-nonexistent-notify")
	writeCodexHookStatus(inst.ID, "running", "new", "turn.started", "another")
	data, err := os.ReadFile(filepath.Join(getHooksDir(), inst.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw hookStatusFile
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if raw.Status != "waiting" {
		t.Fatal("missing owning profile fell back to another database")
	}
}

func TestCodexNotifyUsesMultiRepoWorkingDirectory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AGENTDECK_HOOKS_DIR", filepath.Join(t.TempDir(), "hooks"))
	storage, _ := seedCodexNotifySession(t, "multi", "old")
	loaded, err := storage.Load()
	if err != nil {
		t.Fatal(err)
	}
	inst := loaded[0]
	inst.MultiRepoEnabled, inst.MultiRepoTempDir = true, t.TempDir()
	if err := storage.Save([]*session.Instance{inst}); err != nil {
		t.Fatal(err)
	}
	writeNotifyRollout(t, os.Getenv("CODEX_HOME"), "multi-new", map[string]any{"id": "multi-new", "cwd": inst.EffectiveWorkingDir(), "source": "cli"})
	writeCodexHookStatus(inst.ID, "waiting", "multi-new", "agent-turn-complete", "turn")
	if got := session.ReadHookSessionAnchor(inst.ID); got != "multi-new" {
		t.Fatalf("multi-repo launch directory rejected: %q", got)
	}
}

package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

func writeCodexCandidate(t *testing.T, home, id string, payload map[string]any) string {
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

func seedValidCodexCandidate(t *testing.T, inst *Instance, id string) {
	t.Helper()
	t.Setenv("CODEX_HOME", t.TempDir())
	writeCodexCandidate(t, inst.getCodexHomeDir(), id, map[string]any{
		"id": id, "cwd": inst.ProjectPath, "thread_source": "user", "source": "cli",
	})
}

func TestCodexConversationCandidateMetadata(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(map[string]any)
		want bool
	}{
		{"user", func(map[string]any) {}, true},
		{"legacy_cli", func(p map[string]any) { delete(p, "thread_source") }, true},
		{"legacy_exec", func(p map[string]any) { delete(p, "thread_source"); p["source"] = "exec" }, true},
		{"user_without_source", func(p map[string]any) { delete(p, "source") }, true},
		{"missing_origin", func(p map[string]any) { delete(p, "source"); delete(p, "thread_source") }, false},
		{"wrong_id", func(p map[string]any) { p["id"] = "another" }, false},
		{"wrong_project", func(p map[string]any) { p["cwd"] = "/other" }, false},
		{"missing_cwd", func(p map[string]any) { delete(p, "cwd") }, false},
		{"subagent", func(p map[string]any) { p["thread_source"] = "subagent" }, false},
		{"guardian", func(p map[string]any) { p["thread_source"] = "guardian_review" }, false},
		{"title", func(p map[string]any) { p["thread_source"] = "title_generation" }, false},
		{"ephemeral", func(p map[string]any) { p["ephemeral"] = true }, false},
		{"parent", func(p map[string]any) { p["parent_thread_id"] = "parent" }, false},
		{"legacy_subagent", func(p map[string]any) { delete(p, "thread_source"); p["source"] = map[string]any{"subagent": "review"} }, false},
		{"conflicting_source", func(p map[string]any) { p["source"] = map[string]any{"subagent": "review"} }, false},
		{"malformed_source", func(p map[string]any) { p["source"] = 42 }, false},
		{"malformed_id", func(p map[string]any) { p["id"] = 42 }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := &Instance{Tool: "codex", ProjectPath: t.TempDir()}
			t.Setenv("CODEX_HOME", t.TempDir())
			payload := map[string]any{"id": "candidate", "cwd": inst.ProjectPath, "thread_source": "user", "source": "cli"}
			tc.edit(payload)
			writeCodexCandidate(t, inst.getCodexHomeDir(), "candidate", payload)
			if got := inst.ValidCodexConversationCandidate("candidate"); got != tc.want {
				t.Fatalf("valid = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCodexInvalidHookLeavesStateUntouched(t *testing.T) {
	for _, kind := range []string{"missing", "malformed", "subagent", "wrong_project", "same_invalid_id", "invalid_anchor"} {
		t.Run(kind, func(t *testing.T) {
			setupSessionXDGPathEnv(t)
			inst := &Instance{ID: "guard", Tool: "codex", ProjectPath: t.TempDir(), CodexSessionID: "valid", Status: StatusRunning}
			seedValidCodexCandidate(t, inst, "valid")
			inst.hookStatus, inst.hookEvent, inst.hookLastUpdate = "running", "turn.started", time.Now().Add(-time.Minute)
			inst.lastActivityAt = inst.hookLastUpdate
			inst.codexStartedGeneration, inst.codexCompletedGeneration = "valid:turn", "valid:turn"
			inst.codexStartedSessionID, inst.codexCompletedSessionID = "valid", "valid"
			payload := map[string]any{"id": "invalid", "cwd": inst.ProjectPath, "thread_source": "user"}
			if kind == "subagent" {
				payload["thread_source"] = "subagent"
			}
			if kind == "wrong_project" {
				payload["cwd"] = t.TempDir()
			}
			if kind == "malformed" || kind == "subagent" || kind == "wrong_project" {
				path := writeCodexCandidate(t, inst.getCodexHomeDir(), "invalid", payload)
				if kind == "malformed" {
					if err := os.WriteFile(path, []byte(`{"type":`), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if kind == "same_invalid_id" {
				inst.CodexSessionID = "invalid"
			}
			hook := &HookStatus{Status: "waiting", SessionID: "invalid", Event: "agent-turn-complete", UpdatedAt: time.Now(), CodexStartedGeneration: "invalid:title", CodexCompletedGeneration: "invalid:title", CodexStartedSessionID: "invalid", CodexCompletedSessionID: "invalid"}
			if kind == "invalid_anchor" {
				WriteHookSessionAnchor(inst.ID, "invalid")
				hook.SessionID = ""
			}
			before := []any{inst.CodexSessionID, inst.CodexDetectedAt, inst.hookStatus, inst.hookEvent, inst.hookLastUpdate, inst.lastActivityAt, inst.Status, inst.codexStartedGeneration, inst.codexCompletedGeneration, inst.codexStartedSessionID, inst.codexCompletedSessionID}
			inst.UpdateHookStatusWithDB(hook, nil)
			after := []any{inst.CodexSessionID, inst.CodexDetectedAt, inst.hookStatus, inst.hookEvent, inst.hookLastUpdate, inst.lastActivityAt, inst.Status, inst.codexStartedGeneration, inst.codexCompletedGeneration, inst.codexStartedSessionID, inst.codexCompletedSessionID}
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("invalid hook changed state: before=%v after=%v", before, after)
			}
		})
	}
}

func TestCodexValidatedBindingUsesOwningProfileAndAccount(t *testing.T) {
	for _, foreignGlobal := range []bool{false, true} {
		t.Run(map[bool]string{false: "no_global", true: "foreign_global"}[foreignGlobal], func(t *testing.T) {
			setupSessionXDGPathEnv(t)
			accountHome := codexAccountHome(t)
			prior := statedb.GetGlobal()
			statedb.SetGlobal(nil)
			t.Cleanup(func() { statedb.SetGlobal(prior) })
			owner, other := newTestStorage(t), newTestStorage(t)
			inst := &Instance{ID: "codex-owner", Title: "test", Tool: "codex", Command: "codex", Account: "work", ProjectPath: t.TempDir(), CodexSessionID: "old", Status: StatusRunning, CreatedAt: time.Now()}
			for _, s := range []*Storage{owner, other} {
				if err := s.Save([]*Instance{inst}); err != nil {
					t.Fatal(err)
				}
			}
			if foreignGlobal {
				statedb.SetGlobal(other.GetDB())
			}
			writeCodexCandidate(t, accountHome, "new", map[string]any{"id": "new", "cwd": inst.ProjectPath, "source": "cli", "thread_source": "user"})
			// The same ID under an unrelated home is a subagent. Validation must
			// neither read it nor reuse metadata cached under just the ID.
			t.Setenv("CODEX_HOME", t.TempDir())
			writeCodexCandidate(t, os.Getenv("CODEX_HOME"), "new", map[string]any{"id": "new", "cwd": inst.ProjectPath, "thread_source": "subagent"})
			hook := &HookStatus{Status: "waiting", SessionID: "new", Event: "agent-turn-complete", UpdatedAt: time.Now(), CodexStartedGeneration: "new:turn", CodexCompletedGeneration: "new:turn", CodexStartedSessionID: "new", CodexCompletedSessionID: "new"}
			inst.UpdateHookStatusWithDB(hook, owner.GetDB())
			inst.UpdateHookStatusWithDB(&HookStatus{Status: "running", SessionID: "temporary-title", Event: "agent-turn-complete", UpdatedAt: time.Now()}, owner.GetDB())
			if !inst.codexCompletionConverged() {
				t.Fatal("title event erased valid completion")
			}
			if got := readCodexSessionIDFromDB(t, other.GetDB(), inst.ID); got != "old" {
				t.Fatalf("foreign profile changed: %q", got)
			}
			reloaded, err := owner.Load()
			if err != nil {
				t.Fatal(err)
			}
			if reloaded[0].CodexSessionID != "new" {
				t.Fatalf("reload lost binding: %q", reloaded[0].CodexSessionID)
			}
			reloaded[0].UpdateHookStatusWithDB(hook, owner.GetDB())
			if !reloaded[0].codexCompletionConverged() {
				t.Fatal("completion did not survive reload and hook read")
			}
		})
	}
}

func TestCodexInvalidHookDoesNotEmitDaemonCompletion(t *testing.T) {
	const profile = "_test_codex_invalid_completion"
	d, storage := bootstrapDaemonProfile(t, profile)
	parent, child := seedStaleRowFixture(t, storage, "codex-child", "codex-parent", "running")
	child.Tool, child.Command, child.CodexSessionID = "codex", "codex", "valid"
	seedValidCodexCandidate(t, child, "valid")
	if err := storage.Save([]*Instance{parent, child}); err != nil {
		t.Fatal(err)
	}
	seedHookStatusFile(t, child.ID, "agent-turn-complete", "temporary-title", "waiting")
	d.syncProfile(profile)
	inbox, err := DrainInboxForParent(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(inbox) != 0 {
		t.Fatalf("invalid hook delivered %d completion events", len(inbox))
	}
	if got := readCodexSessionIDFromDB(t, storage.GetDB(), child.ID); got != "valid" {
		t.Fatalf("invalid hook changed binding: %q", got)
	}
}

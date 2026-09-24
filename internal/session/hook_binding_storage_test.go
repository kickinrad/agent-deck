package session

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
)

// The standalone notifier and CLI load a profile without installing the TUI's
// global DB. A rebind must reach that profile even if the global is absent or
// belongs to another profile with the same instance ID.
func TestHookRebindPersistsToOwningStorage(t *testing.T) {
	for _, foreignGlobal := range []bool{false, true} {
		name := "no_global"
		if foreignGlobal {
			name = "other_profile_global"
		}
		t.Run(name, func(t *testing.T) {
			setupSessionXDGPathEnv(t)
			t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), ".claude"))
			ClearUserConfigCache()
			t.Cleanup(ClearUserConfigCache)
			prior := statedb.GetGlobal()
			statedb.SetGlobal(nil)
			t.Cleanup(func() { statedb.SetGlobal(prior) })
			owner := newTestStorage(t)
			other := newTestStorage(t)
			const oldID = "5ea244ce-0000-0000-0000-0000000000ca"
			const newID = "2266314c-0000-0000-0000-0000000000cb"
			row := &statedb.InstanceRow{ID: "rebind-owner", Title: "rebind", ProjectPath: t.TempDir(), GroupPath: DefaultGroupPath, Tool: "claude", Command: "claude", Status: "idle", CreatedAt: time.Now(), ToolData: json.RawMessage(`{"claude_session_id":"` + oldID + `","preserve":"unchanged"}`)}
			for _, s := range []*Storage{owner, other} {
				if err := s.GetDB().SaveInstance(row); err != nil {
					t.Fatal(err)
				}
			}
			if foreignGlobal {
				statedb.SetGlobal(other.GetDB())
			}
			instances, err := owner.Load()
			if err != nil {
				t.Fatal(err)
			}
			inst := instances[0]
			seedClaudeJSONL(t, inst, oldID, 1, 8)
			seedClaudeJSONL(t, inst, newID, 200, 1024)
			inst.UpdateHookStatusWithDB(&HookStatus{Status: "waiting", SessionID: newID, Event: "SessionStart", Cwd: inst.ProjectPath, UpdatedAt: time.Now()}, owner.GetDB())
			if got := readClaudeSessionIDFromDB(t, owner.GetDB(), inst.ID); got != newID {
				t.Errorf("owning profile restart binding = %q, want %q", got, newID)
			}
			if got := readClaudeSessionIDFromDB(t, other.GetDB(), inst.ID); got != oldID {
				t.Errorf("unrelated profile changed to %q, want %q", got, oldID)
			}
			links, err := owner.GetDB().ListSessionLinks(inst.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(links) == 0 || links[len(links)-1].NativeID != newID {
				t.Errorf("owning profile session links = %+v, want a row for %s", links, newID)
			}
			rows, err := owner.GetDB().LoadInstances()
			if err != nil {
				t.Fatal(err)
			}
			var toolData map[string]any
			if err := json.Unmarshal(rows[0].ToolData, &toolData); err != nil {
				t.Fatal(err)
			}
			if toolData["preserve"] != "unchanged" {
				t.Fatal("rebind overwrote unrelated tool data")
			}
			reloaded, err := owner.Load()
			if err != nil {
				t.Fatal(err)
			}
			if reloaded[0].ClaudeSessionID != newID {
				t.Errorf("reload resumed %q, want the rebound conversation", reloaded[0].ClaudeSessionID)
			}
		})
	}
}

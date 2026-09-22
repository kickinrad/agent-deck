package session

import (
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/statedb"
	"github.com/stretchr/testify/require"
)

func TestDefaultGroupMigrationConsolidatesLegacyMembership(t *testing.T) {
	s := newTestStorage(t)
	require.NoError(t, s.db.SaveGroups([]*statedb.GroupRow{
		{Path: "my-sessions", Name: "My Sessions", MaxConcurrent: 2},
		{Path: "sessions", Name: "sessions", MaxConcurrent: 7},
	}))
	require.NoError(t, s.db.SaveInstance(&statedb.InstanceRow{ID: "active", Title: "active", Tool: "shell", ProjectPath: t.TempDir(), GroupPath: "my-sessions"}))
	require.NoError(t, s.db.SaveInstance(&statedb.InstanceRow{ID: "archived", Title: "archived", Tool: "shell", ProjectPath: t.TempDir(), GroupPath: "My Sessions"}))

	require.NoError(t, s.db.Migrate())
	groups, err := s.db.LoadGroups()
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.Equal(t, "sessions", groups[0].Path)
	require.Equal(t, 7, groups[0].MaxConcurrent, "existing sessions settings win")
	for _, id := range []string{"active", "archived"} {
		row, err := s.db.LoadInstanceByID(id)
		require.NoError(t, err)
		require.Equal(t, "sessions", row.GroupPath)
	}
}

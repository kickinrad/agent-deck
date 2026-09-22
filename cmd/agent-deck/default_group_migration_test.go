package main

import (
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
	"github.com/stretchr/testify/require"
)

func TestFleetRecoverSessionIDFilterRequiresExactKnownIDs(t *testing.T) {
	instances := []*session.Instance{{ID: "one"}, {ID: "two"}}
	got, err := filterFleetRecoverySessions(instances, []string{"two", "two"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "two", got[0].ID)
	_, err = filterFleetRecoverySessions(instances, []string{"tw"})
	require.ErrorContains(t, err, "was not found")
}

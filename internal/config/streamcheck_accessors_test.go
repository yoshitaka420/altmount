package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStreamCheckStreamBlocklistAccessorsUseBackendDefaults(t *testing.T) {
	disabled := false
	cfg := DefaultConfig(t.TempDir())
	cfg.StreamCheck.StreamBlocklist.Enabled = &disabled
	cfg.StreamCheck.StreamBlocklist.Quorum = 5
	cfg.StreamCheck.StreamBlocklist.MaxSourceEntries = 1234
	cfg.StreamCheck.StreamBlocklist.BackboneScope = &disabled
	cfg.StreamCheck.StreamBlocklist.MarkDead = &disabled

	require.True(t, cfg.GetStreamCheckStreamBlocklistEnabled())
	require.Equal(t, StreamCheckStreamBlocklistDefaultQuorum, cfg.GetStreamCheckStreamBlocklistQuorum())
	require.Equal(t, StreamCheckStreamBlocklistDefaultMaxSourceEntries, cfg.GetStreamCheckStreamBlocklistMaxSourceEntries())
	require.True(t, cfg.GetStreamCheckStreamBlocklistBackboneScopeEnabled())
	require.True(t, cfg.GetStreamCheckStreamBlocklistMarkDead())

	require.NoError(t, cfg.Validate())
	require.True(t, *cfg.StreamCheck.StreamBlocklist.Enabled)
	require.Equal(t, StreamCheckStreamBlocklistDefaultQuorum, cfg.StreamCheck.StreamBlocklist.Quorum)
	require.Equal(t, StreamCheckStreamBlocklistDefaultMaxSourceEntries, cfg.StreamCheck.StreamBlocklist.MaxSourceEntries)
	require.True(t, *cfg.StreamCheck.StreamBlocklist.BackboneScope)
	require.True(t, *cfg.StreamCheck.StreamBlocklist.MarkDead)
}

func TestStreamCheckStreamBlocklistDefaultsFillOnlyMissingValues(t *testing.T) {
	cfg := DefaultConfig(t.TempDir())
	cfg.StreamCheck.StreamBlocklist.Enabled = nil
	cfg.StreamCheck.StreamBlocklist.Quorum = 0
	cfg.StreamCheck.StreamBlocklist.MaxSourceEntries = 0
	cfg.StreamCheck.StreamBlocklist.BackboneScope = nil
	cfg.StreamCheck.StreamBlocklist.MarkDead = nil

	require.NoError(t, cfg.Validate())
	require.True(t, *cfg.StreamCheck.StreamBlocklist.Enabled)
	require.Equal(t, StreamCheckStreamBlocklistDefaultQuorum, cfg.StreamCheck.StreamBlocklist.Quorum)
	require.Equal(t, StreamCheckStreamBlocklistDefaultMaxSourceEntries, cfg.StreamCheck.StreamBlocklist.MaxSourceEntries)
	require.True(t, *cfg.StreamCheck.StreamBlocklist.BackboneScope)
	require.True(t, *cfg.StreamCheck.StreamBlocklist.MarkDead)
}

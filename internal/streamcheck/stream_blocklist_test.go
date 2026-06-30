package streamcheck

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/javi11/altmount/internal/config"
	"github.com/stretchr/testify/require"
)

func TestComputeStreamBlocklistFingerprint(t *testing.T) {
	got := ComputeStreamBlocklistFingerprint(12345, " Poster@Example.COM ", 3*86400+123)
	require.Equal(t, "wd1:1a7ac611433a9ce80ca1da4539200b55", got)
}

func TestStreamBlocklistStoreImportAndLookup(t *testing.T) {
	ctx := context.Background()
	enabled := true
	cfg := config.DefaultConfig(t.TempDir())
	cfg.StreamCheck.StreamBlocklist.Enabled = &enabled
	cfg.StreamCheck.StreamBlocklist.DBPath = t.TempDir() + "/stream_blocklist.db"
	cfg.StreamCheck.StreamBlocklist.BackboneScope = &enabled
	cfg.Providers = []config.ProviderConfig{{Host: "news.example.com"}}

	store, err := NewStreamBlocklistStore(func() *config.Config { return cfg })
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	fp := ComputeStreamBlocklistFingerprint(1000, "poster@example.com", time.Unix(172800, 0).Unix())
	body := []byte(`{"stream_blocklist":1,"updated":1}
{"fp":"` + fp + `","deadAt":123,"n":1,"bk":["example.com"]}
`)
	count, err := store.MergeIntoLocal(ctx, bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, 1, count)
	require.True(t, store.IsDeadAnywhere(ctx, fp))
	require.Equal(t, 1, store.LocalCount(ctx))

	var exported bytes.Buffer
	require.NoError(t, store.ExportTo(ctx, &exported, nil, true))
	require.Contains(t, exported.String(), fp)
}

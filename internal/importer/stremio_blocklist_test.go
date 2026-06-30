package importer

import (
	"encoding/json"
	"testing"

	"github.com/javi11/altmount/internal/database"
	"github.com/stretchr/testify/require"
)

func TestIsStremioQueueItemMatchesDownloadIDCategoryAndMetadata(t *testing.T) {
	downloadID := "stremio:abc"
	require.True(t, isStremioQueueItem(&database.ImportQueueItem{DownloadID: &downloadID}))

	category := "Stremio"
	require.True(t, isStremioQueueItem(&database.ImportQueueItem{Category: &category}))

	metadata := `{"source":"stremio"}`
	require.True(t, isStremioQueueItem(&database.ImportQueueItem{Metadata: &metadata}))

	regularDownloadID := "sabnzbd:abc"
	regularCategory := "movies"
	regularMetadata := `{"source":"regular"}`
	require.False(t, isStremioQueueItem(&database.ImportQueueItem{
		DownloadID: &regularDownloadID,
		Category:   &regularCategory,
		Metadata:   &regularMetadata,
	}))
}

func TestStremioIdentityFromQueueMetadataUsesUsenetDateThenDate(t *testing.T) {
	metadata := `{"source":"stremio","size":12345,"poster":"Poster@Example.COM","usenet_date":45678,"date":1}`
	identity := stremioIdentityFromQueueMetadata(&metadata)

	require.Equal(t, int64(12345), identity.Size)
	require.Equal(t, "Poster@Example.COM", identity.Poster)
	require.Equal(t, int64(45678), identity.UsenetDate)

	fallbackMetadata := `{"source":"stremio","size":98765,"poster":"poster","date":123456}`
	fallbackIdentity := stremioIdentityFromQueueMetadata(&fallbackMetadata)

	require.Equal(t, int64(98765), fallbackIdentity.Size)
	require.Equal(t, "poster", fallbackIdentity.Poster)
	require.Equal(t, int64(123456), fallbackIdentity.UsenetDate)
}

func TestStremioMarkedDeadQueueMetadataPreservesIdentityAndAddsReason(t *testing.T) {
	metadata := `{"source":"stremio","size":12345,"poster":"poster","usenet_date":45678}`
	updated := stremioMarkedDeadQueueMetadata(&metadata)
	require.NotNil(t, updated)

	var parsed stremioImportMetadata
	require.NoError(t, json.Unmarshal([]byte(*updated), &parsed))
	require.Equal(t, "stremio", parsed.Source)
	require.Equal(t, int64(12345), parsed.Size)
	require.Equal(t, "poster", parsed.Poster)
	require.Equal(t, int64(45678), parsed.UsenetDate)
	require.True(t, parsed.StreamBlocklistMarked)
	require.Equal(t, "This failed Stremio import was added to the stream blocklist.", parsed.StreamBlocklistReason)
}

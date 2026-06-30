package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/javi11/altmount/internal/database"
	"github.com/javi11/altmount/internal/metadata"
	"github.com/stretchr/testify/require"
)

func TestBuildStremioStreamsReturnsEverySeasonPackFileWithFilenameIdentity(t *testing.T) {
	server, item := newStremioStreamsTestServer(t, []string{
		"Show.S01E01.mkv",
		"Show.S01E02.mkv",
		"Show.S01E03.mkv",
	})

	streams, err := server.buildStremioStreams(item, "https://alt.example", "download-key", "Release.Name", nil)
	require.NoError(t, err)
	require.Len(t, streams, 3)

	for i, stream := range streams {
		expectedName := []string{"Show.S01E01.mkv", "Show.S01E02.mkv", "Show.S01E03.mkv"}[i]
		require.Equal(t, expectedName, stream.Title)
		require.Equal(t, expectedName, stream.Name)
		require.Contains(t, stream.URL, "download_key=download-key")

		parsedURL, err := url.Parse(stream.URL)
		require.NoError(t, err)
		require.Equal(t, "/api/files/stream", parsedURL.Path)
		require.Equal(t, "/complete/TV/Release.Name/"+expectedName, parsedURL.Query().Get("path"))
	}
}

func TestBuildStremioStreamsFiltersSeasonPackByEpisodeSelector(t *testing.T) {
	server, item := newStremioStreamsTestServer(t, []string{
		"Show.S01E01.mkv",
		"Show.S01E02.mkv",
		"Show.S01E03.mkv",
	})

	selector := &stremioEpisodeSelector{Season: 1, Episode: 2}
	streams, err := server.buildStremioStreams(item, "https://alt.example", "download-key", "Release.Name", selector)
	require.NoError(t, err)
	require.Len(t, streams, 1)
	require.Equal(t, "Show.S01E02.mkv", streams[0].Name)
	require.Equal(t, "Show.S01E02.mkv", streams[0].Title)
}

func TestBuildStremioStreamsFindsMediaFilesInNestedSeasonPackDirectories(t *testing.T) {
	server, item := newStremioStreamsTestServer(t, []string{
		"Season 01/Show.S01E01.mkv",
		"Season 01/Show.S01E02.mkv",
	})

	selector := &stremioEpisodeSelector{Season: 1, Episode: 2}
	streams, err := server.buildStremioStreams(item, "https://alt.example", "download-key", "Release.Name", selector)
	require.NoError(t, err)
	require.Len(t, streams, 1)
	require.Equal(t, "Show.S01E02.mkv", streams[0].Name)

	parsedURL, err := url.Parse(streams[0].URL)
	require.NoError(t, err)
	require.Equal(t, "/complete/TV/Release.Name/Season 01/Show.S01E02.mkv", parsedURL.Query().Get("path"))
}

func TestBuildStremioStreamsReturnsEmptySliceWhenSelectorDoesNotMatch(t *testing.T) {
	server, item := newStremioStreamsTestServer(t, []string{
		"Show.S01E01.mkv",
	})

	selector := &stremioEpisodeSelector{Season: 1, Episode: 2}
	streams, err := server.buildStremioStreams(item, "https://alt.example", "download-key", "Release.Name", selector)
	require.NoError(t, err)
	require.NotNil(t, streams)
	require.Empty(t, streams)
}

func TestStremioEpisodeSelectorMatchesCommonEpisodeFilenameForms(t *testing.T) {
	selector := &stremioEpisodeSelector{Season: 1, Episode: 2}

	require.True(t, selector.matches("Show.S01E02.mkv"))
	require.True(t, selector.matches("Show.s1e2.mkv"))
	require.True(t, selector.matches("Show.1x02.mkv"))
	require.True(t, selector.matches("Show.S01E01E02.mkv"))
	require.False(t, selector.matches("Show.S01E01.mkv"))
	require.False(t, selector.matches("Show.S02E02.mkv"))
}

func TestStreamcheckIdentityFromRequestUsesFormValuesBeforeQuery(t *testing.T) {
	app := fiber.New()
	app.Post("/", func(c *fiber.Ctx) error {
		identity := streamcheckIdentityFromRequest(c)
		require.Equal(t, int64(12345), identity.Size)
		require.Equal(t, "Poster@Example.COM", identity.Poster)
		require.Equal(t, int64(45678), identity.UsenetDate)
		return c.SendStatus(fiber.StatusOK)
	})

	body := strings.NewReader("size=12345&poster=Poster%40Example.COM&date=45678")
	req := httptest.NewRequest(http.MethodPost, "/?size=1&poster=query&usenet_date=2", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func TestStreamcheckIdentityFromRequestUsesQueryUsenetDate(t *testing.T) {
	app := fiber.New()
	app.Get("/", func(c *fiber.Ctx) error {
		identity := streamcheckIdentityFromRequest(c)
		require.Equal(t, int64(98765), identity.Size)
		require.Equal(t, "poster", identity.Poster)
		require.Equal(t, int64(123456), identity.UsenetDate)
		return c.SendStatus(fiber.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/?size=98765&poster=poster&usenet_date=123456", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, fiber.StatusOK, resp.StatusCode)
}

func newStremioStreamsTestServer(t *testing.T, names []string) (*Server, *database.ImportQueueItem) {
	t.Helper()

	root := t.TempDir()
	storagePath := "/complete/TV/Release.Name"
	metadataDir := filepath.Join(root, strings.TrimPrefix(storagePath, "/"))
	require.NoError(t, os.MkdirAll(metadataDir, 0755))
	for _, name := range names {
		metaPath := filepath.Join(metadataDir, name+".meta")
		require.NoError(t, os.MkdirAll(filepath.Dir(metaPath), 0755))
		require.NoError(t, os.WriteFile(metaPath, []byte("test"), 0644))
	}

	server := &Server{metadataService: metadata.NewMetadataService(root)}
	return server, &database.ImportQueueItem{
		ID:          42,
		StoragePath: &storagePath,
	}
}

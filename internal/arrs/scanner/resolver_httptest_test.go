package scanner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/javi11/altmount/internal/arrs/data"
	"github.com/javi11/altmount/internal/arrs/model"
	"github.com/javi11/altmount/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golift.io/starr"
	"golift.io/starr/radarr"
	"golift.io/starr/sonarr"
)

func testGetter() config.ConfigGetter {
	cfg := config.DefaultConfig()
	return func() *config.Config { return cfg }
}

func newResolverManager() *Manager {
	return &Manager{configGetter: testGetter(), data: data.NewManager()}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func newTestRadarr(srv *httptest.Server) *radarr.Radarr {
	return radarr.New(&starr.Config{URL: srv.URL, APIKey: "test", Client: srv.Client()})
}

func newTestSonarr(srv *httptest.Server) *sonarr.Sonarr {
	return sonarr.New(&starr.Config{URL: srv.URL, APIKey: "test", Client: srv.Client()})
}

// --- Radarr resolver ---

func TestResolveRadarrOwnership_IDPrecisionOwned(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/movie/5", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"id": 5, "title": "Movie", "tmdbId": 999, "hasFile": true,
			"movieFile": map[string]any{"id": 50, "path": "/movies/Movie/movie.mkv"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	m := newResolverManager()
	meta := &model.WebhookMetadata{
		Movie:     &model.MovieMetadata{Id: 5},
		MovieFile: &model.MovieFileMetadata{Id: 50},
	}
	res, err := m.resolveRadarrOwnership(context.Background(), newTestRadarr(srv),
		"/movies/Movie/movie.mkv", "", "radarr-1", meta)
	require.NoError(t, err)
	require.NotNil(t, res.movie)
	assert.Equal(t, int64(50), res.movieFileID)
	assert.False(t, res.hasReplacement)
}

func TestResolveRadarrOwnership_IDPrecisionReplacement(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/movie/5", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"id": 5, "title": "Movie", "hasFile": true,
			"movieFile": map[string]any{"id": 99, "path": "/movies/Movie/new.mkv"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	m := newResolverManager()
	// Stored file id 50, but the arr now links file id 99 → replacement.
	meta := &model.WebhookMetadata{
		Movie:     &model.MovieMetadata{Id: 5},
		MovieFile: &model.MovieFileMetadata{Id: 50},
	}
	res, err := m.resolveRadarrOwnership(context.Background(), newTestRadarr(srv),
		"/movies/Movie/old.mkv", "", "radarr-1", meta)
	require.NoError(t, err)
	assert.True(t, res.hasReplacement)
	assert.Equal(t, int64(99), res.replacementFileID)
}

func TestResolveRadarrOwnership_TmdbIDFallback(t *testing.T) {
	mux := http.NewServeMux()
	// Full-list / tmdbId-filtered lookup share the /movie path; differentiate by query.
	mux.HandleFunc("/api/v3/movie", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("tmdbId") == "999" {
			writeJSON(w, []map[string]any{
				{"id": 7, "title": "Re-added Movie", "tmdbId": 999, "hasFile": true,
					"movieFile": map[string]any{"id": 70, "path": "/movies/x.mkv"}},
			})
			return
		}
		writeJSON(w, []map[string]any{}) // no path match
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	m := newResolverManager()
	// No internal id (id-precision skipped); only the stable tmdbId resolves it.
	meta := &model.WebhookMetadata{Movie: &model.MovieMetadata{TmdbId: 999}}
	res, err := m.resolveRadarrOwnership(context.Background(), newTestRadarr(srv),
		"/movies/renamed.mkv", "", "radarr-1", meta)
	require.NoError(t, err)
	require.NotNil(t, res.movie)
	assert.Equal(t, int64(7), res.movie.ID)
	assert.Equal(t, int64(70), res.movieFileID)
}

// --- Sonarr resolver ---

func TestResolveSonarrOwnership_SeasonEpisodeFallback(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/series", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{{"id": 10, "title": "Show", "path": "/tv/Show"}})
	})
	mux.HandleFunc("/api/v3/episodeFile", func(w http.ResponseWriter, _ *http.Request) {
		// A file that does NOT match the request path/filename (stale id + rename).
		writeJSON(w, []map[string]any{{"id": 60, "path": "/tv/Show/old-name.mkv"}})
	})
	mux.HandleFunc("/api/v3/episode", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{
			{"id": 300, "seasonNumber": 1, "episodeNumber": 3, "hasFile": true, "episodeFileId": 77},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	m := newResolverManager()
	res, err := m.resolveSonarrOwnership(context.Background(), newTestSonarr(srv),
		"/tv/Show/Season 01/Show.S01E03.mkv", "", "sonarr-1", &model.WebhookMetadata{})
	require.NoError(t, err)
	assert.True(t, res.seriesFound)
	// Resolved via stable season+episode, not the mutable file id/path.
	assert.Equal(t, int64(77), res.episodeFileID)
	assert.Contains(t, res.episodeIDs, int64(300))
}

// TestResolveSonarrOwnership_NilEpisodeFileGuard exercises the Smart Repair Guard
// path with metadata present but EpisodeFile nil — it must not panic.
func TestResolveSonarrOwnership_NilEpisodeFileGuard(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/series", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []map[string]any{{"id": 10, "title": "Show", "path": "/tv/Show"}})
	})
	mux.HandleFunc("/api/v3/episodeFile", func(w http.ResponseWriter, _ *http.Request) {
		// Matches by FILENAME only (different dir) so episodeFileID is set from path
		// resolution, yet ef.Path != requested path in the replacement scan.
		writeJSON(w, []map[string]any{{"id": 88, "path": "/elsewhere/ep.mkv"}})
	})
	mux.HandleFunc("/api/v3/episode", func(w http.ResponseWriter, _ *http.Request) {
		// No episode links file id 88 → Smart Repair Guard fires.
		writeJSON(w, []map[string]any{
			{"id": 301, "seasonNumber": 9, "episodeNumber": 9, "hasFile": false, "episodeFileId": 0},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	m := newResolverManager()
	// metadata non-nil but EpisodeFile nil: the guard must short-circuit safely.
	require.NotPanics(t, func() {
		res, err := m.resolveSonarrOwnership(context.Background(), newTestSonarr(srv),
			"/tv/Show/Season 01/ep.mkv", "", "sonarr-1", &model.WebhookMetadata{})
		require.NoError(t, err)
		assert.True(t, res.seriesFound)
		assert.Equal(t, int64(88), res.episodeFileID)
		assert.False(t, res.hasReplacement)
	})
}

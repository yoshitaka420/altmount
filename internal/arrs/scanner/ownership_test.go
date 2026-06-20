package scanner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/javi11/altmount/internal/arrs/data"
	"github.com/javi11/altmount/internal/arrs/model"
	"github.com/javi11/altmount/internal/config"
	"golift.io/starr"
	"golift.io/starr/radarr"
	"golift.io/starr/sonarr"
)

// newResolverManager builds a scanner Manager wired with just the read-only
// dependencies the ownership resolvers use (a real data cache + a config
// getter). instances/clients/failures are nil because the resolvers never
// touch them.
func newResolverManager(cfg *config.Config) *Manager {
	if cfg == nil {
		cfg = &config.Config{MountPath: "/mnt"}
	}
	return NewManager(func() *config.Config { return cfg }, nil, nil, data.NewManager(), nil)
}

func newRadarrClient(srv *httptest.Server) *radarr.Radarr {
	return radarr.New(&starr.Config{URL: srv.URL, APIKey: "test", Client: srv.Client()})
}

func newSonarrClient(srv *httptest.Server) *sonarr.Sonarr {
	return sonarr.New(&starr.Config{URL: srv.URL, APIKey: "test", Client: srv.Client()})
}

func TestResolveRadarrOwnership_ByID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// GET /api/v3/movie/42 -> single movie with a current file
		if strings.HasSuffix(r.URL.Path, "/movie/42") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":42,"title":"The Matrix","hasFile":true,"movieFile":{"id":900,"path":"/movies/matrix.mkv"}}`))
			return
		}
		t.Errorf("unexpected request: %s", r.URL.String())
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	mgr := newResolverManager(nil)
	meta := &model.WebhookMetadata{Movie: &model.MovieMetadata{Id: 42}}

	own, err := mgr.resolveRadarrOwnership(context.Background(), newRadarrClient(srv), "/movies/matrix.mkv", "", "radarr1", meta)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if own.movie == nil || own.movie.ID != 42 {
		t.Fatalf("expected movie 42, got %+v", own.movie)
	}
	if own.movieFileID != 900 {
		t.Errorf("movieFileID = %d; want 900", own.movieFileID)
	}
	if own.alreadySatisfied {
		t.Errorf("alreadySatisfied = true; want false")
	}
	if own.lookupErr {
		t.Errorf("lookupErr = true; want false")
	}
}

func TestResolveRadarrOwnership_SmartRepairGuard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/movie/42") {
			// Current file id 900 differs from the metadata file id 111 -> upgraded.
			_, _ = w.Write([]byte(`{"id":42,"title":"The Matrix","hasFile":true,"movieFile":{"id":900,"path":"/movies/matrix-2160p.mkv"}}`))
			return
		}
		t.Errorf("unexpected request: %s", r.URL.String())
	}))
	defer srv.Close()

	mgr := newResolverManager(nil)
	meta := &model.WebhookMetadata{
		Movie:     &model.MovieMetadata{Id: 42},
		MovieFile: &model.MovieFileMetadata{Id: 111},
	}

	own, err := mgr.resolveRadarrOwnership(context.Background(), newRadarrClient(srv), "/movies/matrix.mkv", "", "radarr1", meta)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !own.alreadySatisfied {
		t.Fatalf("expected alreadySatisfied=true (replacement detected), got %+v", own)
	}
}

func TestResolveRadarrOwnership_TMDBFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No id metadata -> targeted tmdb lookup via data manager: GET /movie?tmdbId=603
		if strings.HasSuffix(r.URL.Path, "/movie") && r.URL.Query().Get("tmdbId") == "603" {
			_, _ = w.Write([]byte(`[{"id":7,"title":"The Matrix","tmdbId":603,"hasFile":true,"movieFile":{"id":71,"path":"/movies/matrix.mkv"}}]`))
			return
		}
		t.Errorf("unexpected request: %s", r.URL.String())
	}))
	defer srv.Close()

	mgr := newResolverManager(nil)
	meta := &model.WebhookMetadata{Movie: &model.MovieMetadata{TmdbId: 603}}

	own, err := mgr.resolveRadarrOwnership(context.Background(), newRadarrClient(srv), "/movies/matrix.mkv", "", "radarr1", meta)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if own.movie == nil || own.movie.ID != 7 {
		t.Fatalf("expected movie 7 via tmdb lookup, got %+v", own.movie)
	}
	if own.movieFileID != 71 {
		t.Errorf("movieFileID = %d; want 71", own.movieFileID)
	}
}

func TestResolveRadarrOwnership_PathMatchByFilename(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No metadata -> full-list scan: GET /movie (no tmdbId)
		if strings.HasSuffix(r.URL.Path, "/movie") && r.URL.Query().Get("tmdbId") == "" {
			_, _ = w.Write([]byte(`[{"id":3,"title":"Heat","hasFile":true,"movieFile":{"id":33,"path":"/library/Heat (1995)/Heat.mkv"}}]`))
			return
		}
		t.Errorf("unexpected request: %s", r.URL.String())
	}))
	defer srv.Close()

	mgr := newResolverManager(nil)

	own, err := mgr.resolveRadarrOwnership(context.Background(), newRadarrClient(srv), "/some/other/path/Heat.mkv", "", "radarr1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if own.movie == nil || own.movie.ID != 3 {
		t.Fatalf("expected movie 3 by filename match, got %+v", own.movie)
	}
	if own.movieFileID != 33 {
		t.Errorf("movieFileID = %d; want 33", own.movieFileID)
	}
}

func TestResolveRadarrOwnership_LookupErrorFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Targeted id lookup errors out.
		if strings.HasSuffix(r.URL.Path, "/movie/99") {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		// Full-list scan returns nothing (no path match either).
		if strings.HasSuffix(r.URL.Path, "/movie") {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		t.Errorf("unexpected request: %s", r.URL.String())
	}))
	defer srv.Close()

	mgr := newResolverManager(nil)
	meta := &model.WebhookMetadata{Movie: &model.MovieMetadata{Id: 99}}

	own, err := mgr.resolveRadarrOwnership(context.Background(), newRadarrClient(srv), "/movies/unknown.mkv", "", "radarr1", meta)
	if err != nil {
		t.Fatalf("unexpected error (path scan succeeded): %v", err)
	}
	if own.movie != nil {
		t.Fatalf("expected no movie, got %+v", own.movie)
	}
	if !own.lookupErr {
		t.Errorf("lookupErr = false; want true (a lookup failed, so callers must fail closed)")
	}
}

func TestResolveSonarrOwnership_ByID(t *testing.T) {
	// With both series and episode-file IDs in metadata, no HTTP calls happen.
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		t.Errorf("unexpected request: %s", r.URL.String())
	}))
	defer srv.Close()

	mgr := newResolverManager(nil)
	meta := &model.WebhookMetadata{
		Series:      &model.SeriesMetadata{Id: 12},
		EpisodeFile: &model.EpisodeFileMetadata{Id: 555},
	}

	own, err := mgr.resolveSonarrOwnership(context.Background(), newSonarrClient(srv), "/tv/Show/S01E01.mkv", "", "sonarr1", meta)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if own.seriesID != 12 || own.episodeFileID != 555 {
		t.Fatalf("seriesID=%d episodeFileID=%d; want 12/555", own.seriesID, own.episodeFileID)
	}
	if called {
		t.Errorf("expected no HTTP calls for ID-based resolution")
	}
}

func TestResolveSonarrOwnership_PathMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/series"):
			_, _ = w.Write([]byte(`[{"id":12,"title":"Breaking Bad","path":"/tv/Breaking Bad"}]`))
		case strings.HasSuffix(r.URL.Path, "/episodeFile") && r.URL.Query().Get("seriesId") == "12":
			_, _ = w.Write([]byte(`[{"id":777,"path":"/tv/Breaking Bad/Season 01/Breaking Bad S01E01.mkv"}]`))
		default:
			t.Errorf("unexpected request: %s", r.URL.String())
		}
	}))
	defer srv.Close()

	mgr := newResolverManager(nil)

	own, err := mgr.resolveSonarrOwnership(context.Background(), newSonarrClient(srv),
		"/tv/Breaking Bad/Season 01/Breaking Bad S01E01.mkv", "", "sonarr1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if own.seriesID != 12 {
		t.Fatalf("seriesID = %d; want 12", own.seriesID)
	}
	if own.episodeFileID != 777 {
		t.Errorf("episodeFileID = %d; want 777", own.episodeFileID)
	}
}

func TestResolveSonarrOwnership_NoSeriesMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/series") {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		t.Errorf("unexpected request: %s", r.URL.String())
	}))
	defer srv.Close()

	mgr := newResolverManager(nil)

	own, err := mgr.resolveSonarrOwnership(context.Background(), newSonarrClient(srv), "/tv/Unknown/file.mkv", "", "sonarr1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if own.seriesID != 0 {
		t.Fatalf("seriesID = %d; want 0 (no match)", own.seriesID)
	}
}

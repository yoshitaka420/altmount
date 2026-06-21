package data

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golift.io/starr"
	"golift.io/starr/radarr"
)

// newTestRadarr spins up a fake Radarr backed by the given handler and returns a
// starr Radarr client pointed at it.
func newTestRadarr(t *testing.T, handler http.HandlerFunc) (*radarr.Radarr, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := radarr.New(&starr.Config{URL: server.URL, APIKey: "test", Client: server.Client()})
	return client, server
}

func TestGetMovieByID_TargetedLookup(t *testing.T) {
	var requestedPath string
	client, _ := newTestRadarr(t, func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":42,"title":"The Matrix","tmdbId":603,"hasFile":true,"movieFile":{"id":7}}`))
	})

	m := NewManager()
	movie, err := m.GetMovieByID(context.Background(), client, "radarr1", 42)
	if err != nil {
		t.Fatalf("GetMovieByID returned error: %v", err)
	}
	if movie == nil || movie.ID != 42 {
		t.Fatalf("expected movie ID 42, got %+v", movie)
	}
	// The fix is the whole point: it must hit the targeted /movie/{id} endpoint,
	// never the full /movie list.
	if requestedPath != "/api/v3/movie/42" {
		t.Errorf("expected targeted path /api/v3/movie/42, got %q", requestedPath)
	}
}

func TestGetMovieByTMDBID_TargetedLookup(t *testing.T) {
	var requestedPath, requestedQuery string
	client, _ := newTestRadarr(t, func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		requestedQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":42,"title":"The Matrix","tmdbId":603,"hasFile":true,"movieFile":{"id":7}}]`))
	})

	m := NewManager()
	movie, err := m.GetMovieByTMDBID(context.Background(), client, "radarr1", 603)
	if err != nil {
		t.Fatalf("GetMovieByTMDBID returned error: %v", err)
	}
	if movie == nil || movie.TmdbID != 603 {
		t.Fatalf("expected movie with TMDB ID 603, got %+v", movie)
	}
	if requestedPath != "/api/v3/movie" {
		t.Errorf("expected path /api/v3/movie, got %q", requestedPath)
	}
	if requestedQuery != "tmdbId=603" {
		t.Errorf("expected query tmdbId=603, got %q", requestedQuery)
	}
}

func TestGetMovieByTMDBID_NotFound(t *testing.T) {
	client, _ := newTestRadarr(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})

	m := NewManager()
	movie, err := m.GetMovieByTMDBID(context.Background(), client, "radarr1", 999)
	if err != nil {
		t.Fatalf("GetMovieByTMDBID returned error: %v", err)
	}
	if movie != nil {
		t.Fatalf("expected nil movie for unknown TMDB ID, got %+v", movie)
	}
}

func TestGetMovieByID_ServesFromWarmCacheWithoutNetwork(t *testing.T) {
	var hits int
	client, _ := newTestRadarr(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusInternalServerError)
	})

	m := NewManager()
	// Warm the full-list cache directly so the targeted lookup can serve from it.
	m.cacheMu.Lock()
	m.movieCache["radarr1"] = []*radarr.Movie{
		{ID: 42, Title: "The Matrix", TmdbID: 603},
	}
	m.cacheExpiry["radarr1"] = time.Now().Add(cacheTTL)
	m.cacheMu.Unlock()

	movie, err := m.GetMovieByID(context.Background(), client, "radarr1", 42)
	if err != nil {
		t.Fatalf("GetMovieByID returned error: %v", err)
	}
	if movie == nil || movie.ID != 42 {
		t.Fatalf("expected cached movie ID 42, got %+v", movie)
	}
	if hits != 0 {
		t.Errorf("expected no network calls when cache is warm, got %d", hits)
	}
}

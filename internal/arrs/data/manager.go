package data

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
	"golift.io/starr/radarr"
	"golift.io/starr/sonarr"
)

const cacheTTL = 10 * time.Minute

type Manager struct {
	cacheMu           sync.RWMutex
	movieCache        map[string][]*radarr.Movie       // key: instance name
	seriesCache       map[string][]*sonarr.Series      // key: instance name
	episodeFilesCache map[string][]*sonarr.EpisodeFile // key: instance name + series id
	cacheExpiry       map[string]time.Time             // key: cache key
	requestGroup      singleflight.Group
}

func NewManager() *Manager {
	return &Manager{
		movieCache:        make(map[string][]*radarr.Movie),
		seriesCache:       make(map[string][]*sonarr.Series),
		episodeFilesCache: make(map[string][]*sonarr.EpisodeFile),
		cacheExpiry:       make(map[string]time.Time),
	}
}

// GetMovies retrieves all movies from Radarr, using a cache if available and valid
func (m *Manager) GetMovies(ctx context.Context, client *radarr.Radarr, instanceName string) ([]*radarr.Movie, error) {
	// 1. Check cache (read lock)
	m.cacheMu.RLock()
	movies, ok := m.movieCache[instanceName]
	expiry, valid := m.cacheExpiry[instanceName]
	m.cacheMu.RUnlock()

	if ok && valid && time.Now().Before(expiry) {
		slog.DebugContext(ctx, "Using cached movie list", "instance", instanceName, "count", len(movies))
		return movies, nil
	}

	// 2. Use singleflight to deduplicate requests
	key := "radarr_movies_" + instanceName
	v, err, _ := m.requestGroup.Do(key, func() (any, error) {
		// Double check cache
		m.cacheMu.RLock()
		movies, ok := m.movieCache[instanceName]
		expiry, valid := m.cacheExpiry[instanceName]
		m.cacheMu.RUnlock()
		if ok && valid && time.Now().Before(expiry) {
			return movies, nil
		}

		slog.DebugContext(ctx, "Fetching fresh movie list", "instance", instanceName)
		freshMovies, err := client.GetMovieContext(ctx, &radarr.GetMovie{})
		if err != nil {
			return nil, err
		}

		m.cacheMu.Lock()
		m.movieCache[instanceName] = freshMovies
		m.cacheExpiry[instanceName] = time.Now().Add(cacheTTL)
		m.cacheMu.Unlock()

		return freshMovies, nil
	})

	if err != nil {
		return nil, err
	}

	return v.([]*radarr.Movie), nil
}

// findCachedMovie returns the first cached movie matching the predicate for an
// instance, but only when the cached movie list is present and still valid. It
// returns nil when the cache is cold/expired or no movie matches.
func (m *Manager) findCachedMovie(instanceName string, match func(*radarr.Movie) bool) *radarr.Movie {
	m.cacheMu.RLock()
	defer m.cacheMu.RUnlock()

	movies, ok := m.movieCache[instanceName]
	expiry, valid := m.cacheExpiry[instanceName]
	if !ok || !valid || !time.Now().Before(expiry) {
		return nil
	}

	for _, movie := range movies {
		if match(movie) {
			return movie
		}
	}
	return nil
}

// GetMovieByID returns a single movie by its Radarr DB ID. It serves from the
// cached full movie list when it is already warm, otherwise it issues a targeted
// lookup (GET /api/v3/movie/{id}) instead of fetching the entire library, which
// can exceed the HTTP client timeout on large instances.
func (m *Manager) GetMovieByID(ctx context.Context, client *radarr.Radarr, instanceName string, movieID int64) (*radarr.Movie, error) {
	if movie := m.findCachedMovie(instanceName, func(mv *radarr.Movie) bool { return mv.ID == movieID }); movie != nil {
		slog.DebugContext(ctx, "Using cached movie for ID lookup", "instance", instanceName, "movie_id", movieID)
		return movie, nil
	}

	slog.DebugContext(ctx, "Fetching single movie by ID", "instance", instanceName, "movie_id", movieID)
	return client.GetMovieByIDContext(ctx, movieID)
}

// GetMovieByTMDBID returns a single movie by its TMDB ID. It serves from the
// cached full movie list when it is already warm, otherwise it issues a targeted
// lookup (GET /api/v3/movie?tmdbId=...) instead of fetching the entire library.
// Returns (nil, nil) when no movie with the given TMDB ID exists.
func (m *Manager) GetMovieByTMDBID(ctx context.Context, client *radarr.Radarr, instanceName string, tmdbID int64) (*radarr.Movie, error) {
	if movie := m.findCachedMovie(instanceName, func(mv *radarr.Movie) bool { return mv.TmdbID == tmdbID }); movie != nil {
		slog.DebugContext(ctx, "Using cached movie for TMDB ID lookup", "instance", instanceName, "tmdb_id", tmdbID)
		return movie, nil
	}

	slog.DebugContext(ctx, "Fetching single movie by TMDB ID", "instance", instanceName, "tmdb_id", tmdbID)
	movies, err := client.GetMovieContext(ctx, &radarr.GetMovie{TMDBID: tmdbID})
	if err != nil {
		return nil, err
	}
	if len(movies) == 0 {
		return nil, nil
	}
	return movies[0], nil
}

// GetSeries retrieves all series from Sonarr, using a cache if available and valid
func (m *Manager) GetSeries(ctx context.Context, client *sonarr.Sonarr, instanceName string) ([]*sonarr.Series, error) {
	// 1. Check cache (read lock)
	m.cacheMu.RLock()
	series, ok := m.seriesCache[instanceName]
	expiry, valid := m.cacheExpiry[instanceName]
	m.cacheMu.RUnlock()

	if ok && valid && time.Now().Before(expiry) {
		slog.DebugContext(ctx, "Using cached series list", "instance", instanceName, "count", len(series))
		return series, nil
	}

	// 2. Use singleflight to deduplicate requests
	key := "sonarr_series_" + instanceName
	v, err, _ := m.requestGroup.Do(key, func() (any, error) {
		// Double check cache
		m.cacheMu.RLock()
		series, ok := m.seriesCache[instanceName]
		expiry, valid := m.cacheExpiry[instanceName]
		m.cacheMu.RUnlock()
		if ok && valid && time.Now().Before(expiry) {
			return series, nil
		}

		slog.DebugContext(ctx, "Fetching fresh series list", "instance", instanceName)
		freshSeries, err := client.GetAllSeriesContext(ctx)
		if err != nil {
			return nil, err
		}

		m.cacheMu.Lock()
		m.seriesCache[instanceName] = freshSeries
		m.cacheExpiry[instanceName] = time.Now().Add(cacheTTL)
		m.cacheMu.Unlock()

		return freshSeries, nil
	})

	if err != nil {
		return nil, err
	}

	return v.([]*sonarr.Series), nil
}

// GetEpisodeFiles retrieves all episode files for a series from Sonarr, using a cache if available and valid
func (m *Manager) GetEpisodeFiles(ctx context.Context, client *sonarr.Sonarr, instanceName string, seriesID int64) ([]*sonarr.EpisodeFile, error) {
	cacheKey := fmt.Sprintf("sonarr_episode_files_%s_%d", instanceName, seriesID)

	// 1. Check cache (read lock)
	m.cacheMu.RLock()
	files, ok := m.episodeFilesCache[cacheKey]
	expiry, valid := m.cacheExpiry[cacheKey]
	m.cacheMu.RUnlock()

	if ok && valid && time.Now().Before(expiry) {
		slog.DebugContext(ctx, "Using cached episode files list", "instance", instanceName, "series_id", seriesID, "count", len(files))
		return files, nil
	}

	// 2. Use singleflight to deduplicate requests
	v, err, _ := m.requestGroup.Do(cacheKey, func() (any, error) {
		// Double check cache
		m.cacheMu.RLock()
		files, ok := m.episodeFilesCache[cacheKey]
		expiry, valid := m.cacheExpiry[cacheKey]
		m.cacheMu.RUnlock()
		if ok && valid && time.Now().Before(expiry) {
			return files, nil
		}

		slog.DebugContext(ctx, "Fetching fresh episode files list", "instance", instanceName, "series_id", seriesID)
		freshFiles, err := client.GetSeriesEpisodeFilesContext(ctx, seriesID)
		if err != nil {
			return nil, err
		}

		m.cacheMu.Lock()
		m.episodeFilesCache[cacheKey] = freshFiles
		m.cacheExpiry[cacheKey] = time.Now().Add(cacheTTL)
		m.cacheMu.Unlock()

		return freshFiles, nil
	})

	if err != nil {
		return nil, err
	}

	return v.([]*sonarr.EpisodeFile), nil
}

// ClearMoviesCache clears the movies cache for a specific instance
func (m *Manager) ClearMoviesCache(instanceName string) {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	delete(m.movieCache, instanceName)
	delete(m.cacheExpiry, instanceName)
}

// ClearSeriesCache clears the series and episode files cache for a specific instance
func (m *Manager) ClearSeriesCache(instanceName string) {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	delete(m.seriesCache, instanceName)
	delete(m.cacheExpiry, instanceName)

	// Clear all episode file caches for this instance
	prefix := "sonarr_episode_files_" + instanceName + "_"
	for key := range m.episodeFilesCache {
		if strings.HasPrefix(key, prefix) {
			delete(m.episodeFilesCache, key)
			delete(m.cacheExpiry, key)
		}
	}
}

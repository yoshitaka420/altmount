package scanner

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/javi11/altmount/internal/arrs/model"
	"golift.io/starr/radarr"
	"golift.io/starr/sonarr"
)

// radarrOwnership is the read-only result of resolving which Radarr movie (if
// any) currently owns a given file. It carries everything the repair path needs
// to decide on destructive actions, but resolving it performs no writes.
type radarrOwnership struct {
	// movie is the resolved Radarr movie, or nil when no movie matched.
	movie *radarr.Movie
	// movieFileID is the Radarr movie-file record id linked to the resolved
	// movie (0 when the movie has no current file).
	movieFileID int64
	// alreadySatisfied is true when the movie already has a *different* healthy
	// file than the one we were asked about (Smart Repair Guard): the title was
	// upgraded/replaced, so the old copy is redundant.
	alreadySatisfied bool
	// lookupErr is true when a lookup call failed and prevented a definitive
	// determination. The repair path ignores it (it falls back to other
	// strategies), but fail-closed callers use it to avoid treating an
	// errored lookup as "unowned".
	//
	// It is an audit flag for "a targeted lookup failed", not "overall lookup
	// failed": it can stay true even after a later targeted/fallback lookup
	// successfully resolves the movie. That is intentional — when own.movie is
	// non-nil, callers use the movie and lookupErr is irrelevant; it only
	// matters when no movie was resolved.
	lookupErr bool
}

// resolveRadarrOwnership resolves which movie owns filePath using the same
// cascade the repair path relies on (targeted id lookup → targeted tmdb lookup
// → full-list path matching → stable tmdbId fallback), but WITHOUT any
// destructive side effects (no blocklist, delete, or search). A non-nil error
// is returned only for a hard enumeration failure (the full movie list could
// not be fetched), matching the repair path's historical behavior.
func (m *Manager) resolveRadarrOwnership(ctx context.Context, client *radarr.Radarr, filePath, relativePath, instanceName string, metadata *model.WebhookMetadata) (radarrOwnership, error) {
	var own radarrOwnership

	// ID-Based Precision: resolve the exact movie with a targeted lookup instead
	// of fetching the entire library (which can exceed the HTTP client timeout on
	// large instances). Prefer the Radarr DB movie ID, then the TMDB ID.
	if metadata != nil && metadata.Movie != nil {
		// Targeted lookups, cheapest first: the Radarr DB id, then the stable TMDB
		// id. Both are attempted before any full-library enumeration below, so an
		// id-lookup failure still tries the targeted TMDB lookup rather than
		// jumping straight to the expensive full-list scan.
		if metadata.Movie.Id > 0 {
			slog.InfoContext(ctx, "ID-Based Precision: Looking up Radarr movie by ID", "movie_id", metadata.Movie.Id)
			movie, err := m.data.GetMovieByID(ctx, client, instanceName, metadata.Movie.Id)
			if err != nil {
				slog.WarnContext(ctx, "Targeted Radarr movie lookup by ID failed, trying TMDB id then path-based guessing",
					"movie_id", metadata.Movie.Id, "error", err)
				own.lookupErr = true
			} else {
				own.movie = movie
			}
		}
		if own.movie == nil && metadata.Movie.TmdbId > 0 {
			slog.InfoContext(ctx, "ID-Based Precision: Looking up Radarr movie by TMDB ID", "tmdb_id", metadata.Movie.TmdbId)
			movie, err := m.data.GetMovieByTMDBID(ctx, client, instanceName, metadata.Movie.TmdbId)
			if err != nil {
				slog.WarnContext(ctx, "Targeted Radarr movie lookup by TMDB ID failed, falling back to path-based guessing",
					"tmdb_id", metadata.Movie.TmdbId, "error", err)
				own.lookupErr = true
			} else {
				own.movie = movie
			}
		}

		if own.movie != nil && own.movie.HasFile && own.movie.MovieFile != nil {
			// Smart Repair Guard: if the movie already has a different healthy file
			// than the one we were asked to repair, it was likely upgraded.
			if metadata.MovieFile != nil && metadata.MovieFile.Id > 0 && own.movie.MovieFile.ID != metadata.MovieFile.Id {
				slog.InfoContext(ctx, "Smart Repair Guard: Movie already has a different healthy file (likely upgraded).",
					"movie", own.movie.Title,
					"old_file_id", metadata.MovieFile.Id,
					"new_file_id", own.movie.MovieFile.ID)
				own.alreadySatisfied = true
				return own, nil
			}
			own.movieFileID = own.movie.MovieFile.ID
		}
	}

	// Fallback to path-based guessing if ID-based precision failed or metadata was missing.
	if own.movie == nil {
		slog.DebugContext(ctx, "Falling back to path-based guessing for Radarr", "file_path", filePath)
		movies, err := m.data.GetMovies(ctx, client, instanceName)
		if err != nil {
			return own, fmt.Errorf("failed to get movies from Radarr: %w", err)
		}

		for _, movie := range movies {
			requestFileName := filepath.Base(filePath)

			if movie.HasFile && movie.MovieFile != nil {
				if movie.MovieFile.Path == filePath {
					own.movie = movie
					own.movieFileID = movie.MovieFile.ID
					break
				}

				movieFileName := filepath.Base(movie.MovieFile.Path)
				if movieFileName == requestFileName {
					slog.InfoContext(ctx, "Found Radarr movie match by filename",
						"movie", movie.Title,
						"path", movie.MovieFile.Path)
					own.movie = movie
					own.movieFileID = movie.MovieFile.ID
					break
				}

				if before, ok := strings.CutSuffix(filePath, ".strm"); ok {
					strippedPath := before
					if strings.TrimSuffix(movie.MovieFile.Path, filepath.Ext(movie.MovieFile.Path)) == strippedPath {
						own.movie = movie
						own.movieFileID = movie.MovieFile.ID
						break
					}
				}
				if relativePath != "" {
					strippedRelative := strings.TrimSuffix(relativePath, ".strm")
					if strings.HasSuffix(movie.MovieFile.Path, relativePath) ||
						strings.HasSuffix(strings.TrimSuffix(movie.MovieFile.Path, filepath.Ext(movie.MovieFile.Path)), strippedRelative) {
						slog.InfoContext(ctx, "Found Radarr movie match by relative path suffix",
							"radarr_path", movie.MovieFile.Path,
							"relative_path", relativePath)
						own.movie = movie
						own.movieFileID = movie.MovieFile.ID
						break
					}
				}
			}
		}
	}

	// tmdbId fallback: the stored internal Movie.Id goes stale when a movie is
	// removed and re-added in Radarr, and a rename defeats the path-based guessing
	// above. tmdbId is immutable across remove+re-add, so resolve the movie
	// directly by it before giving up.
	if own.movie == nil && metadata != nil && metadata.Movie != nil && metadata.Movie.TmdbId > 0 {
		slog.InfoContext(ctx, "ID and path match failed, resolving Radarr movie by stable tmdbId",
			"instance", instanceName,
			"tmdb_id", metadata.Movie.TmdbId)

		movies, err := client.GetMovieContext(ctx, &radarr.GetMovie{TMDBID: metadata.Movie.TmdbId})
		if err != nil {
			slog.WarnContext(ctx, "Failed to resolve Radarr movie by tmdbId",
				"instance", instanceName,
				"tmdb_id", metadata.Movie.TmdbId,
				"error", err)
			own.lookupErr = true
		} else if len(movies) > 0 {
			own.movie = movies[0]
			if own.movie.HasFile && own.movie.MovieFile != nil {
				own.movieFileID = own.movie.MovieFile.ID
			}
			slog.InfoContext(ctx, "Resolved Radarr movie by tmdbId fallback",
				"instance", instanceName,
				"tmdb_id", metadata.Movie.TmdbId,
				"movie_id", own.movie.ID,
				"movie_title", own.movie.Title)
		}
	}

	return own, nil
}

// sonarrOwnership is the read-only result of resolving which Sonarr series and
// episode-file (if any) currently own a given file. Resolving it performs no
// writes; the repair path layers its destructive actions on top.
type sonarrOwnership struct {
	// seriesID is the Sonarr series id that owns the path (0 when no series matched).
	seriesID int64
	// seriesTitle is the matched series title (for logging).
	seriesTitle string
	// episodeFileID is the Sonarr episode-file record id matching the path
	// (0 when the series matched but no file record lined up).
	episodeFileID int64
	// lookupErr is true when a lookup call failed and prevented a definitive
	// determination, so fail-closed callers don't treat it as "unowned".
	lookupErr bool
}

// resolveSonarrOwnership resolves which series + episode file own filePath using
// the same id-based then path-based cascade the repair path relies on, but
// WITHOUT destructive side effects. A non-nil error is returned only for a hard
// enumeration failure (series list or episode-file list could not be fetched),
// matching the repair path's historical behavior. A zero seriesID with a nil
// error means the path simply did not match any series.
func (m *Manager) resolveSonarrOwnership(ctx context.Context, client *sonarr.Sonarr, filePath, relativePath, instanceName string, metadata *model.WebhookMetadata) (sonarrOwnership, error) {
	var own sonarrOwnership

	// ID-Based Precision: if we have exact IDs from metadata, use them.
	if metadata != nil && metadata.Series != nil && metadata.EpisodeFile != nil && metadata.Series.Id > 0 && metadata.EpisodeFile.Id > 0 {
		slog.InfoContext(ctx, "ID-Based Precision: Using metadata IDs for Sonarr repair",
			"series_id", metadata.Series.Id,
			"episode_file_id", metadata.EpisodeFile.Id)

		own.seriesID = metadata.Series.Id
		own.episodeFileID = metadata.EpisodeFile.Id
		own.seriesTitle = "Known Series (ID Based)" // Title is just for logging
		return own, nil
	}

	// Fallback to path-based guessing if ID-based precision failed or metadata was missing.
	slog.DebugContext(ctx, "Falling back to path-based guessing for Sonarr", "file_path", filePath)

	libraryDir := m.configGetter().MountPath
	cfg := m.configGetter()
	if cfg.Health.LibraryDir != nil && *cfg.Health.LibraryDir != "" {
		libraryDir = *cfg.Health.LibraryDir
	}
	slog.DebugContext(ctx, "Searching Sonarr for matching series by path", "library_dir", libraryDir)

	series, err := m.data.GetSeries(ctx, client, instanceName)
	if err != nil {
		return own, fmt.Errorf("failed to get series from Sonarr: %w", err)
	}

	var targetSeries *sonarr.Series
	for _, show := range series {
		if pathContainsDir(filePath, show.Path) {
			targetSeries = show
			break
		}
	}

	if targetSeries == nil {
		for _, show := range series {
			showFolderName := filepath.Base(show.Path)
			if hasPathComponent(relativePath, showFolderName) {
				slog.InfoContext(ctx, "Found series match by folder name", "series", show.Title, "folder", showFolderName)
				targetSeries = show
				break
			}
		}
	}

	if targetSeries == nil {
		// No series matched: leave seriesID zero so the caller can run its
		// (destructive) queue-based fallback.
		return own, nil
	}

	own.seriesID = targetSeries.ID
	own.seriesTitle = targetSeries.Title

	slog.InfoContext(ctx, "Found matching series, searching for episode file",
		"series_title", own.seriesTitle,
		"series_path", targetSeries.Path)

	episodeFiles, err := m.data.GetEpisodeFiles(ctx, client, instanceName, own.seriesID)
	if err != nil {
		return own, fmt.Errorf("failed to get episode files for series %s: %w", own.seriesTitle, err)
	}

	var targetEpisodeFile *sonarr.EpisodeFile
	for _, episodeFile := range episodeFiles {
		if episodeFile.Path == filePath {
			targetEpisodeFile = episodeFile
			break
		}

		if filepath.Base(episodeFile.Path) == filepath.Base(filePath) {
			slog.InfoContext(ctx, "Found Sonarr episode match by filename", "path", episodeFile.Path)
			targetEpisodeFile = episodeFile
			break
		}

		if before, ok := strings.CutSuffix(filePath, ".strm"); ok {
			strippedPath := before
			if strings.TrimSuffix(episodeFile.Path, filepath.Ext(episodeFile.Path)) == strippedPath {
				targetEpisodeFile = episodeFile
				break
			}
		}

		if relativePath != "" {
			strippedRelative := strings.TrimSuffix(relativePath, ".strm")
			if strings.HasSuffix(episodeFile.Path, relativePath) ||
				strings.HasSuffix(strings.TrimSuffix(episodeFile.Path, filepath.Ext(episodeFile.Path)), strippedRelative) {
				slog.InfoContext(ctx, "Found Sonarr episode match by relative path suffix",
					"sonarr_path", episodeFile.Path,
					"relative_path", relativePath)
				targetEpisodeFile = episodeFile
				break
			}
		}
	}

	if targetEpisodeFile != nil {
		own.episodeFileID = targetEpisodeFile.ID
	}

	return own, nil
}

// pathContainsDir reports whether dir occurs in p terminated at a path-component
// boundary (the next character is a separator or end of string). This keeps the
// original lenient "appears anywhere" behavior across differing path prefixes
// while preventing a sibling directory from matching by raw substring (e.g.
// "/tv/Show" must not match "/tv/Showtime/...").
func pathContainsDir(p, dir string) bool {
	p = filepath.ToSlash(p)
	// Trim any trailing separators so the component-boundary check below lines up
	// with the end of the matched directory (e.g. dir "/tv/Show/" must still
	// match "/tv/Show/ep.mkv").
	dir = strings.TrimRight(filepath.ToSlash(dir), "/")
	if dir == "" {
		return false
	}
	for i := 0; ; {
		idx := strings.Index(p[i:], dir)
		if idx < 0 {
			return false
		}
		end := i + idx + len(dir)
		if end == len(p) || p[end] == '/' {
			return true
		}
		i = i + idx + 1
	}
}

// hasPathComponent reports whether comp appears as a whole, slash-delimited path
// component of p, so a folder name like "Show" matches "Show/..." but not
// "Showtime/..." or "My Show Extra/...".
func hasPathComponent(p, comp string) bool {
	if comp == "" {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(p), "/") {
		if part == comp {
			return true
		}
	}
	return false
}

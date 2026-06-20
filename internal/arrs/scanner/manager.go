package scanner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/javi11/altmount/internal/arrs/clients"
	"github.com/javi11/altmount/internal/arrs/data"
	"github.com/javi11/altmount/internal/arrs/failures"
	"github.com/javi11/altmount/internal/arrs/instances"
	"github.com/javi11/altmount/internal/arrs/model"
	"github.com/javi11/altmount/internal/config"
	"golang.org/x/sync/singleflight"
	"golift.io/starr"
	"golift.io/starr/lidarr"
	"golift.io/starr/radarr"
	"golift.io/starr/readarr"
	"golift.io/starr/sonarr"
)

type Manager struct {
	configGetter config.ConfigGetter
	instances    *instances.Manager
	clients      *clients.Manager
	data         *data.Manager
	// failures is the shared per-target circuit breaker (see arrs/failures). The
	// scanner bumps it on every targeted re-search it issues; at the configured
	// queue_cleanup_max_failures threshold it stops searching and unmonitors the
	// target instead, so a dead release can't drive an endless re-grab storm.
	failures *failures.Tracker
	sf       singleflight.Group
}

func NewManager(configGetter config.ConfigGetter, instances *instances.Manager, clients *clients.Manager, data *data.Manager, failureTracker *failures.Tracker) *Manager {
	return &Manager{
		configGetter: configGetter,
		instances:    instances,
		clients:      clients,
		data:         data,
		failures:     failureTracker,
	}
}

// findInstanceForFilePath finds which ARR instance manages the given file path
func (m *Manager) findInstanceForFilePath(ctx context.Context, filePath string, relativePath string) (instanceType string, instanceName string, err error) {
	slog.DebugContext(ctx, "Finding instance for file path", "file_path", filePath, "relative_path", relativePath)

	allInstances := m.instances.GetAllInstances()

	// Strategy 1: Fast Path - Check Root Folders
	for _, instance := range allInstances {
		if !instance.Enabled {
			continue
		}

		if client, err := m.clients.GetOrCreateClient(instance); err == nil {
			if m.managesFile(ctx, instance.Type, client, filePath) {
				return instance.Type, instance.Name, nil
			}
		}
	}

	// Strategy 2: Category Match - Check if file is in the staging/complete folder
	cfg := m.configGetter()
	if cfg.SABnzbd.CompleteDir != "" {
		completeDir := strings.Trim(filepath.ToSlash(cfg.SABnzbd.CompleteDir), "/")
		completeSegment := "/" + completeDir + "/"
		normalizedPath := filepath.ToSlash(filePath)

		// Check if path contains the complete directory as a segment
		if _, after, found := strings.Cut(normalizedPath, completeSegment); found {
			// Extract everything after the complete directory segment (e.g., "tv/show/file.mkv")
			afterPrefix := after
			parts := strings.Split(afterPrefix, "/")
			if len(parts) > 0 {
				category := parts[0]
				slog.DebugContext(ctx, "File is in complete_dir, matching by category", "category", category)

				for _, instance := range allInstances {
					if !instance.Enabled {
						continue
					}

					if strings.EqualFold(instance.Category, category) {
						slog.InfoContext(ctx, "Found managing instance by category in complete_dir", "instance", instance.Name, "category", category)
						return instance.Type, instance.Name, nil
					}
				}
			}
		}
	}

	// Strategy 3: Slow Path - Search Cache by Relative Path
	if relativePath != "" {
		slog.InfoContext(ctx, "Root folder match failed, attempting relative path search", "relative_path", relativePath)

		for _, instance := range allInstances {
			if !instance.Enabled {
				continue
			}

			if client, err := m.clients.GetOrCreateClient(instance); err == nil {
				if m.hasFile(ctx, instance.Type, client, instance.Name, relativePath) {
					slog.InfoContext(ctx, "Found managing instance by relative path", "instance", instance.Name, "type", instance.Type)
					return instance.Type, instance.Name, nil
				}
			}
		}
	}

	return "", "", fmt.Errorf("no ARR instance found managing file path: %s", filePath)
}

func (m *Manager) managesFile(ctx context.Context, instanceType string, client any, filePath string) bool {
	switch instanceType {
	case "radarr":
		rc, ok := client.(*radarr.Radarr)
		if !ok {
			return false
		}
		return m.radarrManagesFile(ctx, rc, filePath)
	case "sonarr":
		sc, ok := client.(*sonarr.Sonarr)
		if !ok {
			return false
		}
		return m.sonarrManagesFile(ctx, sc, filePath)
	case "lidarr":
		lc, ok := client.(*lidarr.Lidarr)
		if !ok {
			return false
		}
		return m.lidarrManagesFile(ctx, lc, filePath)
	case "readarr":
		rc, ok := client.(*readarr.Readarr)
		if !ok {
			return false
		}
		return m.readarrManagesFile(ctx, rc, filePath)
	case "whisparr":
		wc, ok := client.(*sonarr.Sonarr)
		if !ok {
			return false
		}
		return m.sonarrManagesFile(ctx, wc, filePath)
	default:
		return false
	}
}

func (m *Manager) hasFile(ctx context.Context, instanceType string, client any, instanceName, relativePath string) bool {
	switch instanceType {
	case "radarr":
		rc, ok := client.(*radarr.Radarr)
		if !ok {
			return false
		}
		return m.radarrHasFile(ctx, rc, instanceName, relativePath)
	case "sonarr":
		sc, ok := client.(*sonarr.Sonarr)
		if !ok {
			return false
		}
		return m.sonarrHasFile(ctx, sc, instanceName, relativePath)
	case "lidarr", "readarr", "whisparr":
		// For now, these don't have a slow path search implementation
		// They rely on the Root Folder (Strategy 1) or Category (Strategy 2)
		return false
	default:
		return false
	}
}

// radarrManagesFile checks if Radarr manages the given file path using root folders (checkrr approach)
func (m *Manager) radarrManagesFile(ctx context.Context, client *radarr.Radarr, filePath string) bool {
	slog.DebugContext(ctx, "Checking Radarr root folders for file ownership",
		"file_path", filePath)

	// Get root folders from Radarr (much faster than GetMovie)
	rootFolders, err := client.GetRootFoldersContext(ctx)
	if err != nil {
		slog.DebugContext(ctx, "Failed to get root folders from Radarr for file check", "error", err)
		return false
	}

	// Check if file path starts with any root folder path
	for _, folder := range rootFolders {
		slog.DebugContext(ctx, "Checking Radarr root folder", "folder_path", folder.Path, "file_path", filePath)
		// Check for direct prefix match or if the filePath contains the folder.Path (common in Docker/Remote setups)
		if strings.HasPrefix(filePath, folder.Path) {
			slog.DebugContext(ctx, "File matches Radarr root folder", "folder_path", folder.Path)
			return true
		}
	}

	slog.DebugContext(ctx, "File does not match any Radarr root folders")
	return false
}

// sonarrManagesFile checks if Sonarr manages the given file path using root folders (checkrr approach)
func (m *Manager) sonarrManagesFile(ctx context.Context, client *sonarr.Sonarr, filePath string) bool {
	slog.DebugContext(ctx, "Checking Sonarr root folders for file ownership",
		"file_path", filePath)

	// Get root folders from Sonarr (much faster than GetAllSeries)
	rootFolders, err := client.GetRootFoldersContext(ctx)
	if err != nil {
		slog.DebugContext(ctx, "Failed to get root folders from Sonarr for file check", "error", err)
		return false
	}

	// Check if file path starts with any root folder path
	for _, folder := range rootFolders {
		slog.DebugContext(ctx, "Checking Sonarr root folder", "folder_path", folder.Path, "file_path", filePath)
		if strings.HasPrefix(filePath, folder.Path) {
			slog.DebugContext(ctx, "File matches Sonarr root folder", "folder_path", folder.Path)
			return true
		}
	}

	slog.DebugContext(ctx, "File does not match any Sonarr root folders")
	return false
}

// lidarrManagesFile checks if Lidarr manages the given file path using root folders
func (m *Manager) lidarrManagesFile(ctx context.Context, client *lidarr.Lidarr, filePath string) bool {
	slog.DebugContext(ctx, "Checking Lidarr root folders for file ownership", "file_path", filePath)
	rootFolders, err := client.GetRootFoldersContext(ctx)
	if err != nil {
		slog.DebugContext(ctx, "Failed to get root folders from Lidarr", "error", err)
		return false
	}
	for _, folder := range rootFolders {
		if strings.HasPrefix(filePath, folder.Path) {
			return true
		}
	}
	return false
}

// readarrManagesFile checks if Readarr manages the given file path using root folders
func (m *Manager) readarrManagesFile(ctx context.Context, client *readarr.Readarr, filePath string) bool {
	slog.DebugContext(ctx, "Checking Readarr root folders for file ownership", "file_path", filePath)
	rootFolders, err := client.GetRootFoldersContext(ctx)
	if err != nil {
		slog.DebugContext(ctx, "Failed to get root folders from Readarr", "error", err)
		return false
	}
	for _, folder := range rootFolders {
		if strings.HasPrefix(filePath, folder.Path) {
			return true
		}
	}
	return false
}

// radarrHasFile checks if any movie in the instance contains the given relative path
func (m *Manager) radarrHasFile(ctx context.Context, client *radarr.Radarr, instanceName, relativePath string) bool {
	movies, err := m.data.GetMovies(ctx, client, instanceName)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to get movies for relative path check", "instance", instanceName, "error", err)
		return false
	}

	strippedRelative := strings.TrimSuffix(relativePath, ".strm")

	for _, movie := range movies {
		if movie.HasFile && movie.MovieFile != nil {
			if strings.HasSuffix(movie.MovieFile.Path, relativePath) ||
				strings.HasSuffix(strings.TrimSuffix(movie.MovieFile.Path, filepath.Ext(movie.MovieFile.Path)), strippedRelative) {
				return true
			}
		}
	}
	return false
}

// sonarrHasFile checks if any series in the instance contains the given relative path
func (m *Manager) sonarrHasFile(ctx context.Context, client *sonarr.Sonarr, instanceName, relativePath string) bool {
	seriesList, err := m.data.GetSeries(ctx, client, instanceName)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to get series for relative path check", "instance", instanceName, "error", err)
		return false
	}

	// Normalize relative path for comparison
	relativePath = filepath.ToSlash(relativePath)
	strippedRelative := strings.TrimSuffix(relativePath, ".strm")

	for _, series := range seriesList {
		// Check if the series folder name is part of the relative path
		folderName := filepath.Base(series.Path)
		if strings.Contains(relativePath, folderName) || strings.Contains(strippedRelative, folderName) {
			return true
		}
	}
	return false
}

// TriggerFileRescan triggers a rescan for a specific file path through the appropriate ARR instance
func (m *Manager) TriggerFileRescan(ctx context.Context, pathForRescan string, relativePath string, metadataStr *string) error {
	res, err, _ := m.sf.Do(fmt.Sprintf("rescan:%s", pathForRescan), func() (interface{}, error) {
		slog.InfoContext(ctx, "Triggering ARR rescan", "path", pathForRescan, "relative_path", relativePath)

		var metadata *model.WebhookMetadata
		if metadataStr != nil && *metadataStr != "" {
			var parsedMetadata model.WebhookMetadata
			if err := json.Unmarshal([]byte(*metadataStr), &parsedMetadata); err == nil {
				metadata = &parsedMetadata
			} else {
				slog.WarnContext(ctx, "Failed to parse metadata string, falling back to path-based repair", "error", err, "path", pathForRescan)
			}
		}

		var instanceType, instanceName string
		var err error

		// Try fast path: use instance from metadata
		if metadata != nil && metadata.InstanceName != "" {
			// Find if the instance exists in config
			instances := m.instances.GetAllInstances()
			for _, inst := range instances {
				if inst.Name == metadata.InstanceName {
					instanceType = inst.Type
					instanceName = inst.Name
					slog.InfoContext(ctx, "Fast path: Found instance from metadata", "instance", instanceName, "type", instanceType)
					break
				}
			}
		}

		// Fallback to path-based logic if instance not found from metadata
		if instanceName == "" {
			slog.DebugContext(ctx, "Instance not found from metadata, falling back to path-based detection", "path", pathForRescan)
			instanceType, instanceName, err = m.findInstanceForFilePath(ctx, pathForRescan, relativePath)
			if err != nil {
				return nil, fmt.Errorf("failed to find ARR instance for file path %s: %w", pathForRescan, err)
			}
		}

		// Find the instance configuration
		instanceConfig, err := m.instances.FindConfigInstance(instanceType, instanceName)
		if err != nil {
			return nil, fmt.Errorf("failed to find instance config: %w", err)
		}

		// Check if instance is enabled
		if !instanceConfig.Enabled {
			return nil, fmt.Errorf("instance %s/%s is disabled", instanceType, instanceName)
		}

		// Trigger rescan based on instance type
		switch instanceType {
		case "radarr":
			client, err := m.clients.GetOrCreateRadarrClient(instanceName, instanceConfig.URL, instanceConfig.APIKey)
			if err != nil {
				return nil, fmt.Errorf("failed to create Radarr client: %w", err)
			}
			return nil, m.triggerRadarrRescanByPath(ctx, client, pathForRescan, relativePath, instanceName, metadata)

		case "sonarr":
			client, err := m.clients.GetOrCreateSonarrClient(instanceName, instanceConfig.URL, instanceConfig.APIKey)
			if err != nil {
				return nil, fmt.Errorf("failed to create Sonarr client: %w", err)
			}
			return nil, m.triggerSonarrRescanByPath(ctx, client, pathForRescan, relativePath, instanceName, metadata)

		case "lidarr":
			client, err := m.clients.GetOrCreateLidarrClient(instanceName, instanceConfig.URL, instanceConfig.APIKey)
			if err != nil {
				return nil, fmt.Errorf("failed to create Lidarr client: %w", err)
			}
			return nil, m.triggerLidarrRescanByPath(ctx, client, pathForRescan, relativePath, instanceName, metadata)

		case "readarr":
			client, err := m.clients.GetOrCreateReadarrClient(instanceName, instanceConfig.URL, instanceConfig.APIKey)
			if err != nil {
				return nil, fmt.Errorf("failed to create Readarr client: %w", err)
			}
			return nil, m.triggerReadarrRescanByPath(ctx, client, pathForRescan, relativePath, instanceName, metadata)

		case "whisparr":
			client, err := m.clients.GetOrCreateWhisparrClient(instanceName, instanceConfig.URL, instanceConfig.APIKey)
			if err != nil {
				return nil, fmt.Errorf("failed to create Whisparr client: %w", err)
			}
			return nil, m.triggerSonarrRescanByPath(ctx, client, pathForRescan, relativePath, instanceName, metadata)

		default:
			return nil, fmt.Errorf("unsupported instance type: %s", instanceType)
		}
	})

	if err != nil {
		return err
	}
	if res != nil {
		return res.(error)
	}
	return nil
}

// TriggerScanForFile finds the ARR instance managing the file and triggers a download scan on it.
func (m *Manager) TriggerScanForFile(ctx context.Context, filePath string) error {
	// Try to find which instance manages this file path
	instanceType, instanceName, err := m.findInstanceForFilePath(ctx, filePath, "")
	if err != nil {
		return err
	}

	instance, err := m.instances.FindConfigInstance(instanceType, instanceName)
	if err != nil {
		return err
	}

	if !instance.Enabled {
		return fmt.Errorf("instance %s is disabled", instanceName)
	}

	slog.InfoContext(ctx, "Triggering download scan for specific instance", "instance", instanceName, "type", instanceType)

	// Launch scan in background to not block caller
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		switch instance.Type {
		case "radarr":
			client, err := m.clients.GetOrCreateRadarrClient(instance.Name, instance.URL, instance.APIKey)
			if err != nil {
				slog.ErrorContext(bgCtx, "Failed to create Radarr client for scan trigger", "instance", instance.Name, "error", err)
				return
			}
			// Trigger RefreshMonitoredDownloads
			_, err = client.SendCommandContext(bgCtx, &radarr.CommandRequest{Name: "RefreshMonitoredDownloads"})
			if err != nil {
				slog.ErrorContext(bgCtx, "Failed to trigger RefreshMonitoredDownloads", "instance", instance.Name, "error", err)
			} else {
				slog.InfoContext(bgCtx, "Triggered RefreshMonitoredDownloads", "instance", instance.Name)
			}

		case "sonarr":
			client, err := m.clients.GetOrCreateSonarrClient(instance.Name, instance.URL, instance.APIKey)
			if err != nil {
				slog.ErrorContext(bgCtx, "Failed to create Sonarr client for scan trigger", "instance", instance.Name, "error", err)
				return
			}
			// Trigger RefreshMonitoredDownloads
			_, err = client.SendCommandContext(bgCtx, &sonarr.CommandRequest{Name: "RefreshMonitoredDownloads"})
			if err != nil {
				slog.ErrorContext(bgCtx, "Failed to trigger RefreshMonitoredDownloads", "instance", instance.Name, "error", err)
			} else {
				slog.InfoContext(bgCtx, "Triggered RefreshMonitoredDownloads", "instance", instance.Name)
			}
		case "lidarr":
			client, err := m.clients.GetOrCreateLidarrClient(instance.Name, instance.URL, instance.APIKey)
			if err == nil {
				_, _ = client.SendCommandContext(bgCtx, &lidarr.CommandRequest{Name: "RefreshMonitoredDownloads"})
			}
		case "readarr":
			client, err := m.clients.GetOrCreateReadarrClient(instance.Name, instance.URL, instance.APIKey)
			if err == nil {
				_, _ = client.SendCommandContext(bgCtx, &readarr.CommandRequest{Name: "RefreshMonitoredDownloads"})
			}
		case "whisparr":
			client, err := m.clients.GetOrCreateWhisparrClient(instance.Name, instance.URL, instance.APIKey)
			if err == nil {
				_, _ = client.SendCommandContext(bgCtx, &sonarr.CommandRequest{Name: "RefreshMonitoredDownloads"})
			}
		}
	}()

	return nil
}

// TriggerDownloadScan triggers the "Check For Finished Downloads" task in ARR instances
func (m *Manager) TriggerDownloadScan(ctx context.Context, instanceType string) {
	instances := m.instances.GetAllInstances()
	for _, instance := range instances {
		if !instance.Enabled || instance.Type != instanceType {
			continue
		}

		slog.DebugContext(ctx, "Triggering download client scan", "instance", instance.Name, "type", instance.Type)

		go func(inst *model.ConfigInstance) {
			_, _, _ = m.sf.Do(fmt.Sprintf("scan:%s", inst.Name), func() (interface{}, error) {
				bgCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()

				switch inst.Type {
				case "radarr":
					client, err := m.clients.GetOrCreateRadarrClient(inst.Name, inst.URL, inst.APIKey)
					if err != nil {
						slog.ErrorContext(bgCtx, "Failed to create Radarr client for scan trigger", "instance", inst.Name, "error", err)
						return nil, err
					}
					// Trigger RefreshMonitoredDownloads
					_, err = client.SendCommandContext(bgCtx, &radarr.CommandRequest{Name: "RefreshMonitoredDownloads"})
					if err != nil {
						slog.ErrorContext(bgCtx, "Failed to trigger RefreshMonitoredDownloads", "instance", inst.Name, "error", err)
					} else {
						slog.InfoContext(bgCtx, "Triggered RefreshMonitoredDownloads", "instance", inst.Name)
					}

				case "sonarr":
					client, err := m.clients.GetOrCreateSonarrClient(inst.Name, inst.URL, inst.APIKey)
					if err != nil {
						slog.ErrorContext(bgCtx, "Failed to create Sonarr client for scan trigger", "instance", inst.Name, "error", err)
						return nil, err
					}
					// Trigger RefreshMonitoredDownloads
					_, err = client.SendCommandContext(bgCtx, &sonarr.CommandRequest{Name: "RefreshMonitoredDownloads"})
					if err != nil {
						slog.ErrorContext(bgCtx, "Failed to trigger RefreshMonitoredDownloads", "instance", inst.Name, "error", err)
					} else {
						slog.InfoContext(bgCtx, "Triggered RefreshMonitoredDownloads", "instance", inst.Name)
					}
				case "lidarr":
					client, err := m.clients.GetOrCreateLidarrClient(inst.Name, inst.URL, inst.APIKey)
					if err == nil {
						_, _ = client.SendCommandContext(bgCtx, &lidarr.CommandRequest{Name: "RefreshMonitoredDownloads"})
					}
				case "readarr":
					client, err := m.clients.GetOrCreateReadarrClient(inst.Name, inst.URL, inst.APIKey)
					if err == nil {
						_, _ = client.SendCommandContext(bgCtx, &readarr.CommandRequest{Name: "RefreshMonitoredDownloads"})
					}
				case "whisparr":
					client, err := m.clients.GetOrCreateWhisparrClient(inst.Name, inst.URL, inst.APIKey)
					if err == nil {
						_, _ = client.SendCommandContext(bgCtx, &sonarr.CommandRequest{Name: "RefreshMonitoredDownloads"})
					}
				}
				return nil, nil
			})
		}(instance)
	}
}

// radarrResolution is the read-only outcome of resolving which Radarr movie and
// movie-file a given path corresponds to. It performs no mutations so it can be
// shared by the repair path (which then blocklists/deletes/re-searches) and the
// corrupted-file triage (which uses it only to decide ownership). hasReplacement
// reports that the arr already holds a different healthy file for the movie
// (upgrade/re-import), which the repair path treats as "already satisfied".
type radarrResolution struct {
	movie          *radarr.Movie
	movieFileID    int64
	hasReplacement bool
}

// resolveRadarrOwnership resolves the Radarr movie/file for a path using the same
// id -> path -> tmdbId strategy as repair, performing only read-only lookups. It
// is the single source of truth for "does this arr own this file" shared by both
// triggerRadarrRescanByPath and the triage.
func (m *Manager) resolveRadarrOwnership(ctx context.Context, client *radarr.Radarr, filePath, relativePath, instanceName string, metadata *model.WebhookMetadata) (radarrResolution, error) {
	var res radarrResolution

	// ID-Based Precision: If we have the exact movie ID and file ID from metadata, use them directly
	if metadata != nil && metadata.Movie != nil && metadata.MovieFile != nil && metadata.Movie.Id > 0 && metadata.MovieFile.Id > 0 {
		slog.InfoContext(ctx, "ID-Based Precision: Using metadata IDs for Radarr repair",
			"movie_id", metadata.Movie.Id,
			"movie_file_id", metadata.MovieFile.Id)

		// Targeted lookup by internal id (GET /api/v3/movie/{id}) instead of
		// fetching the entire library and scanning in memory: on large libraries
		// the full movie list can exceed the HTTP client timeout. A stale id
		// (movie removed+re-added gets a new internal id) returns an error here;
		// fall through to the path-based fallback rather than failing the repair.
		movie, lookupErr := client.GetMovieByIDContext(ctx, metadata.Movie.Id)
		if lookupErr != nil {
			slog.DebugContext(ctx, "Targeted Radarr movie lookup by id failed, falling back",
				"movie_id", metadata.Movie.Id, "error", lookupErr)
		} else if movie != nil {
			res.movie = movie

			// Smart Repair Guard: Check if the movie already has a newer/different healthy file
			if movie.HasFile && movie.MovieFile != nil {
				if movie.MovieFile.ID != metadata.MovieFile.Id {
					slog.InfoContext(ctx, "Smart Repair Guard: Movie already has a different healthy file (likely upgraded). Skipping repair.",
						"movie", movie.Title,
						"old_file_id", metadata.MovieFile.Id,
						"new_file_id", movie.MovieFile.ID)
					res.hasReplacement = true
					return res, nil
				}
				res.movieFileID = movie.MovieFile.ID
			}
		}
	}

	// Fallback to path-based guessing if ID-based precision failed or metadata was missing
	if res.movie == nil {
		slog.DebugContext(ctx, "Falling back to path-based guessing for Radarr", "file_path", filePath)
		// Get all movies to find the one with matching file path
		movies, err := m.data.GetMovies(ctx, client, instanceName)
		if err != nil {
			return res, fmt.Errorf("failed to get movies from Radarr: %w", err)
		}

		for _, movie := range movies {
			// Try match by filename (the most robust way if paths differ)
			requestFileName := filepath.Base(filePath)

			if movie.HasFile && movie.MovieFile != nil {
				// Try exact match
				if movie.MovieFile.Path == filePath {
					res.movie = movie
					res.movieFileID = movie.MovieFile.ID
					break
				}

				movieFileName := filepath.Base(movie.MovieFile.Path)
				if movieFileName == requestFileName {
					slog.InfoContext(ctx, "Found Radarr movie match by filename",
						"movie", movie.Title,
						"path", movie.MovieFile.Path)
					res.movie = movie
					res.movieFileID = movie.MovieFile.ID
					break
				}

				// Try match without .strm extension if filePath is a .strm file
				if before, ok := strings.CutSuffix(filePath, ".strm"); ok {
					strippedPath := before
					// Check if movie file path (without its own extension) matches stripped filePath
					if strings.TrimSuffix(movie.MovieFile.Path, filepath.Ext(movie.MovieFile.Path)) == strippedPath {
						res.movie = movie
						res.movieFileID = movie.MovieFile.ID
						break
					}
				}
				// Try suffix match with relative path if provided
				if relativePath != "" {
					strippedRelative := strings.TrimSuffix(relativePath, ".strm")
					if strings.HasSuffix(movie.MovieFile.Path, relativePath) ||
						strings.HasSuffix(strings.TrimSuffix(movie.MovieFile.Path, filepath.Ext(movie.MovieFile.Path)), strippedRelative) {
						slog.InfoContext(ctx, "Found Radarr movie match by relative path suffix",
							"radarr_path", movie.MovieFile.Path,
							"relative_path", relativePath)
						res.movie = movie
						res.movieFileID = movie.MovieFile.ID
						break
					}
				}
			}
		}
	}

	// Stable-id fallback: when both the internal id and the path failed to
	// resolve the movie, retry with the immutable tmdbId. The internal Movie.Id
	// changes when a movie is removed and re-added in Radarr, but the tmdbId is
	// stable, so this recovers a movie the arr still owns. Targeted
	// GET /api/v3/movie?tmdbId= avoids fetching the whole library.
	if res.movie == nil && metadata != nil && metadata.Movie != nil && metadata.Movie.TmdbId > 0 {
		slog.InfoContext(ctx, "Resolving Radarr movie by stable tmdbId after id/path match failed",
			"tmdb_id", metadata.Movie.TmdbId)
		movies, lookupErr := client.GetMovieContext(ctx, &radarr.GetMovie{TMDBID: metadata.Movie.TmdbId})
		if lookupErr != nil {
			slog.DebugContext(ctx, "Targeted Radarr movie lookup by tmdbId failed, falling back",
				"tmdb_id", metadata.Movie.TmdbId, "error", lookupErr)
		} else {
			for _, movie := range movies {
				if movie.TmdbID == metadata.Movie.TmdbId {
					res.movie = movie
					if movie.HasFile && movie.MovieFile != nil {
						res.movieFileID = movie.MovieFile.ID
					}
					break
				}
			}
		}
	}

	return res, nil
}

// sonarrResolution is the read-only outcome of resolving which Sonarr series,
// episode-file and episodes a given path corresponds to. Shared by repair and
// triage; performs no mutations. hasReplacement reports the arr already holds a
// different healthy file for the episode (upgrade/re-import).
type sonarrResolution struct {
	seriesFound    bool
	seriesID       int64
	seriesTitle    string
	episodeFileID  int64
	episodeIDs     []int64
	hasReplacement bool
}

// resolveSonarrOwnership resolves the Sonarr series/episode-file for a path using
// the same id -> path -> season+episode strategy as repair, read-only. It is the
// single source of truth for ownership shared by triggerSonarrRescanByPath and
// the triage.
func (m *Manager) resolveSonarrOwnership(ctx context.Context, client *sonarr.Sonarr, filePath, relativePath, instanceName string, metadata *model.WebhookMetadata) (sonarrResolution, error) {
	var res sonarrResolution

	// ID-Based Precision: If we have exact IDs from metadata, use them
	if metadata != nil && metadata.Series != nil && metadata.EpisodeFile != nil && metadata.Series.Id > 0 && metadata.EpisodeFile.Id > 0 {
		slog.InfoContext(ctx, "ID-Based Precision: Using metadata IDs for Sonarr repair",
			"series_id", metadata.Series.Id,
			"episode_file_id", metadata.EpisodeFile.Id)

		res.seriesFound = true
		res.seriesID = metadata.Series.Id
		res.episodeFileID = metadata.EpisodeFile.Id
		res.seriesTitle = "Known Series (ID Based)" // Title is just for logging
	}

	// Fallback to path-based guessing if ID-based precision failed or metadata was missing
	if res.seriesID == 0 {
		slog.DebugContext(ctx, "Falling back to path-based guessing for Sonarr", "file_path", filePath)

		// Get library directory from health config
		libraryDir := m.configGetter().MountPath
		cfg := m.configGetter()
		if cfg.Health.LibraryDir != nil && *cfg.Health.LibraryDir != "" {
			libraryDir = *cfg.Health.LibraryDir
		}

		slog.DebugContext(ctx, "Searching Sonarr for matching series by path",
			"library_dir", libraryDir)

		// Get all series to find the one that contains this file path
		series, err := m.data.GetSeries(ctx, client, instanceName)
		if err != nil {
			return res, fmt.Errorf("failed to get series from Sonarr: %w", err)
		}

		// Find the series that contains this file path
		var targetSeries *sonarr.Series
		for _, show := range series {
			if strings.Contains(filePath, show.Path) {
				targetSeries = show
				break
			}
		}

		if targetSeries == nil {
			// Fallback search for series using relative path
			for _, show := range series {
				showFolderName := filepath.Base(show.Path)
				if strings.Contains(relativePath, showFolderName) {
					slog.InfoContext(ctx, "Found series match by folder name", "series", show.Title, "folder", showFolderName)
					targetSeries = show
					break
				}
			}
		}

		if targetSeries == nil {
			// Series not found in the library: leave seriesFound=false for the
			// caller to handle (queue fallback / path-match-failed).
			return res, nil
		}

		res.seriesFound = true
		res.seriesID = targetSeries.ID
		res.seriesTitle = targetSeries.Title

		slog.InfoContext(ctx, "Found matching series, searching for episode file",
			"series_title", res.seriesTitle,
			"series_path", targetSeries.Path)

		// Get episode files for this series to find the matching file
		episodeFiles, err := m.data.GetEpisodeFiles(ctx, client, instanceName, res.seriesID)
		if err != nil {
			return res, fmt.Errorf("failed to get episode files for series %s: %w", res.seriesTitle, err)
		}

		// Find the episode file with matching path
		var targetEpisodeFile *sonarr.EpisodeFile
		for _, episodeFile := range episodeFiles {
			if episodeFile.Path == filePath {
				targetEpisodeFile = episodeFile
				break
			}

			// Try match by filename
			if filepath.Base(episodeFile.Path) == filepath.Base(filePath) {
				slog.InfoContext(ctx, "Found Sonarr episode match by filename", "path", episodeFile.Path)
				targetEpisodeFile = episodeFile
				break
			}

			// Try match without .strm extension
			if before, ok := strings.CutSuffix(filePath, ".strm"); ok {
				strippedPath := before
				if strings.TrimSuffix(episodeFile.Path, filepath.Ext(episodeFile.Path)) == strippedPath {
					targetEpisodeFile = episodeFile
					break
				}
			}

			// Try match with relative path
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
			res.episodeFileID = targetEpisodeFile.ID
		}
	}

	// Get all episodes for this specific series
	episodes, err := client.GetSeriesEpisodesContext(ctx, &sonarr.GetEpisode{
		SeriesID: res.seriesID,
	})
	if err != nil {
		return res, fmt.Errorf("failed to get episodes for series %s: %w", res.seriesTitle, err)
	}

	// Class-2 path-match fallback: when the mutable episode-file id and the
	// path/filename both failed to resolve the file, fall back to the stable
	// season+episode identity parsed from the path. The internal EpisodeFile.Id
	// changes on re-import and the path changes on rename, but season+episode is
	// stable, so this recovers a dead episode the arr still owns (HasFile).
	// Ambiguous inputs (dailies, anime absolute numbering, multi-episode files)
	// return ok=false from parseSeasonEpisode and are left as corrupted.
	if res.episodeFileID == 0 {
		if season, epNum, ok := parseSeasonEpisode(filePath); ok {
			for _, episode := range episodes {
				if episode.SeasonNumber == season && episode.EpisodeNumber == epNum &&
					episode.HasFile && episode.EpisodeFileID > 0 {
					slog.InfoContext(ctx, "Found Sonarr episode by stable season+episode after id/path match failed",
						"series", res.seriesTitle,
						"season", season,
						"episode", epNum,
						"episode_file_id", episode.EpisodeFileID)
					res.episodeFileID = episode.EpisodeFileID
					break
				}
			}
		}
	}

	if res.episodeFileID > 0 {
		// Found the file record - get episodes linked to it
		for _, episode := range episodes {
			if episode.EpisodeFileID == res.episodeFileID {
				res.episodeIDs = append(res.episodeIDs, episode.ID)
			}
		}

		// Smart Repair Guard: If we had a file ID but it's no longer found linked to any episode,
		// it's likely been upgraded or deleted.
		if len(res.episodeIDs) == 0 {
			slog.InfoContext(ctx, "Smart Repair Guard: Episode file ID is no longer active. Checking for newer replacements.",
				"old_file_id", res.episodeFileID)

			// Try to find if any episode currently has a different file at the same path or with same scene name
			episodeFiles, err := m.data.GetEpisodeFiles(ctx, client, instanceName, res.seriesID)
			if err == nil {
				for _, ef := range episodeFiles {
					if ef.Path == filePath || (metadata != nil && metadata.EpisodeFile != nil && ef.SceneName == metadata.EpisodeFile.SceneName) {
						slog.InfoContext(ctx, "Smart Repair Guard: Episode already has a different healthy file (likely upgraded). Skipping repair.",
							"old_file_id", res.episodeFileID,
							"new_file_id", ef.ID)
						res.hasReplacement = true
						return res, nil
					}
				}
			}
		}
	}

	return res, nil
}

// OwnershipResult is the read-only, arr-type-agnostic ownership outcome used by
// the corrupted-file triage to decide whether a file is safe to soft-delete.
//
// Fail-closed contract: when LookupOK is false, ownership could NOT be
// determined (no managing instance could be confirmed, an arr type we cannot
// introspect may own it, or an arr lookup errored/timed out). Callers MUST
// treat LookupOK==false as "do not delete" — never as "unowned".
type OwnershipResult struct {
	// LookupOK is true only when every lookup needed to reach the verdict
	// succeeded.
	LookupOK bool
	// Managed is true when a configured arr manages this path (root-folder
	// match, or the webhook named the instance). Only meaningful when LookupOK.
	Managed bool
	// HasReplacement is true when the managing arr already holds a different
	// healthy file for the item (upgrade/re-import). Only meaningful when
	// LookupOK && Managed.
	HasReplacement bool
	// InstanceName is the managing instance (for logging), if one was found.
	InstanceName string
}

// ResolveOwnership determines, read-only and fail-closed, whether any configured
// arr owns the given file and whether the owner already holds a replacement. It
// reuses the same per-type resolvers as repair so triage and repair agree on
// ownership. Any lookup error yields LookupOK=false (do not delete).
func (m *Manager) ResolveOwnership(ctx context.Context, filePath, relativePath string, metadataStr *string) OwnershipResult {
	var metadata *model.WebhookMetadata
	if metadataStr != nil && *metadataStr != "" {
		var parsed model.WebhookMetadata
		if err := json.Unmarshal([]byte(*metadataStr), &parsed); err == nil {
			metadata = &parsed
		}
	}

	allInstances := m.instances.GetAllInstances()

	// 1. If webhook metadata names the managing instance, trust it: the file is
	//    arr-managed. Assess replacement on that instance directly.
	if metadata != nil && metadata.InstanceName != "" {
		for _, inst := range allInstances {
			if inst == nil || !inst.Enabled || inst.Name != metadata.InstanceName {
				continue
			}
			return m.ownershipForInstance(ctx, inst, filePath, relativePath, metadata)
		}
	}

	// 2. Otherwise find the managing instance by root folder, tracking lookup
	//    errors so an unreachable/unintrospectable arr is never mistaken for
	//    "unowned".
	sawError := false
	for _, inst := range allInstances {
		if inst == nil || !inst.Enabled {
			continue
		}
		managed, ok := m.instanceManagesPath(ctx, inst, filePath)
		if !ok {
			sawError = true
			continue
		}
		if managed {
			return m.ownershipForInstance(ctx, inst, filePath, relativePath, metadata)
		}
	}

	if sawError {
		// At least one enabled arr could not be queried (error) or cannot be
		// introspected (e.g. sportarr) — we cannot prove the file is unowned.
		return OwnershipResult{LookupOK: false}
	}

	// Every enabled arr was queried successfully and none manages this path.
	return OwnershipResult{LookupOK: true, Managed: false}
}

// ownershipForInstance assesses replacement state on a known managing instance,
// reusing the repair resolvers. A resolver/client error yields LookupOK=false.
func (m *Manager) ownershipForInstance(ctx context.Context, inst *model.ConfigInstance, filePath, relativePath string, metadata *model.WebhookMetadata) OwnershipResult {
	res := OwnershipResult{LookupOK: true, Managed: true, InstanceName: inst.Name}
	switch inst.Type {
	case "radarr":
		client, err := m.clients.GetOrCreateRadarrClient(inst.Name, inst.URL, inst.APIKey)
		if err != nil {
			return OwnershipResult{LookupOK: false}
		}
		r, err := m.resolveRadarrOwnership(ctx, client, filePath, relativePath, inst.Name, metadata)
		if err != nil {
			return OwnershipResult{LookupOK: false}
		}
		res.HasReplacement = r.hasReplacement
		return res
	case "sonarr", "whisparr":
		var (
			client *sonarr.Sonarr
			err    error
		)
		if inst.Type == "whisparr" {
			client, err = m.clients.GetOrCreateWhisparrClient(inst.Name, inst.URL, inst.APIKey)
		} else {
			client, err = m.clients.GetOrCreateSonarrClient(inst.Name, inst.URL, inst.APIKey)
		}
		if err != nil {
			return OwnershipResult{LookupOK: false}
		}
		r, err := m.resolveSonarrOwnership(ctx, client, filePath, relativePath, inst.Name, metadata)
		if err != nil {
			return OwnershipResult{LookupOK: false}
		}
		res.HasReplacement = r.hasReplacement
		return res
	default:
		// lidarr/readarr/sportarr: the file is managed but we cannot assess
		// replacement, so report it as an only-copy (HasReplacement=false). The
		// triage then leaves it alone.
		return res
	}
}

// instanceManagesPath reports whether an instance's root folders cover filePath.
// ok=false means the check could not be performed (client/lookup error, or an
// arr type without a root-folder check such as sportarr); callers must treat
// that as "unknown", not "not managed", to stay fail-closed.
func (m *Manager) instanceManagesPath(ctx context.Context, inst *model.ConfigInstance, filePath string) (managed bool, ok bool) {
	switch inst.Type {
	case "radarr":
		client, err := m.clients.GetOrCreateRadarrClient(inst.Name, inst.URL, inst.APIKey)
		if err != nil {
			return false, false
		}
		folders, err := client.GetRootFoldersContext(ctx)
		if err != nil {
			return false, false
		}
		for _, f := range folders {
			if strings.HasPrefix(filePath, f.Path) {
				return true, true
			}
		}
		return false, true
	case "sonarr", "whisparr":
		var (
			client *sonarr.Sonarr
			err    error
		)
		if inst.Type == "whisparr" {
			client, err = m.clients.GetOrCreateWhisparrClient(inst.Name, inst.URL, inst.APIKey)
		} else {
			client, err = m.clients.GetOrCreateSonarrClient(inst.Name, inst.URL, inst.APIKey)
		}
		if err != nil {
			return false, false
		}
		folders, err := client.GetRootFoldersContext(ctx)
		if err != nil {
			return false, false
		}
		for _, f := range folders {
			if strings.HasPrefix(filePath, f.Path) {
				return true, true
			}
		}
		return false, true
	case "lidarr":
		client, err := m.clients.GetOrCreateLidarrClient(inst.Name, inst.URL, inst.APIKey)
		if err != nil {
			return false, false
		}
		folders, err := client.GetRootFoldersContext(ctx)
		if err != nil {
			return false, false
		}
		for _, f := range folders {
			if strings.HasPrefix(filePath, f.Path) {
				return true, true
			}
		}
		return false, true
	case "readarr":
		client, err := m.clients.GetOrCreateReadarrClient(inst.Name, inst.URL, inst.APIKey)
		if err != nil {
			return false, false
		}
		folders, err := client.GetRootFoldersContext(ctx)
		if err != nil {
			return false, false
		}
		for _, f := range folders {
			if strings.HasPrefix(filePath, f.Path) {
				return true, true
			}
		}
		return false, true
	default:
		// sportarr or unknown: no root-folder introspection available.
		return false, false
	}
}

// triggerRadarrRescanByPath triggers a rescan in Radarr for the given file path
func (m *Manager) triggerRadarrRescanByPath(ctx context.Context, client *radarr.Radarr, filePath, relativePath, instanceName string, metadata *model.WebhookMetadata) error {
	slog.InfoContext(ctx, "Searching Radarr for matching movie",
		"instance", instanceName,
		"file_path", filePath,
		"relative_path", relativePath)

	res, err := m.resolveRadarrOwnership(ctx, client, filePath, relativePath, instanceName, metadata)
	if err != nil {
		return err
	}

	// Smart Repair Guard: the arr already has a different healthy file.
	if res.hasReplacement {
		return model.ErrEpisodeAlreadySatisfied
	}

	targetMovie := res.movie
	targetMovieFileID := res.movieFileID

	if targetMovie == nil {
		slog.WarnContext(ctx, "No movie found with matching file path or ID in Radarr library, attempting queue-based failure",
			"instance", instanceName,
			"file_path", filePath)

		// Fallback: search in Radarr download queue for active/stuck imports
		if err := m.failRadarrQueueItemByPath(ctx, client, filePath); err == nil {
			return nil
		}

		return fmt.Errorf("no movie found with file path %s in library or queue: %w", filePath, model.ErrPathMatchFailed)
	}

	slog.InfoContext(ctx, "Found matching movie for file",
		"instance", instanceName,
		"movie_id", targetMovie.ID,
		"movie_title", targetMovie.Title,
		"movie_path", targetMovie.Path,
		"file_path", filePath)

	// If we found the movie and have a file ID, try to blocklist and delete the file
	if targetMovieFileID > 0 {
		// Try to blocklist the release associated with this file
		if err := m.blocklistRadarrMovieFile(ctx, client, targetMovie.ID, targetMovieFileID); err != nil {
			slog.WarnContext(ctx, "Failed to blocklist Radarr release", "error", err)
		}

		// Delete the existing file from Radarr database
		err = client.DeleteMovieFilesContext(ctx, targetMovieFileID)
		if err != nil {
			slog.WarnContext(ctx, "Failed to delete movie file from Radarr, continuing with search",
				"instance", instanceName,
				"movie_id", targetMovie.ID,
				"file_id", targetMovieFileID,
				"error", err)
		}
	} else {
		slog.InfoContext(ctx, "Movie has no specific file ID linked in Radarr, skipping blocklist/delete and proceeding to search",
			"movie", targetMovie.Title)
	}

	// Failure breaker: every targeted re-search counts one failure-driven action
	// against the movie. At the threshold the movie is unmonitored instead of
	// re-searched so a dead release can't drive an endless re-grab storm.
	if m.movieBreakerTripped(instanceName, targetMovie.ID) {
		unmonitorRadarrMovie(ctx, client, instanceName, targetMovie.ID)
		return nil
	}

	// Step 3: Trigger targeted search for the missing movie
	searchCmd := &radarr.CommandRequest{
		Name:     "MoviesSearch",
		MovieIDs: []int64{targetMovie.ID},
	}

	response, err := client.SendCommandContext(ctx, searchCmd)
	if err != nil {
		return fmt.Errorf("failed to trigger Radarr search for movie ID %d: %w", targetMovie.ID, err)
	}

	slog.InfoContext(ctx, "Successfully triggered Radarr targeted search for re-download",
		"instance", instanceName,
		"movie_id", targetMovie.ID,
		"command_id", response.ID)

	return nil
}

// triggerSonarrRescanByPath triggers a rescan in Sonarr for the given file path
func (m *Manager) triggerSonarrRescanByPath(ctx context.Context, client *sonarr.Sonarr, filePath, relativePath, instanceName string, metadata *model.WebhookMetadata) error {
	slog.InfoContext(ctx, "Searching Sonarr for matching series",
		"instance", instanceName,
		"file_path", filePath,
		"relative_path", relativePath)

	res, err := m.resolveSonarrOwnership(ctx, client, filePath, relativePath, instanceName, metadata)
	if err != nil {
		return err
	}

	if !res.seriesFound {
		slog.WarnContext(ctx, "No series found in Sonarr matching file path in library, attempting queue-based failure",
			"instance", instanceName,
			"file_path", filePath)

		// Fallback: search in Sonarr download queue for active/stuck imports
		if err := m.failSonarrQueueItemByPath(ctx, client, filePath); err == nil {
			return nil
		}

		return fmt.Errorf("no series found containing file path in library or queue: %s: %w", filePath, model.ErrPathMatchFailed)
	}

	// Smart Repair Guard: the arr already has a different healthy file.
	if res.hasReplacement {
		return model.ErrEpisodeAlreadySatisfied
	}

	if res.episodeFileID > 0 {
		if len(res.episodeIDs) > 0 {
			slog.DebugContext(ctx, "Found matching episodes by file ID",
				"episode_count", len(res.episodeIDs),
				"episode_file_id", res.episodeFileID)

			// Try to blocklist the release associated with this file
			if err := m.blocklistSonarrEpisodeFile(ctx, client, res.seriesID, res.episodeFileID); err != nil {
				slog.WarnContext(ctx, "Failed to blocklist Sonarr release", "error", err)
			}

			// Delete the existing episode file from Sonarr database
			if err := client.DeleteEpisodeFileContext(ctx, res.episodeFileID); err != nil {
				slog.WarnContext(ctx, "Failed to delete episode file from Sonarr, continuing with search",
					"instance", instanceName,
					"episode_file_id", res.episodeFileID,
					"error", err)
			}
		}
	} else {
		slog.WarnContext(ctx, "Series found but no matching episode file ID found, attempting queue-based failure",
			"series", res.seriesTitle,
			"file_path", filePath)

		// Fallback: search in Sonarr download queue
		if err := m.failSonarrQueueItemByPath(ctx, client, filePath); err == nil {
			return nil
		}
	}

	if len(res.episodeIDs) == 0 {
		return fmt.Errorf("no episodes found for file in library or queue: %s: %w", filePath, model.ErrPathMatchFailed)
	}

	// Failure breaker: every targeted re-search counts one failure-driven action
	// against each episode. Episodes at the threshold are unmonitored instead of
	// re-searched so a dead release can't drive an endless re-grab storm.
	searchIDs, giveUpIDs := m.splitEpisodesByBreaker(instanceName, res.episodeIDs)
	unmonitorSonarrEpisodes(ctx, client, instanceName, giveUpIDs)
	if len(searchIDs) == 0 {
		return nil
	}

	// Trigger targeted episode search for the remaining episodes in this file
	searchCmd := &sonarr.CommandRequest{
		Name:       "EpisodeSearch",
		EpisodeIDs: searchIDs,
	}

	response, err := client.SendCommandContext(ctx, searchCmd)
	if err != nil {
		return fmt.Errorf("failed to trigger Sonarr episode search: %w", err)
	}

	slog.InfoContext(ctx, "Successfully triggered Sonarr targeted episode search for re-download",
		"instance", instanceName,
		"series_title", res.seriesTitle,
		"episode_ids", searchIDs,
		"command_id", response.ID)

	return nil
}

// failRadarrQueueItemByPath searches for an item in the active Radarr queue by path and marks it as failed
func (m *Manager) failRadarrQueueItemByPath(ctx context.Context, client *radarr.Radarr, path string) error {
	queue, err := client.GetQueueContext(ctx, 0, 500)
	if err != nil {
		return fmt.Errorf("failed to get Radarr queue: %w", err)
	}

	for _, q := range queue.Records {
		// Try exact match, prefix match (if queue item is parent dir), or filename match
		if q.OutputPath == path ||
			(q.OutputPath != "" && strings.HasPrefix(filepath.ToSlash(path), filepath.ToSlash(q.OutputPath))) ||
			(q.OutputPath != "" && filepath.Base(q.OutputPath) == filepath.Base(path)) {
			slog.InfoContext(ctx, "Found matching item in Radarr download queue, marking as failed",
				"queue_id", q.ID, "path", path, "output_path", q.OutputPath)

			removeFromClient := true
			opts := &starr.QueueDeleteOpts{
				RemoveFromClient: &removeFromClient,
				BlockList:        true,
				SkipRedownload:   false,
			}
			return client.DeleteQueueContext(ctx, q.ID, opts)
		}
	}

	return fmt.Errorf("no matching item found in Radarr queue for path: %s", path)
}

// failSonarrQueueItemByPath searches for an item in the active Sonarr queue by path and marks it as failed
func (m *Manager) failSonarrQueueItemByPath(ctx context.Context, client *sonarr.Sonarr, path string) error {
	queue, err := client.GetQueueContext(ctx, 0, 500)
	if err != nil {
		return fmt.Errorf("failed to get Sonarr queue: %w", err)
	}

	for _, q := range queue.Records {
		// Try exact match, prefix match (if queue item is parent dir), or filename match
		if q.OutputPath == path ||
			(q.OutputPath != "" && strings.HasPrefix(filepath.ToSlash(path), filepath.ToSlash(q.OutputPath))) ||
			(q.OutputPath != "" && filepath.Base(q.OutputPath) == filepath.Base(path)) {
			slog.InfoContext(ctx, "Found matching item in Sonarr download queue, marking as failed",
				"queue_id", q.ID, "path", path, "output_path", q.OutputPath)

			removeFromClient := true
			opts := &starr.QueueDeleteOpts{
				RemoveFromClient: &removeFromClient,
				BlockList:        true,
				SkipRedownload:   false,
			}
			return client.DeleteQueueContext(ctx, q.ID, opts)
		}
	}

	return fmt.Errorf("no matching item found in Sonarr queue for path: %s", path)
}

// blocklistRadarrMovieFile finds the history event for the given file and marks it as failed (blocklisting the release)
func (m *Manager) blocklistRadarrMovieFile(ctx context.Context, client *radarr.Radarr, movieID int64, fileID int64) error {
	slog.DebugContext(ctx, "Attempting to find and blocklist release for movie file", "movie_id", movieID, "file_id", fileID)

	// Fetch history for this specific movie
	req := &starr.PageReq{PageSize: 100, SortKey: "date", SortDir: starr.SortDescend}
	req.Set("movieId", strconv.FormatInt(movieID, 10))

	history, err := client.GetHistoryPageContext(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to fetch Radarr history: %w", err)
	}

	targetFileID := strconv.FormatInt(fileID, 10)
	var downloadID string

	// 1. Find the import event to get the downloadId
	for _, record := range history.Records {
		if record.Data.FileID == targetFileID && (record.EventType == "movieFileImported" || record.EventType == "downloadFolderImported") {
			downloadID = record.DownloadID
			break
		}
	}

	if downloadID == "" {
		slog.WarnContext(ctx, "Could not find import event in Radarr history for file", "movie_id", movieID, "file_id", fileID)
		return nil
	}

	// 2. Find the original grab event using the downloadId
	for _, record := range history.Records {
		if record.DownloadID == downloadID && record.EventType == "grabbed" {
			slog.InfoContext(ctx, "Found grabbed history record, marking as failed to blocklist release",
				"history_id", record.ID, "download_id", downloadID)
			if failErr := client.FailContext(ctx, record.ID); failErr != nil {
				return fmt.Errorf("failed to fail Radarr grab event %d: %w", record.ID, failErr)
			}
			return nil
		}
	}

	slog.WarnContext(ctx, "Could not find grab event in Radarr history for download", "download_id", downloadID)
	return nil
}

// blocklistSonarrEpisodeFile finds the grabbed history event for the given file and marks it as failed (blocklisting the release)
func (m *Manager) blocklistSonarrEpisodeFile(ctx context.Context, client *sonarr.Sonarr, seriesID int64, fileID int64) error {
	slog.DebugContext(ctx, "Attempting to find and blocklist release for episode file", "series_id", seriesID, "file_id", fileID)

	// Fetch history for this specific series
	req := &starr.PageReq{PageSize: 100, SortKey: "date", SortDir: starr.SortDescend}
	req.Set("seriesId", strconv.FormatInt(seriesID, 10))

	history, err := client.GetHistoryPageContext(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to fetch Sonarr history: %w", err)
	}

	targetFileID := strconv.FormatInt(fileID, 10)
	var downloadID string

	// 1. Find the import event to get the downloadId
	for _, record := range history.Records {
		if record.Data.FileID == targetFileID && record.EventType == "downloadFolderImported" {
			downloadID = record.DownloadID
			break
		}
	}

	if downloadID == "" {
		slog.WarnContext(ctx, "Could not find import event in Sonarr history for file", "series_id", seriesID, "file_id", fileID)
		return nil
	}

	// 2. Find the original grab event using the downloadId
	for _, record := range history.Records {
		if record.DownloadID == downloadID && record.EventType == "grabbed" {
			slog.InfoContext(ctx, "Found grabbed history record, marking as failed to blocklist release",
				"history_id", record.ID, "download_id", downloadID)
			if failErr := client.FailContext(ctx, record.ID); failErr != nil {
				return fmt.Errorf("failed to fail Sonarr grab event %d: %w", record.ID, failErr)
			}
			return nil
		}
	}
	slog.WarnContext(ctx, "Could not find grab event in Sonarr history for download", "download_id", downloadID)
	return nil
}

// triggerLidarrRescanByPath triggers a rescan in Lidarr
func (m *Manager) triggerLidarrRescanByPath(ctx context.Context, client *lidarr.Lidarr, filePath, relativePath, instanceName string, metadata *model.WebhookMetadata) error {
	slog.InfoContext(ctx, "Searching Lidarr for matching track", "instance", instanceName, "file_path", filePath)

	var targetAlbumID int64
	var targetTrackFileID int64

	if metadata != nil && metadata.Album != nil && metadata.TrackFile != nil && metadata.Album.Id > 0 && metadata.TrackFile.Id > 0 {
		targetAlbumID = metadata.Album.Id
		targetTrackFileID = metadata.TrackFile.Id
	}

	if targetAlbumID == 0 {
		m.TriggerScanForFile(ctx, filePath)
		return nil
	}

	if targetTrackFileID > 0 {
		if err := m.blocklistLidarrTrackFile(ctx, client, targetAlbumID, targetTrackFileID); err != nil {
			slog.WarnContext(ctx, "Failed to blocklist Lidarr release", "error", err)
		}
		_ = client.DeleteTrackFileContext(ctx, targetTrackFileID)
	}

	searchCmd := &lidarr.CommandRequest{
		Name:     "AlbumSearch",
		AlbumIDs: []int64{targetAlbumID},
	}
	_, err := client.SendCommandContext(ctx, searchCmd)
	if err != nil {
		return fmt.Errorf("failed to trigger Lidarr search: %w", err)
	}

	return nil
}

// blocklistLidarrTrackFile marks a track file as failed
func (m *Manager) blocklistLidarrTrackFile(ctx context.Context, client *lidarr.Lidarr, albumID int64, fileID int64) error {
	req := &starr.PageReq{PageSize: 100, SortKey: "date", SortDir: starr.SortDescend}
	req.Set("albumId", strconv.FormatInt(albumID, 10))

	history, err := client.GetHistoryPageContext(ctx, req)
	if err != nil {
		return err
	}

	var downloadID string

	for _, record := range history.Records {
		// Attempting Data.TrackFileId or fallback if needed
		if record.EventType == "trackFileImported" {
			// Without knowing exact starr mapping, checking DownloadID is safer if it's there
			downloadID = record.DownloadID
			break
		}
	}

	if downloadID == "" {
		return nil
	}

	for _, record := range history.Records {
		if record.DownloadID == downloadID && record.EventType == "grabbed" {
			return client.FailContext(ctx, record.ID)
		}
	}
	return nil
}

// triggerReadarrRescanByPath triggers a rescan in Readarr
func (m *Manager) triggerReadarrRescanByPath(ctx context.Context, client *readarr.Readarr, filePath, relativePath, instanceName string, metadata *model.WebhookMetadata) error {
	slog.InfoContext(ctx, "Searching Readarr for matching book", "instance", instanceName, "file_path", filePath)

	var targetBookID int64
	var targetBookFileID int64

	if metadata != nil && metadata.Book != nil && metadata.BookFile != nil && metadata.Book.Id > 0 && metadata.BookFile.Id > 0 {
		targetBookID = metadata.Book.Id
		targetBookFileID = metadata.BookFile.Id
	}

	if targetBookID == 0 {
		m.TriggerScanForFile(ctx, filePath)
		return nil
	}

	if targetBookFileID > 0 {
		if err := m.blocklistReadarrBookFile(ctx, client, targetBookID, targetBookFileID); err != nil {
			slog.WarnContext(ctx, "Failed to blocklist Readarr release", "error", err)
		}
		_ = client.DeleteBookFileContext(ctx, targetBookFileID)
	}

	searchCmd := &readarr.CommandRequest{
		Name:    "BookSearch",
		BookIDs: []int64{targetBookID},
	}
	_, err := client.SendCommandContext(ctx, searchCmd)
	if err != nil {
		return fmt.Errorf("failed to trigger Readarr search: %w", err)
	}

	return nil
}

// blocklistReadarrBookFile marks a book file as failed
func (m *Manager) blocklistReadarrBookFile(ctx context.Context, client *readarr.Readarr, bookID int64, fileID int64) error {
	req := &starr.PageReq{PageSize: 100, SortKey: "date", SortDir: starr.SortDescend}
	req.Set("bookId", strconv.FormatInt(bookID, 10))

	history, err := client.GetHistoryPageContext(ctx, req)
	if err != nil {
		return err
	}

	var downloadID string

	for _, record := range history.Records {
		if record.EventType == "bookFileImported" {
			downloadID = record.DownloadID
			break
		}
	}

	if downloadID == "" {
		return nil
	}

	for _, record := range history.Records {
		if record.DownloadID == downloadID && record.EventType == "grabbed" {
			return client.FailContext(ctx, record.ID)
		}
	}
	return nil
}

package scanner

import (
	"context"
	"log/slog"
	"net/url"
	"path/filepath"

	"github.com/javi11/altmount/internal/arrs/model"
	"golift.io/starr"
	"golift.io/starr/lidarr"
	"golift.io/starr/readarr"
	"golift.io/starr/sonarr"
)

// ResolveOwnership answers, read-only and fail-closed, whether any *arr instance
// currently owns filePath. It never performs writes.
//
// Fail-closed contract: the result defaults to model.OwnershipUnknown and is
// only ever promoted to Unowned/Replaced on a *positive* determination. Any
// error, timeout, unreachable instance, or non-introspectable arr leaves the
// status Unknown so that callers (e.g. corrupted-file triage) never delete on
// ambiguity. Radarr, Sonarr, Whisparr (Sonarr-compatible), Lidarr, and Readarr
// are introspected; Sportarr and anything else stay Unknown.
func (m *Manager) ResolveOwnership(ctx context.Context, filePath, relativePath string, metadata *model.WebhookMetadata) model.Ownership {
	res := model.Ownership{Status: model.OwnershipUnknown}

	instanceType, instanceName := m.findOwningInstance(ctx, filePath, relativePath, metadata)
	if instanceType == "" || instanceName == "" {
		slog.DebugContext(ctx, "Ownership unknown: no managing instance resolved", "file_path", filePath)
		return res
	}
	res.InstanceType = instanceType
	res.InstanceName = instanceName

	instanceConfig, err := m.instances.FindConfigInstance(instanceType, instanceName)
	if err != nil || instanceConfig == nil || !instanceConfig.Enabled {
		slog.DebugContext(ctx, "Ownership unknown: instance config missing or disabled",
			"instance", instanceName, "type", instanceType)
		return res
	}

	switch instanceType {
	case "radarr":
		client, err := m.clients.GetOrCreateRadarrClient(instanceName, instanceConfig.URL, instanceConfig.APIKey)
		if err != nil {
			slog.WarnContext(ctx, "Ownership unknown: failed to create Radarr client", "instance", instanceName, "error", err)
			return res
		}
		own, err := m.resolveRadarrOwnership(ctx, client, filePath, relativePath, instanceName, metadata)
		if err != nil {
			slog.WarnContext(ctx, "Ownership unknown: Radarr lookup failed", "instance", instanceName, "error", err)
			return res
		}
		switch {
		case own.alreadySatisfied:
			res.Status = model.OwnershipReplaced
			if own.movie != nil && own.movie.MovieFile != nil {
				res.ReplacementID = own.movie.MovieFile.ID
			}
		case own.movie != nil:
			res.Status = model.OwnershipOwned
		case own.lookupErr:
			// A lookup failed and nothing matched; we cannot be sure it is unowned.
			res.Status = model.OwnershipUnknown
		default:
			res.Status = model.OwnershipUnowned
		}

	case "sonarr", "whisparr":
		// Whisparr speaks the Sonarr API and is created through the Whisparr client,
		// so it reuses the Sonarr resolver verbatim.
		client, err := m.sonarrCompatibleClient(instanceType, instanceName, instanceConfig.URL, instanceConfig.APIKey)
		if err != nil {
			slog.WarnContext(ctx, "Ownership unknown: failed to create Sonarr-compatible client", "instance", instanceName, "type", instanceType, "error", err)
			return res
		}
		status, replacementID := m.resolveSonarrOwnershipStatus(ctx, client, filePath, relativePath, instanceName, metadata)
		res.Status = status
		res.ReplacementID = replacementID

	case "lidarr":
		client, err := m.clients.GetOrCreateLidarrClient(instanceName, instanceConfig.URL, instanceConfig.APIKey)
		if err != nil {
			slog.WarnContext(ctx, "Ownership unknown: failed to create Lidarr client", "instance", instanceName, "error", err)
			return res
		}
		status, replacementID := m.resolveLidarrOwnershipStatus(ctx, client, filePath, relativePath, instanceName, metadata)
		res.Status = status
		res.ReplacementID = replacementID

	case "readarr":
		client, err := m.clients.GetOrCreateReadarrClient(instanceName, instanceConfig.URL, instanceConfig.APIKey)
		if err != nil {
			slog.WarnContext(ctx, "Ownership unknown: failed to create Readarr client", "instance", instanceName, "error", err)
			return res
		}
		status, replacementID := m.resolveReadarrOwnershipStatus(ctx, client, filePath, relativePath, instanceName, metadata)
		res.Status = status
		res.ReplacementID = replacementID

	default:
		// sportarr (native, non-starr API) and anything else are not introspected:
		// stay Unknown (fail closed) so triage never deletes their files.
		slog.DebugContext(ctx, "Ownership unknown: instance type not introspectable for triage",
			"instance", instanceName, "type", instanceType)
	}

	return res
}

// sonarrCompatibleClient returns a *sonarr.Sonarr for a Sonarr or Whisparr
// instance (Whisparr uses the Sonarr API).
func (m *Manager) sonarrCompatibleClient(instanceType, instanceName, instanceURL, apiKey string) (*sonarr.Sonarr, error) {
	if instanceType == "whisparr" {
		return m.clients.GetOrCreateWhisparrClient(instanceName, instanceURL, apiKey)
	}
	return m.clients.GetOrCreateSonarrClient(instanceName, instanceURL, apiKey)
}

// resolveSonarrOwnershipStatus turns a Sonarr ownership resolution into a
// fail-closed status. When the series owns the path it inspects the episode list
// (read-only) to distinguish a still-tracked dead file (Owned) from one that has
// already been replaced by a different healthy file (Replaced).
func (m *Manager) resolveSonarrOwnershipStatus(ctx context.Context, client *sonarr.Sonarr, filePath, relativePath, instanceName string, metadata *model.WebhookMetadata) (model.OwnershipStatus, int64) {
	own, err := m.resolveSonarrOwnership(ctx, client, filePath, relativePath, instanceName, metadata)
	if err != nil {
		slog.WarnContext(ctx, "Ownership unknown: Sonarr lookup failed", "instance", instanceName, "error", err)
		return model.OwnershipUnknown, 0
	}
	if own.seriesID == 0 {
		// GetSeries succeeded but no series matched the path: genuinely unowned.
		return model.OwnershipUnowned, 0
	}

	if own.episodeFileID == 0 {
		// The series matched but no specific file record lined up with our path. The
		// series still owns the area; treat as Owned (the arr drives repair) rather
		// than risk deleting a file the arr still expects. No episode fetch needed.
		return model.OwnershipOwned, 0
	}

	// Only now is an episode fetch worthwhile: we need it to tell a still-tracked
	// dead file (Owned) from one already replaced by a different healthy file.
	episodes, err := client.GetSeriesEpisodesContext(ctx, &sonarr.GetEpisode{SeriesID: own.seriesID})
	if err != nil {
		slog.WarnContext(ctx, "Ownership unknown: failed to fetch Sonarr episodes", "instance", instanceName, "error", err)
		return model.OwnershipUnknown, 0
	}

	// Is the (dead) file we resolved still the current file for an episode?
	for _, ep := range episodes {
		if ep.EpisodeFileID == own.episodeFileID {
			// The series still references this exact file; a repair will be
			// driven against it. Keep it.
			return model.OwnershipOwned, 0
		}
	}
	// The resolved file id is no longer referenced by any episode: it was
	// removed/upgraded. Look for a replacement at the same season/episode.
	if season, episode, ok := parseSeasonEpisodeFromPaths(filePath, relativePath); ok {
		for _, ep := range episodes {
			if ep.SeasonNumber == season && ep.EpisodeNumber == episode {
				if ep.HasFile && ep.EpisodeFileID > 0 {
					return model.OwnershipReplaced, ep.EpisodeFileID
				}
				// Episode tracked but currently fileless: the arr will re-grab.
				return model.OwnershipOwned, 0
			}
		}
	}
	// Stale file id we cannot map to a current episode: ambiguous, fail closed
	// toward keeping the file.
	return model.OwnershipOwned, 0
}

// parseSeasonEpisodeFromPaths tries the file path first, then the relative path,
// reusing the same SxxExx parser the repair path uses.
func parseSeasonEpisodeFromPaths(filePath, relativePath string) (season, episode int, ok bool) {
	if season, episode, ok = parseSonarrSeasonEpisode(filePath); ok {
		return season, episode, true
	}
	if relativePath != "" {
		return parseSonarrSeasonEpisode(relativePath)
	}
	return 0, 0, false
}

// findOwningInstance resolves the managing instance for a file, preferring the
// webhook metadata's instance name and falling back to path-based detection.
func (m *Manager) findOwningInstance(ctx context.Context, filePath, relativePath string, metadata *model.WebhookMetadata) (instanceType, instanceName string) {
	if metadata != nil && metadata.InstanceName != "" {
		for _, inst := range m.instances.GetAllInstances() {
			if inst.Name == metadata.InstanceName {
				return inst.Type, inst.Name
			}
		}
	}

	instanceType, instanceName, err := m.findInstanceForFilePath(ctx, filePath, relativePath)
	if err != nil {
		return "", ""
	}
	return instanceType, instanceName
}

// entityPath pairs an *arr entity id (artist/author) with its library folder.
type entityPath struct {
	id   int64
	path string
}

// findEntityByPath returns the id of the first entity whose folder owns filePath
// (component-boundary match), with a folder-name fallback against relativePath.
func findEntityByPath(entities []entityPath, filePath, relativePath string) (int64, bool) {
	for _, e := range entities {
		if e.path != "" && pathContainsDir(filePath, e.path) {
			return e.id, true
		}
	}
	if relativePath != "" {
		for _, e := range entities {
			if e.path != "" && hasPathComponent(relativePath, filepath.Base(e.path)) {
				return e.id, true
			}
		}
	}
	return 0, false
}

// resolveLidarrArtist resolves the Lidarr artist that owns filePath: targeted by
// the webhook's artist id, otherwise by matching the file against each artist's
// folder. found=false means no artist owns the path (genuinely unowned); a
// non-nil error means the lookup itself failed (fail closed).
func (m *Manager) resolveLidarrArtist(ctx context.Context, client *lidarr.Lidarr, filePath, relativePath, instanceName string, metadata *model.WebhookMetadata) (artistID int64, found bool, err error) {
	if metadata != nil && metadata.Artist != nil && metadata.Artist.Id > 0 {
		if a, e := client.GetArtistByIDContext(ctx, metadata.Artist.Id); e == nil && a != nil && a.ID > 0 {
			return a.ID, true, nil
		}
	}
	artists, e := client.GetArtistContext(ctx, "")
	if e != nil {
		slog.WarnContext(ctx, "Ownership unknown: Lidarr artist lookup failed", "instance", instanceName, "error", e)
		return 0, false, e
	}
	entities := make([]entityPath, 0, len(artists))
	for _, a := range artists {
		entities = append(entities, entityPath{id: a.ID, path: a.Path})
	}
	id, ok := findEntityByPath(entities, filePath, relativePath)
	return id, ok, nil
}

// resolveLidarrOwnershipStatus resolves read-only, fail-closed ownership for a
// Lidarr-managed (music) file at file granularity, mirroring the Sonarr resolver
// (artist→track files like series→episode files). No artist owns the path →
// Unowned; the dead track file (from webhook metadata) is still present → Owned;
// it is gone but the same album now has a different file → Replaced; any lookup
// error → Unknown.
func (m *Manager) resolveLidarrOwnershipStatus(ctx context.Context, client *lidarr.Lidarr, filePath, relativePath, instanceName string, metadata *model.WebhookMetadata) (model.OwnershipStatus, int64) {
	artistID, found, err := m.resolveLidarrArtist(ctx, client, filePath, relativePath, instanceName, metadata)
	if err != nil {
		return model.OwnershipUnknown, 0
	}
	if !found {
		return model.OwnershipUnowned, 0
	}

	var oldFileID int64
	if metadata != nil && metadata.TrackFile != nil {
		oldFileID = metadata.TrackFile.Id
	}
	if oldFileID == 0 {
		// Artist owns the area but there is no dead-file id to chase: keep.
		return model.OwnershipOwned, 0
	}

	trackFiles, err := client.GetTrackFilesForArtistContext(ctx, artistID)
	if err != nil {
		slog.WarnContext(ctx, "Ownership unknown: Lidarr track-file lookup failed", "instance", instanceName, "error", err)
		return model.OwnershipUnknown, 0
	}
	for _, tf := range trackFiles {
		if tf.ID == oldFileID {
			// The dead file is still referenced; a repair will be driven against it.
			return model.OwnershipOwned, 0
		}
	}
	// The dead file is gone: look for a replacement within the same album.
	// (oldFileID>0 already implies metadata!=nil; the guard makes that local.)
	if metadata != nil && metadata.Album != nil && metadata.Album.Id > 0 {
		if albumFiles, aerr := client.GetTrackFilesForAlbumContext(ctx, metadata.Album.Id); aerr == nil {
			for _, tf := range albumFiles {
				if tf.ID > 0 && tf.ID != oldFileID {
					return model.OwnershipReplaced, tf.ID
				}
			}
		}
	}
	// Artist owns the area but a replacement can't be confirmed: conservative keep.
	return model.OwnershipOwned, 0
}

// resolveReadarrAuthor resolves the Readarr author that owns filePath. Readarr
// has no typed "list all authors" helper in this starr version, so the path
// fallback issues the raw GET the typed helpers use under the hood.
func (m *Manager) resolveReadarrAuthor(ctx context.Context, client *readarr.Readarr, filePath, relativePath, instanceName string, metadata *model.WebhookMetadata) (authorID int64, found bool, err error) {
	if metadata != nil && metadata.Author != nil && metadata.Author.Id > 0 {
		if a, e := client.GetAuthorByIDContext(ctx, metadata.Author.Id); e == nil && a != nil && a.ID > 0 {
			return a.ID, true, nil
		}
	}
	var authors []*readarr.Author
	req := starr.Request{URI: "v1/author", Query: make(url.Values)}
	if e := client.GetInto(ctx, req, &authors); e != nil {
		slog.WarnContext(ctx, "Ownership unknown: Readarr author lookup failed", "instance", instanceName, "error", e)
		return 0, false, e
	}
	entities := make([]entityPath, 0, len(authors))
	for _, a := range authors {
		entities = append(entities, entityPath{id: a.ID, path: a.Path})
	}
	id, ok := findEntityByPath(entities, filePath, relativePath)
	return id, ok, nil
}

// resolveReadarrOwnershipStatus resolves read-only, fail-closed ownership for a
// Readarr-managed (book) file at file granularity, mirroring the Lidarr resolver
// (author→book files, with the book as the replacement scope).
func (m *Manager) resolveReadarrOwnershipStatus(ctx context.Context, client *readarr.Readarr, filePath, relativePath, instanceName string, metadata *model.WebhookMetadata) (model.OwnershipStatus, int64) {
	authorID, found, err := m.resolveReadarrAuthor(ctx, client, filePath, relativePath, instanceName, metadata)
	if err != nil {
		return model.OwnershipUnknown, 0
	}
	if !found {
		return model.OwnershipUnowned, 0
	}

	var oldFileID int64
	if metadata != nil && metadata.BookFile != nil {
		oldFileID = metadata.BookFile.Id
	}
	if oldFileID == 0 {
		return model.OwnershipOwned, 0
	}

	bookFiles, err := client.GetBookFilesForAuthorContext(ctx, authorID)
	if err != nil {
		slog.WarnContext(ctx, "Ownership unknown: Readarr book-file lookup failed", "instance", instanceName, "error", err)
		return model.OwnershipUnknown, 0
	}
	for _, bf := range bookFiles {
		if bf.ID == oldFileID {
			return model.OwnershipOwned, 0
		}
	}
	// The dead file is gone: look for a replacement of the same book.
	// (oldFileID>0 already implies metadata!=nil; the guard makes that local.)
	if metadata != nil && metadata.Book != nil && metadata.Book.Id > 0 {
		if bookFiles2, berr := client.GetBookFilesForBookContext(ctx, metadata.Book.Id); berr == nil {
			for _, bf := range bookFiles2 {
				if bf.ID > 0 && bf.ID != oldFileID {
					return model.OwnershipReplaced, bf.ID
				}
			}
		}
	}
	return model.OwnershipOwned, 0
}

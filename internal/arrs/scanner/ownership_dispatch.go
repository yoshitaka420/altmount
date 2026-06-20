package scanner

import (
	"context"
	"log/slog"

	"github.com/javi11/altmount/internal/arrs/model"
	"golift.io/starr/sonarr"
)

// ResolveOwnership answers, read-only and fail-closed, whether any *arr instance
// currently owns filePath. It never performs writes.
//
// Fail-closed contract: the result defaults to model.OwnershipUnknown and is
// only ever promoted to Unowned/Replaced on a *positive* determination. Any
// error, timeout, unreachable instance, or non-introspectable arr leaves the
// status Unknown so that callers (e.g. corrupted-file triage) never delete on
// ambiguity. Only Radarr and Sonarr expose the per-file ownership detail needed
// to make a safe determination; every other instance type is reported Unknown.
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

	case "sonarr":
		client, err := m.clients.GetOrCreateSonarrClient(instanceName, instanceConfig.URL, instanceConfig.APIKey)
		if err != nil {
			slog.WarnContext(ctx, "Ownership unknown: failed to create Sonarr client", "instance", instanceName, "error", err)
			return res
		}
		status, replacementID := m.resolveSonarrOwnershipStatus(ctx, client, filePath, relativePath, instanceName, metadata)
		res.Status = status
		res.ReplacementID = replacementID

	default:
		// lidarr, readarr, whisparr, sportarr, etc. are not introspected for
		// fine-grained file ownership here: stay Unknown (fail closed).
		slog.DebugContext(ctx, "Ownership unknown: instance type not introspectable for triage",
			"instance", instanceName, "type", instanceType)
	}

	return res
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

	episodes, err := client.GetSeriesEpisodesContext(ctx, &sonarr.GetEpisode{SeriesID: own.seriesID})
	if err != nil {
		slog.WarnContext(ctx, "Ownership unknown: failed to fetch Sonarr episodes", "instance", instanceName, "error", err)
		return model.OwnershipUnknown, 0
	}

	if own.episodeFileID > 0 {
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

	// The series matched but no specific file record lined up with our path. The
	// series still owns the area; treat as Owned (the arr drives repair) rather
	// than risk deleting a file the arr still expects.
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

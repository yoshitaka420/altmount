package triage

import (
	"context"
	"strings"

	"github.com/javi11/altmount/internal/arrs/model"
	"github.com/javi11/altmount/internal/config"
	"github.com/javi11/altmount/internal/database"
	"github.com/javi11/altmount/internal/metadata"
	"github.com/javi11/altmount/internal/utils"
)

// ArrsOwnership is the read-only ownership API triage depends on. *arrs.Service
// satisfies it; declaring it here keeps the triage package decoupled from the
// arrs package.
type ArrsOwnership interface {
	ResolveOwnership(ctx context.Context, filePath, relativePath string, metadata *model.WebhookMetadata) model.Ownership
}

// healthStore adapts *database.HealthRepository to HealthStore.
type healthStore struct {
	repo *database.HealthRepository
}

// NewHealthStore wraps the health repository for triage.
func NewHealthStore(repo *database.HealthRepository) HealthStore {
	return &healthStore{repo: repo}
}

func (a *healthStore) ListCorrupted(ctx context.Context, limit int) ([]*database.FileHealth, error) {
	status := database.HealthStatusCorrupted
	return a.repo.ListHealthItems(ctx, &status, limit, 0, nil, "", "", "")
}

func (a *healthStore) DeleteIfStatus(ctx context.Context, filePath string, expected database.HealthStatus) (bool, error) {
	return a.repo.DeleteHealthRecordIfStatus(ctx, filePath, expected)
}

// metaStore adapts the metadata service to MetaStore. It only ever touches the
// .meta record (never the library file, never the source NZB).
type metaStore struct {
	ms  *metadata.MetadataService
	cfg config.ConfigGetter
}

// NewMetaStore wraps the metadata service for triage.
func NewMetaStore(ms *metadata.MetadataService, cfg config.ConfigGetter) MetaStore {
	return &metaStore{ms: ms, cfg: cfg}
}

func (a *metaStore) Exists(ctx context.Context, item *database.FileHealth) (bool, error) {
	meta, err := a.ms.ReadFileMetadata(item.FilePath)
	if err != nil {
		// The .meta is present but unreadable (physical corruption). That is a
		// dead file, not a removed one, so report it as existing and let the
		// ownership gate decide.
		return true, nil
	}
	if meta != nil {
		return true, nil
	}

	// The original .meta is gone. Distinguish "an arr removed the file" from
	// "AltMount moved the .meta to corrupted_metadata when it condemned the file":
	// if a corrupted_metadata copy exists, the file is dead-and-hidden (not
	// removed), so report it as existing so Evaluate runs the ownership gate
	// (fail-closed KEEP when the arr is unreachable or the lookup errors) instead
	// of treating it as a file_removed zombie and deleting unconditionally.
	// rel mirrors metaStore.Delete and the MoveToCorrupted callers (MountPath stripped).
	cfg := a.cfg()
	rel := strings.TrimPrefix(item.FilePath, cfg.MountPath)
	rel = strings.TrimPrefix(rel, "/")
	if a.ms.CorruptedMetadataExists(rel) {
		return true, nil
	}
	return false, nil
}

func (a *metaStore) Delete(ctx context.Context, item *database.FileHealth) error {
	cfg := a.cfg()
	rel := strings.TrimPrefix(item.FilePath, cfg.MountPath)
	rel = strings.TrimPrefix(rel, "/")
	// deleteSourceNzb is always false: triage removes the .meta only.
	return a.ms.DeleteFileMetadataWithSourceNzb(ctx, rel, false)
}

// ownershipResolver adapts an ArrsOwnership service to OwnershipResolver.
type ownershipResolver struct {
	arrs ArrsOwnership
	cfg  config.ConfigGetter
}

// NewOwnershipResolver wraps the arrs ownership service for triage.
func NewOwnershipResolver(arrs ArrsOwnership, cfg config.ConfigGetter) OwnershipResolver {
	return &ownershipResolver{arrs: arrs, cfg: cfg}
}

func (a *ownershipResolver) ResolveForItem(ctx context.Context, item *database.FileHealth, metadata *model.WebhookMetadata) model.Ownership {
	pathForRescan := resolvePathForRescan(a.cfg(), item)
	return a.arrs.ResolveOwnership(ctx, pathForRescan, item.FilePath, metadata)
}

// resolvePathForRescan mirrors the health worker's path resolution so ownership
// is resolved against the same absolute path the repair flow uses.
func resolvePathForRescan(cfg *config.Config, item *database.FileHealth) string {
	if item.LibraryPath != nil && *item.LibraryPath != "" {
		return *item.LibraryPath
	}
	if cfg.Health.LibraryDir != nil && *cfg.Health.LibraryDir != "" {
		return utils.JoinAbsPath(*cfg.Health.LibraryDir, item.FilePath)
	}
	if cfg.Import.ImportDir != nil && *cfg.Import.ImportDir != "" {
		return utils.JoinAbsPath(*cfg.Import.ImportDir, item.FilePath)
	}
	return utils.JoinAbsPath(cfg.MountPath, item.FilePath)
}

package health

import (
	"context"
	"strings"
	"testing"

	"github.com/javi11/altmount/internal/arrs"
	"github.com/javi11/altmount/internal/database"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// insertCorrupted inserts a corrupted file_health row and returns nothing.
func insertCorrupted(t *testing.T, env *repairTestEnv, filePath, libraryPath string) {
	t.Helper()
	_, err := env.db.Exec(`
		INSERT INTO file_health
			(file_path, library_path, status, retry_count, max_retries,
			 repair_retry_count, max_repair_retries, last_checked)
		VALUES (?, ?, 'corrupted', 0, 3, 0, 3, datetime('now'))
	`, filePath, libraryPath)
	require.NoError(t, err)
}

func statusOf(t *testing.T, env *repairTestEnv, filePath string) (database.HealthStatus, bool) {
	t.Helper()
	fh, err := env.healthRepo.GetFileHealth(context.Background(), filePath)
	require.NoError(t, err)
	if fh == nil {
		return "", false
	}
	return fh.Status, true
}

// TestTriage_Classification covers the four ownership verdicts plus the
// file_removed zombie case in a single guarded run.
func TestTriage_Classification(t *testing.T) {
	tempDir := t.TempDir()
	env := newRepairTestEnv(t, tempDir, nil)

	enabled := true
	env.cfg.Health.CorruptedTriage.Enabled = &enabled

	// Ownership keyed on a marker embedded in the (library) path passed to the resolver.
	env.mockARRs.ownership = func(filePath string) arrs.OwnershipResult {
		switch {
		case strings.Contains(filePath, "unowned"):
			return arrs.OwnershipResult{LookupOK: true, Managed: false}
		case strings.Contains(filePath, "replaced"):
			return arrs.OwnershipResult{LookupOK: true, Managed: true, HasReplacement: true, InstanceName: "radarr-1"}
		case strings.Contains(filePath, "onlycopy"):
			return arrs.OwnershipResult{LookupOK: true, Managed: true, HasReplacement: false, InstanceName: "radarr-1"}
		case strings.Contains(filePath, "failclosed"):
			return arrs.OwnershipResult{LookupOK: false}
		default:
			return arrs.OwnershipResult{LookupOK: false}
		}
	}

	// Records: all corrupted. Non-zombie ones get a .meta so classification
	// reaches the ownership check; the zombie one deliberately has no .meta.
	withMeta := []string{
		"complete/unowned.mkv",
		"complete/replaced.mkv",
		"complete/onlycopy.mkv",
		"complete/failclosed.mkv",
	}
	for _, p := range withMeta {
		require.NoError(t, env.metadataService.WriteFileMetadata(p, validSegmentMeta(env.metadataService, 1024)))
		insertCorrupted(t, env, p, "/lib/"+strings.TrimPrefix(p, "complete/"))
	}
	// Zombie: corrupted row, no .meta on disk.
	insertCorrupted(t, env, "complete/zombie.mkv", "/lib/zombie.mkv")

	stats := env.hw.runCorruptedTriage(context.Background())

	// Deleted: unowned, replaced, zombie. Kept: onlycopy (owned), failclosed.
	assert.Equal(t, 3, stats.deleted, "should delete unowned + replaced + zombie")
	assert.Equal(t, 1, stats.unowned)
	assert.Equal(t, 1, stats.replaced)
	assert.Equal(t, 1, stats.zombies)
	assert.Equal(t, 1, stats.skippedOwned, "owned-only-copy must be kept")
	assert.Equal(t, 1, stats.skippedFailClosed, "fail-closed must be kept")

	// Verify DB outcomes.
	for _, p := range []string{"complete/unowned.mkv", "complete/replaced.mkv", "complete/zombie.mkv"} {
		_, exists := statusOf(t, env, p)
		assert.False(t, exists, "%s should have been soft-deleted", p)
	}
	for _, p := range []string{"complete/onlycopy.mkv", "complete/failclosed.mkv"} {
		st, exists := statusOf(t, env, p)
		require.True(t, exists, "%s must be kept", p)
		assert.Equal(t, database.HealthStatusCorrupted, st)
	}

	// The .meta of deleted-non-zombie records must be gone; kept records keep theirs.
	for _, p := range []string{"complete/unowned.mkv", "complete/replaced.mkv"} {
		meta, err := env.metadataService.ReadFileMetadata(p)
		require.NoError(t, err)
		assert.Nil(t, meta, "%s .meta should be deleted", p)
	}
	for _, p := range []string{"complete/onlycopy.mkv", "complete/failclosed.mkv"} {
		meta, err := env.metadataService.ReadFileMetadata(p)
		require.NoError(t, err)
		assert.NotNil(t, meta, "%s .meta must be preserved", p)
	}
}

// TestTriage_MassEventAbort verifies the run aborts (deletes nothing) when the
// corrupted population exceeds the mass-event threshold.
func TestTriage_MassEventAbort(t *testing.T) {
	tempDir := t.TempDir()
	env := newRepairTestEnv(t, tempDir, nil)

	enabled := true
	env.cfg.Health.CorruptedTriage.Enabled = &enabled
	env.cfg.Health.CorruptedTriage.MassEventThreshold = 3 // tiny threshold

	// Everything is provably unowned (would be deleted if not for the guard).
	env.mockARRs.ownership = func(string) arrs.OwnershipResult {
		return arrs.OwnershipResult{LookupOK: true, Managed: false}
	}

	for _, p := range []string{"complete/a.mkv", "complete/b.mkv", "complete/c.mkv", "complete/d.mkv"} {
		require.NoError(t, env.metadataService.WriteFileMetadata(p, validSegmentMeta(env.metadataService, 1024)))
		insertCorrupted(t, env, p, "/lib/"+strings.TrimPrefix(p, "complete/"))
	}

	stats := env.hw.runCorruptedTriage(context.Background())

	assert.True(t, stats.massEventAborted, "run must abort on mass event")
	assert.Equal(t, 0, stats.deleted, "no deletes when aborted")

	// All records still present.
	for _, p := range []string{"complete/a.mkv", "complete/b.mkv", "complete/c.mkv", "complete/d.mkv"} {
		_, exists := statusOf(t, env, p)
		assert.True(t, exists, "%s must survive a mass-event abort", p)
	}
}

// TestTriage_PerRunCap verifies the per-run delete cap bounds deletions.
func TestTriage_PerRunCap(t *testing.T) {
	tempDir := t.TempDir()
	env := newRepairTestEnv(t, tempDir, nil)

	enabled := true
	env.cfg.Health.CorruptedTriage.Enabled = &enabled
	env.cfg.Health.CorruptedTriage.MaxDeletesPerRun = 2
	env.cfg.Health.CorruptedTriage.MassEventThreshold = 100

	env.mockARRs.ownership = func(string) arrs.OwnershipResult {
		return arrs.OwnershipResult{LookupOK: true, Managed: false}
	}

	for _, p := range []string{"complete/1.mkv", "complete/2.mkv", "complete/3.mkv", "complete/4.mkv", "complete/5.mkv"} {
		require.NoError(t, env.metadataService.WriteFileMetadata(p, validSegmentMeta(env.metadataService, 1024)))
		insertCorrupted(t, env, p, "/lib/"+strings.TrimPrefix(p, "complete/"))
	}

	stats := env.hw.runCorruptedTriage(context.Background())
	assert.Equal(t, 2, stats.deleted, "must stop at the per-run cap")
}

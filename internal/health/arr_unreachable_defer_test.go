package health

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"testing"

	"github.com/javi11/altmount/internal/arrs"
	"github.com/javi11/altmount/internal/database"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// arrUnreachableErr mimics what arrs.Service.TriggerFileRescan returns when the
// arr is down: the underlying transport error tagged with arrs.ErrArrUnreachable.
func arrUnreachableErr() error {
	return fmt.Errorf("%w: failed to get episodes for series Foo: dial tcp: lookup sonarr: no such host", arrs.ErrArrUnreachable)
}

// TestApplyRepairOutcome_ArrUnreachableDefers is the pure-unit guard on the
// decision: unreachable defers (RepairTrigger, repair_triggered, no budget burn);
// a generic failure still condemns.
func TestApplyRepairOutcome_ArrUnreachableDefers(t *testing.T) {
	deferred := &database.HealthStatusUpdate{}
	applyRepairOutcome(deferred, repairOutcomeArrUnreachable, arrUnreachableErr())
	assert.Equal(t, database.UpdateTypeRepairTrigger, deferred.Type,
		"defer must use RepairTrigger (does NOT increment repair_retry_count)")
	assert.Equal(t, database.HealthStatusRepairTriggered, deferred.Status,
		"defer must keep repair_triggered, never corrupted")
	require.NotNil(t, deferred.ErrorMessage)
	assert.Contains(t, *deferred.ErrorMessage, "deferred")

	condemned := &database.HealthStatusUpdate{}
	applyRepairOutcome(condemned, repairOutcomeCorrupted, errors.New("sonarr 500 internal server error"))
	assert.Equal(t, database.HealthStatusCorrupted, condemned.Status,
		"a genuine repair failure must still condemn")
}

// TestE2E_ARRUnreachable_DefersNotCorrupted: arr unreachable during repair → file
// DEFERRED (repair_triggered), NOT corrupted, and the repair budget is untouched.
func TestE2E_ARRUnreachable_DefersNotCorrupted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks not supported on Windows")
	}
	tempDir := t.TempDir()
	env := newRepairTestEnv(t, tempDir, arrUnreachableErr())
	ctx := context.Background()

	filePath := "series/show.s01e01.mkv"
	libraryPath := "/media/library/show.s01e01.mkv"
	require.NoError(t, env.metadataService.WriteFileMetadata(filePath, validSegmentMeta(env.metadataService, 1024)))
	insertFileHealth(t, env.db, filePath, libraryPath, 2, 3) // at last retry → repair stage

	require.NoError(t, env.hw.runHealthCheckCycle(ctx))

	h, err := env.healthRepo.GetFileHealth(ctx, filePath)
	require.NoError(t, err)
	require.NotNil(t, h)
	assert.Equal(t, database.HealthStatusRepairTriggered, h.Status,
		"arr unreachable must DEFER, not condemn")
	assert.Equal(t, 0, h.RepairRetryCount, "defer must NOT burn the repair budget")
	require.NotNil(t, h.LastError)
	assert.Contains(t, *h.LastError, "arr unreachable")

	env.mockARRs.mu.Lock()
	calls := len(env.mockARRs.calls)
	env.mockARRs.mu.Unlock()
	assert.Equal(t, 1, calls, "repair was attempted (arr was tried, just unreachable)")
}

// TestE2E_ARRGenuineFailure_StillCorrupted: arr reachable but the repair genuinely
// fails (non-transport error) → STILL condemned, exactly as before.
func TestE2E_ARRGenuineFailure_StillCorrupted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks not supported on Windows")
	}
	tempDir := t.TempDir()
	env := newRepairTestEnv(t, tempDir, errors.New("sonarr returned 500: internal server error"))
	ctx := context.Background()

	filePath := "series/show.s02e02.mkv"
	libraryPath := "/media/library/show.s02e02.mkv"
	require.NoError(t, env.metadataService.WriteFileMetadata(filePath, validSegmentMeta(env.metadataService, 1024)))
	insertFileHealth(t, env.db, filePath, libraryPath, 2, 3)

	require.NoError(t, env.hw.runHealthCheckCycle(ctx))

	h, err := env.healthRepo.GetFileHealth(ctx, filePath)
	require.NoError(t, err)
	require.NotNil(t, h)
	assert.Equal(t, database.HealthStatusCorrupted, h.Status,
		"a genuine (reachable) repair failure must still condemn")
}

// TestE2E_HealthCheckRunsDuringARROutage: detection is NNTP-only and must keep
// running while the arr is down — a fresh file still gets checked (retry advances),
// and the arr is not even consulted at the detection stage.
func TestE2E_HealthCheckRunsDuringARROutage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks not supported on Windows")
	}
	tempDir := t.TempDir()
	env := newRepairTestEnv(t, tempDir, arrUnreachableErr())
	ctx := context.Background()

	filePath := "series/fresh.s01e01.mkv"
	libraryPath := "/media/library/fresh.s01e01.mkv"
	require.NoError(t, env.metadataService.WriteFileMetadata(filePath, validSegmentMeta(env.metadataService, 1024)))
	insertFileHealth(t, env.db, filePath, libraryPath, 0, 3) // fresh: retry_count = 0

	require.NoError(t, env.hw.runHealthCheckCycle(ctx))

	h, err := env.healthRepo.GetFileHealth(ctx, filePath)
	require.NoError(t, err)
	require.NotNil(t, h)
	assert.Equal(t, database.HealthStatusPending, h.Status,
		"check must keep running during arr outage (stays pending, scheduled to retry)")
	assert.Equal(t, 1, h.RetryCount, "health check retry advanced — detection NOT disabled by the outage")

	env.mockARRs.mu.Lock()
	calls := len(env.mockARRs.calls)
	env.mockARRs.mu.Unlock()
	assert.Equal(t, 0, calls, "arr is not consulted at the detection stage")
}

// TestE2E_ARRUnreachable_SelfHealsWhenARRReturns is the self-heal proof: a file
// deferred while the arr is down is picked up again automatically once the arr
// returns, the repair resumes (budget advances), and it never became corrupted.
func TestE2E_ARRUnreachable_SelfHealsWhenARRReturns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks not supported on Windows")
	}
	tempDir := t.TempDir()
	env := newRepairTestEnv(t, tempDir, arrUnreachableErr()) // arr starts DOWN
	ctx := context.Background()

	filePath := "series/heal.s01e01.mkv"
	libraryPath := "/media/library/heal.s01e01.mkv"
	require.NoError(t, env.metadataService.WriteFileMetadata(filePath, validSegmentMeta(env.metadataService, 1024)))
	insertFileHealth(t, env.db, filePath, libraryPath, 2, 3)

	// Cycle 1 — arr down: file is deferred.
	require.NoError(t, env.hw.runHealthCheckCycle(ctx))
	h, err := env.healthRepo.GetFileHealth(ctx, filePath)
	require.NoError(t, err)
	require.Equal(t, database.HealthStatusRepairTriggered, h.Status, "should be deferred while arr down")
	require.Equal(t, 0, h.RepairRetryCount, "no budget burned while deferred")

	// Arr comes back online. No manual action on the file.
	env.mockARRs.mu.Lock()
	env.mockARRs.returnErr = nil
	env.mockARRs.mu.Unlock()

	// The retry engine re-selects the repair_triggered file next cycle.
	advanceScheduledCheck(t, env.db, filePath)
	require.NoError(t, env.hw.runHealthCheckCycle(ctx))

	h, err = env.healthRepo.GetFileHealth(ctx, filePath)
	require.NoError(t, err)
	require.NotNil(t, h)
	assert.NotEqual(t, database.HealthStatusCorrupted, h.Status,
		"file must NEVER have become corrupted across the outage")
	assert.Equal(t, database.HealthStatusRepairTriggered, h.Status,
		"repair resumed under the normal engine")
	assert.Equal(t, 1, h.RepairRetryCount,
		"once the arr returned, the repair advanced normally (budget now increments) — self-heal")

	env.mockARRs.mu.Lock()
	calls := len(env.mockARRs.calls)
	env.mockARRs.mu.Unlock()
	assert.Equal(t, 2, calls, "arr retried automatically: 1 deferred attempt + 1 successful re-trigger")
}

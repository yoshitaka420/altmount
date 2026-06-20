package health

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/javi11/altmount/internal/arrs"
	"github.com/javi11/altmount/internal/database"
)

// triageReason classifies why a corrupted record was soft-deleted by the triage.
type triageReason string

const (
	// triageReasonZombie: the .meta backing the virtual file is already gone, so
	// the health record points at nothing (file_removed zombie).
	triageReasonZombie triageReason = "file_removed_zombie"
	// triageReasonUnowned: no configured arr manages this path (proven, error-free).
	triageReasonUnowned triageReason = "dead_unowned"
	// triageReasonReplaced: the managing arr already holds a different healthy file.
	triageReasonReplaced triageReason = "dead_arr_has_replacement"
)

// triageStats is the per-run funnel outcome of a corrupted-file triage pass.
type triageStats struct {
	corruptedTotal      int
	scanned             int
	deleted             int
	zombies             int
	unowned             int
	replaced            int
	skippedOwned        int
	skippedFailClosed   int
	skippedNotCorrupted int
	errors              int
	massEventAborted    bool
}

// countNewlyCorrupted counts how many bulk updates transitioned a file into the
// corrupted status (used to decide whether to kick an event-driven triage pass).
func countNewlyCorrupted(results []database.HealthStatusUpdate) int {
	n := 0
	for _, r := range results {
		if !r.Skip && r.Status == database.HealthStatusCorrupted {
			n++
		}
	}
	return n
}

// TriggerTriage requests an asynchronous corrupted-file triage pass. Used by the
// arr webhook handoff: an import/upgrade webhook means a replacement may have
// landed, so some corrupted records may now be provably safe to delete. It is
// non-blocking, runs on a detached context (so it survives the HTTP request),
// and coalesces with any in-flight pass.
func (hw *HealthWorker) TriggerTriage() {
	if !hw.configGetter().Health.CorruptedTriage.IsEnabled() {
		return
	}
	go func() {
		_, _ = hw.runCorruptedTriageSafe(context.Background())
	}()
}

// triageBackstopLoop runs the adaptive backstop sweep. Webhook/enter-corrupted
// events drive most triage work; this sweep is the safety net that catches
// anything the events missed. It backs off when idle and tightens when active.
func (hw *HealthWorker) triageBackstopLoop(ctx context.Context) {
	base := hw.configGetter().Health.CorruptedTriage.GetBackstopInterval()
	interval := base
	timer := time.NewTimer(interval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-hw.stopChan:
			return
		case <-timer.C:
			stats, ran := hw.runCorruptedTriageSafe(ctx)

			base = hw.configGetter().Health.CorruptedTriage.GetBackstopInterval()
			switch {
			case !ran:
				// Disabled or coalesced: back off toward the cap.
				interval = minDuration(interval*2, base*8)
			case stats.scanned > 0 || stats.massEventAborted:
				// Active population: stay at the base cadence.
				interval = base
			default:
				// Enabled but nothing to do: back off toward the cap.
				interval = minDuration(interval*2, base*8)
			}
			if interval < base {
				interval = base
			}
			timer.Reset(interval)
		}
	}
}

// runCorruptedTriageSafe runs one triage pass under the triageRunning guard with
// panic isolation. Concurrent triggers (backstop, enter-corrupted, webhook) are
// coalesced: if a pass is already running, this returns ran=false immediately.
func (hw *HealthWorker) runCorruptedTriageSafe(ctx context.Context) (stats triageStats, ran bool) {
	if !hw.configGetter().Health.CorruptedTriage.IsEnabled() {
		return triageStats{}, false
	}

	hw.mu.Lock()
	if hw.triageRunning {
		hw.mu.Unlock()
		return triageStats{}, false
	}
	hw.triageRunning = true
	hw.mu.Unlock()

	ran = true
	defer func() {
		hw.mu.Lock()
		hw.triageRunning = false
		hw.mu.Unlock()
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "Panic in corrupted-file triage (recovered)", "panic", r)
		}
	}()

	stats = hw.runCorruptedTriage(ctx)
	return stats, ran
}

// runCorruptedTriage performs one bounded, guarded pass: it enumerates corrupted
// records and soft-deletes (DB row + .meta only) the ones that are provably safe.
// It NEVER touches the library file under the mount and NEVER deletes an arr's
// only copy. All uncertainty fails closed (keeps the record).
func (hw *HealthWorker) runCorruptedTriage(ctx context.Context) triageStats {
	tcfg := hw.configGetter().Health.CorruptedTriage
	maxDeletes := tcfg.GetMaxDeletesPerRun()
	massThreshold := tcfg.GetMassEventThreshold()

	var stats triageStats

	// Mass-event guard: an abnormally large corrupted population usually means a
	// systemic failure (provider outage, mount flap) marked everything corrupted.
	// Abort rather than mass-delete.
	statsByStatus, err := hw.healthRepo.GetHealthStats(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "Triage: failed to read health stats, aborting (fail closed)", "error", err)
		return stats
	}
	stats.corruptedTotal = statsByStatus[database.HealthStatusCorrupted]
	if stats.corruptedTotal == 0 {
		return stats
	}
	if stats.corruptedTotal > massThreshold {
		stats.massEventAborted = true
		slog.WarnContext(ctx, "Triage: corrupted count exceeds mass-event threshold, aborting run (fail closed)",
			"corrupted_total", stats.corruptedTotal,
			"mass_event_threshold", massThreshold)
		return stats
	}

	// Scan a bounded window of candidates (more than the delete cap so a run that
	// hits the cap still makes forward progress next time).
	scanLimit := maxDeletes * 4
	if scanLimit < 100 {
		scanLimit = 100
	}
	candidates, err := hw.healthRepo.GetFilesByStatus(ctx, database.HealthStatusCorrupted, scanLimit)
	if err != nil {
		slog.ErrorContext(ctx, "Triage: failed to list corrupted candidates, aborting", "error", err)
		return stats
	}

	for _, fh := range candidates {
		if stats.deleted >= maxDeletes {
			slog.InfoContext(ctx, "Triage: per-run delete cap reached, stopping",
				"max_deletes_per_run", maxDeletes)
			break
		}
		select {
		case <-ctx.Done():
			return stats
		default:
		}
		stats.scanned++
		hw.triageOne(ctx, fh, &stats)
	}

	slog.InfoContext(ctx, "Corrupted-file triage run complete",
		"corrupted_total", stats.corruptedTotal,
		"scanned", stats.scanned,
		"deleted", stats.deleted,
		"zombies", stats.zombies,
		"unowned", stats.unowned,
		"replaced", stats.replaced,
		"skipped_owned", stats.skippedOwned,
		"skipped_fail_closed", stats.skippedFailClosed,
		"skipped_not_corrupted", stats.skippedNotCorrupted,
		"errors", stats.errors)

	if stats.deleted > 0 {
		hw.broadcastHealthChanged()
	}
	return stats
}

// triageOne evaluates and (if provably safe) soft-deletes a single corrupted
// record. It is panic-isolated so one bad record can never abort the whole pass.
func (hw *HealthWorker) triageOne(ctx context.Context, fh *database.FileHealth, stats *triageStats) {
	defer func() {
		if r := recover(); r != nil {
			stats.errors++
			slog.ErrorContext(ctx, "Triage: panic evaluating record (recovered, skipped)",
				"file_path", fh.FilePath, "panic", r)
		}
	}()

	reason, deletable, own := hw.triageClassify(ctx, fh, stats)
	if !deletable {
		return
	}

	// Status-guarded atomic delete: only removes the row if it is STILL
	// corrupted, so a record concurrently rescued (e.g. re-imported back to
	// pending) is never clobbered. If nothing was deleted we also leave the
	// .meta intact.
	deleted, err := hw.healthRepo.DeleteHealthRecordIfStatus(ctx, fh.FilePath, database.HealthStatusCorrupted)
	if err != nil {
		stats.errors++
		slog.ErrorContext(ctx, "Triage: status-guarded delete failed", "file_path", fh.FilePath, "error", err)
		return
	}
	if !deleted {
		stats.skippedNotCorrupted++
		return
	}

	// Row removed; now remove the .meta (and its .id sidecar) ONLY — never the
	// library file under the mount, never the source NZB.
	relativePath := hw.metadataRelativePath(fh.FilePath)
	if err := hw.metadataService.DeleteFileMetadata(relativePath); err != nil {
		slog.ErrorContext(ctx, "Triage: failed to delete metadata after row delete",
			"file_path", fh.FilePath, "error", err)
	}

	stats.deleted++
	switch reason {
	case triageReasonZombie:
		stats.zombies++
	case triageReasonUnowned:
		stats.unowned++
	case triageReasonReplaced:
		stats.replaced++
	}

	// Per-action audit log: enough to reconstruct every deletion decision.
	slog.InfoContext(ctx, "Triage: soft-deleted corrupted record",
		"file_path", fh.FilePath,
		"reason", string(reason),
		"instance", own.InstanceName,
		"owned", own.Managed,
		"replacement_id", own.ReplacementFileID)
}

// triageClassify decides, read-only and fail-closed, whether a corrupted record
// is provably safe to soft-delete. It returns (reason, true) only for the safe
// cases; every uncertain case returns (_, false) and is counted as skipped.
func (hw *HealthWorker) triageClassify(ctx context.Context, fh *database.FileHealth, stats *triageStats) (triageReason, bool, arrs.OwnershipResult) {
	// 1. file_removed zombie: the .meta backing the virtual file is gone, so the
	//    record points at nothing. (An unreadable-but-present .meta is real
	//    corruption, not a zombie — that returns err != nil and falls through.)
	meta, err := hw.metadataService.ReadFileMetadata(hw.metadataRelativePath(fh.FilePath))
	if err == nil && meta == nil {
		return triageReasonZombie, true, arrs.OwnershipResult{}
	}

	// 2. Ownership — read-only and fail-closed, sharing the repair resolvers.
	//    Feed the resolver the SAME inputs repair uses.
	pathForRescan := hw.resolvePathForRescan(fh)
	own := hw.arrsService.ResolveOwnership(ctx, pathForRescan, fh.FilePath, fh.Metadata)
	if !own.LookupOK {
		// Could not determine ownership (lookup error/timeout, or an arr type we
		// cannot introspect). Fail closed: do NOT delete.
		stats.skippedFailClosed++
		return "", false, own
	}
	if !own.Managed {
		// Proven unowned: no configured arr manages this path.
		return triageReasonUnowned, true, own
	}
	if own.HasReplacement {
		// The owning arr already has a different healthy file.
		return triageReasonReplaced, true, own
	}
	// Managed with no replacement: this could be the arr's only copy. Keep it.
	stats.skippedOwned++
	return "", false, own
}

// metadataRelativePath converts a health record file path to the mount-relative
// virtual path that the metadata service (ReadFileMetadata / DeleteFileMetadata)
// expects, so both operate on the same key.
func (hw *HealthWorker) metadataRelativePath(filePath string) string {
	rel := strings.TrimPrefix(filePath, hw.configGetter().MountPath)
	return strings.TrimPrefix(rel, "/")
}

// minDuration returns the smaller of two durations.
func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

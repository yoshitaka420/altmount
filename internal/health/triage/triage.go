// Package triage implements the corrupted-file auto-delete triage.
//
// When enabled, triage soft-deletes only the AltMount bookkeeping for corrupted
// files that no *arr will ever repair: the health record row and the .meta
// record. It NEVER deletes the library file in the media directory and NEVER
// deletes an *arr's only copy — those invariants are guaranteed structurally,
// because triage only acts on files that are either gone (file_removed zombies),
// owned by no arr (dead + unowned), or already replaced by a different healthy
// file in the arr (dead + replacement).
//
// Ownership is resolved read-only and fail-closed (see arrs ResolveOwnership):
// any lookup error, unreachable instance, or non-introspectable arr leaves the
// file untouched.
package triage

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/javi11/altmount/internal/arrs/model"
	"github.com/javi11/altmount/internal/config"
	"github.com/javi11/altmount/internal/database"
)

// Source identifies what triggered a triage evaluation, for observability.
type Source string

const (
	SourceEnterCorrupted Source = "enter_corrupted" // a file just entered the corrupted state
	SourceWebhook        Source = "webhook"         // an arr webhook handed a file off to triage
	SourceBackstop       Source = "backstop"        // the periodic adaptive backstop sweep
)

// Reason is the outcome classification for a single evaluated record.
type Reason string

const (
	ReasonFileRemoved  Reason = "file_removed"      // .meta is gone: zombie record
	ReasonDeadUnowned  Reason = "dead_unowned"      // corrupted and no arr owns it
	ReasonDeadReplaced Reason = "dead_replaced"     // corrupted and the arr already has a replacement
	ReasonOwned        Reason = "owned"             // kept: an arr will repair it
	ReasonUnknown      Reason = "ownership_unknown" // kept: ownership could not be determined (fail closed)
)

// Action is the decision for a record.
type Action int

const (
	ActionKeep Action = iota
	ActionDelete
)

// Decision is the read-only evaluation of a single record.
type Decision struct {
	Action    Action
	Reason    Reason
	Ownership model.Ownership
}

// Stats is the per-run observability funnel.
type Stats struct {
	Source         Source
	Considered     int
	Deleted        int
	SkippedOwned   int
	SkippedUnknown int
	Errors         int
	Capped         bool
	Aborted        bool
	ByReason       map[Reason]int
}

// HealthStore is the subset of the health repository triage needs.
type HealthStore interface {
	// ListCorrupted returns up to limit records currently in the corrupted state.
	ListCorrupted(ctx context.Context, limit int) ([]*database.FileHealth, error)
	// DeleteIfStatus atomically removes the record only if it is still in the
	// expected status, returning whether a row was deleted.
	DeleteIfStatus(ctx context.Context, filePath string, expected database.HealthStatus) (bool, error)
}

// MetaStore abstracts the .meta side effects. Implementations MUST only ever
// touch the .meta record — never the library file and never the source NZB.
type MetaStore interface {
	// Exists reports whether the .meta record still exists for the item.
	Exists(ctx context.Context, item *database.FileHealth) (bool, error)
	// Delete removes the .meta record for the item.
	Delete(ctx context.Context, item *database.FileHealth) error
}

// OwnershipResolver resolves read-only, fail-closed ownership for a record.
type OwnershipResolver interface {
	ResolveForItem(ctx context.Context, item *database.FileHealth, metadata *model.WebhookMetadata) model.Ownership
}

// Service runs the triage logic. It is safe to call even when triage is disabled
// in config — every entry point is a no-op until enabled.
type Service struct {
	cfg      config.ConfigGetter
	store    HealthStore
	meta     MetaStore
	resolver OwnershipResolver
}

// NewService builds a triage service.
func NewService(cfg config.ConfigGetter, store HealthStore, meta MetaStore, resolver OwnershipResolver) *Service {
	return &Service{cfg: cfg, store: store, meta: meta, resolver: resolver}
}

// Enabled reports whether triage is currently turned on.
func (s *Service) Enabled() bool {
	return s.cfg().GetCorruptedTriageEnabled()
}

// Evaluate makes the read-only delete/keep decision for a single record. It
// performs no writes and is safe to call regardless of the enabled flag.
func (s *Service) Evaluate(ctx context.Context, item *database.FileHealth) Decision {
	// A missing .meta means the underlying file is gone: a file_removed zombie.
	// Ownership is irrelevant — there is nothing left to protect.
	exists, err := s.meta.Exists(ctx, item)
	if err != nil {
		// We could not determine whether the file is gone: fail closed and keep.
		return Decision{Action: ActionKeep, Reason: ReasonUnknown}
	}
	if !exists {
		return Decision{Action: ActionDelete, Reason: ReasonFileRemoved}
	}

	// The file is still present but corrupted ("dead"). Resolve ownership.
	own := s.resolver.ResolveForItem(ctx, item, parseMetadata(item))
	switch own.Status {
	case model.OwnershipUnowned:
		return Decision{Action: ActionDelete, Reason: ReasonDeadUnowned, Ownership: own}
	case model.OwnershipReplaced:
		return Decision{Action: ActionDelete, Reason: ReasonDeadReplaced, Ownership: own}
	case model.OwnershipOwned:
		return Decision{Action: ActionKeep, Reason: ReasonOwned, Ownership: own}
	default:
		return Decision{Action: ActionKeep, Reason: ReasonUnknown, Ownership: own}
	}
}

// ProcessItem evaluates and (if warranted and enabled) soft-deletes a single
// record. It is the entry point used by the event-driven triggers
// (enter-corrupted kick, webhook handoff). Returns whether a delete happened.
func (s *Service) ProcessItem(ctx context.Context, item *database.FileHealth, source Source) bool {
	if !s.Enabled() || item == nil {
		return false
	}
	deleted := false
	s.guard(ctx, item, source, func() {
		deleted = s.processOne(ctx, item, source)
	})
	return deleted
}

// Run evaluates a batch with all safety guards: a mass-event abort, a per-run
// delete cap, and per-item panic isolation. It returns the observability funnel.
func (s *Service) Run(ctx context.Context, items []*database.FileHealth, source Source) Stats {
	st := Stats{Source: source, ByReason: map[Reason]int{}}
	if !s.Enabled() {
		return st
	}
	st.Considered = len(items)

	// Mass-event abort: a sudden flood of corrupted records (e.g. a provider
	// outage marking everything dead) must not trigger mass deletion.
	threshold := s.cfg().GetCorruptedTriageMassEventThreshold()
	if len(items) > threshold {
		st.Aborted = true
		slog.WarnContext(ctx, "Corrupted triage run aborted: candidate count exceeds mass-event threshold",
			"source", source, "candidates", len(items), "threshold", threshold)
		s.logSummary(ctx, st)
		return st
	}

	maxDeletes := s.cfg().GetCorruptedTriageMaxDeletesPerRun()
	for _, item := range items {
		if st.Deleted >= maxDeletes {
			st.Capped = true
			slog.InfoContext(ctx, "Corrupted triage hit per-run delete cap", "source", source, "cap", maxDeletes)
			break
		}

		// Capture per-iteration so a recover can still record the outcome.
		item := item
		func() {
			// Panic isolation: one bad record must not abort the whole run.
			defer func() {
				if r := recover(); r != nil {
					st.Errors++
					slog.ErrorContext(ctx, "Corrupted triage recovered from panic",
						"source", source, "file_path", item.FilePath, "panic", r)
				}
			}()

			dec := s.Evaluate(ctx, item)
			if dec.Action == ActionKeep {
				switch dec.Reason {
				case ReasonOwned:
					st.SkippedOwned++
				default:
					st.SkippedUnknown++
				}
				return
			}
			deleted, err := s.applyDelete(ctx, item, dec, source)
			switch {
			case err != nil:
				st.Errors++
			case deleted:
				st.Deleted++
				st.ByReason[dec.Reason]++
			}
		}()
	}

	s.logSummary(ctx, st)
	return st
}

// Sweep is the adaptive backstop: it fetches the current corrupted records and
// runs them through Run. The number fetched is bounded so a single sweep stays
// cheap; the mass-event threshold still applies.
func (s *Service) Sweep(ctx context.Context) (Stats, error) {
	if !s.Enabled() {
		return Stats{Source: SourceBackstop, ByReason: map[Reason]int{}}, nil
	}
	// Fetch a little past the mass-event threshold so the abort guard can see a
	// flood rather than silently truncating it.
	limit := s.cfg().GetCorruptedTriageMassEventThreshold() + 1
	items, err := s.store.ListCorrupted(ctx, limit)
	if err != nil {
		return Stats{Source: SourceBackstop, ByReason: map[Reason]int{}}, err
	}
	return s.Run(ctx, items, SourceBackstop), nil
}

// AdaptiveInterval computes the next backstop delay from the previous run. It
// speeds up when there was work to do (or the cap was hit) and backs off when
// the queue was clean or a mass event was aborted, all relative to base.
func AdaptiveInterval(base time.Duration, st Stats) time.Duration {
	if base <= 0 {
		base = 6 * time.Hour
	}
	switch {
	case st.Aborted:
		// A mass event is in progress; back off hard and let it settle.
		return base * 4
	case st.Capped || st.Deleted > 0:
		// There was a backlog; come back sooner (but not hammering).
		d := base / 2
		if d < time.Minute {
			d = time.Minute
		}
		return d
	default:
		// Nothing to do; ease off up to 2x base.
		return base * 2
	}
}

// guard runs fn with panic isolation so one bad record cannot abort a run.
func (s *Service) guard(ctx context.Context, item *database.FileHealth, source Source, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "Corrupted triage recovered from panic",
				"source", source, "file_path", item.FilePath, "panic", r)
		}
	}()
	fn()
}

// processOne evaluates then applies a delete for a single item (no guards).
func (s *Service) processOne(ctx context.Context, item *database.FileHealth, source Source) bool {
	dec := s.Evaluate(ctx, item)
	if dec.Action == ActionKeep {
		slog.DebugContext(ctx, "Corrupted triage keeping file",
			"source", source, "file_path", item.FilePath, "reason", dec.Reason)
		return false
	}
	deleted, _ := s.applyDelete(ctx, item, dec, source)
	return deleted
}

// applyDelete performs the status-guarded soft delete: it removes the health row
// only if it is still corrupted, then removes the .meta record. It never touches
// the library file or the source NZB.
func (s *Service) applyDelete(ctx context.Context, item *database.FileHealth, dec Decision, source Source) (bool, error) {
	ok, err := s.store.DeleteIfStatus(ctx, item.FilePath, database.HealthStatusCorrupted)
	if err != nil {
		slog.ErrorContext(ctx, "Corrupted triage failed to delete health record",
			"source", source, "file_path", item.FilePath, "error", err)
		return false, err
	}
	if !ok {
		// The record changed status between evaluation and delete (e.g. it was
		// repaired). Leave the .meta alone.
		slog.InfoContext(ctx, "Corrupted triage skipped: record no longer corrupted (status-guarded)",
			"source", source, "file_path", item.FilePath)
		return false, nil
	}

	if err := s.meta.Delete(ctx, item); err != nil {
		// The row is already gone; surface the .meta failure but still count the
		// record as triaged.
		slog.ErrorContext(ctx, "Corrupted triage deleted health row but failed to delete .meta",
			"source", source, "file_path", item.FilePath, "error", err)
	}

	slog.InfoContext(ctx, "Corrupted triage soft-deleted file bookkeeping",
		"source", source,
		"file_path", item.FilePath,
		"reason", dec.Reason,
		"instance_type", dec.Ownership.InstanceType,
		"instance", dec.Ownership.InstanceName,
		"owned", dec.Ownership.Status.String(),
		"replacement_id", dec.Ownership.ReplacementID)
	return true, nil
}

// logSummary emits the per-run funnel counts.
func (s *Service) logSummary(ctx context.Context, st Stats) {
	slog.InfoContext(ctx, "Corrupted triage run complete",
		"source", st.Source,
		"considered", st.Considered,
		"deleted", st.Deleted,
		"file_removed", st.ByReason[ReasonFileRemoved],
		"dead_unowned", st.ByReason[ReasonDeadUnowned],
		"dead_replaced", st.ByReason[ReasonDeadReplaced],
		"skipped_owned", st.SkippedOwned,
		"skipped_unknown", st.SkippedUnknown,
		"errors", st.Errors,
		"capped", st.Capped,
		"aborted", st.Aborted)
}

// parseMetadata decodes the webhook metadata stored on the health record, if any.
func parseMetadata(item *database.FileHealth) *model.WebhookMetadata {
	if item == nil || item.Metadata == nil || *item.Metadata == "" {
		return nil
	}
	var meta model.WebhookMetadata
	if err := json.Unmarshal([]byte(*item.Metadata), &meta); err != nil {
		return nil
	}
	return &meta
}

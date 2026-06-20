package triage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/javi11/altmount/internal/arrs/model"
	"github.com/javi11/altmount/internal/config"
	"github.com/javi11/altmount/internal/database"
)

// --- fakes ---

type fakeStore struct {
	listResp  []*database.FileHealth
	listErr   error
	status    map[string]database.HealthStatus // overrides; default == corrupted
	deleted   []string
	deleteErr error
}

func (f *fakeStore) ListCorrupted(ctx context.Context, limit int) ([]*database.FileHealth, error) {
	return f.listResp, f.listErr
}

func (f *fakeStore) DeleteIfStatus(ctx context.Context, filePath string, expected database.HealthStatus) (bool, error) {
	if f.deleteErr != nil {
		return false, f.deleteErr
	}
	if f.status != nil {
		if st, ok := f.status[filePath]; ok && st != expected {
			return false, nil // status changed: guarded out
		}
	}
	f.deleted = append(f.deleted, filePath)
	return true, nil
}

type fakeMeta struct {
	exists    map[string]bool // default true
	existsErr error
	deleted   []string
	deleteErr error
}

func (f *fakeMeta) Exists(ctx context.Context, item *database.FileHealth) (bool, error) {
	if f.existsErr != nil {
		return false, f.existsErr
	}
	if f.exists != nil {
		if v, ok := f.exists[item.FilePath]; ok {
			return v, nil
		}
	}
	return true, nil
}

func (f *fakeMeta) Delete(ctx context.Context, item *database.FileHealth) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, item.FilePath)
	return nil
}

type fakeResolver struct {
	byPath  map[string]model.Ownership
	panicOn string
}

func (f *fakeResolver) ResolveForItem(ctx context.Context, item *database.FileHealth, meta *model.WebhookMetadata) model.Ownership {
	if f.panicOn != "" && item.FilePath == f.panicOn {
		panic("resolver boom")
	}
	if o, ok := f.byPath[item.FilePath]; ok {
		return o
	}
	return model.Ownership{Status: model.OwnershipUnknown}
}

// --- helpers ---

func cfgGetter(enabled bool, maxDeletes, threshold int) config.ConfigGetter {
	c := &config.Config{}
	c.Health.CorruptedTriage = config.CorruptedTriageConfig{
		Enabled:                 &enabled,
		MaxDeletesPerRun:        maxDeletes,
		MassEventThreshold:      threshold,
		BackstopIntervalMinutes: 360,
	}
	return func() *config.Config { return c }
}

func corruptedItem(path string) *database.FileHealth {
	return &database.FileHealth{FilePath: path, Status: database.HealthStatusCorrupted}
}

// --- tests ---

func TestEvaluate_FileRemovedZombie(t *testing.T) {
	store := &fakeStore{}
	meta := &fakeMeta{exists: map[string]bool{"/a.mkv": false}}
	// Resolver panics if consulted — proves ownership is irrelevant for zombies.
	res := &fakeResolver{panicOn: "/a.mkv"}
	svc := NewService(cfgGetter(true, 50, 500), store, meta, res)

	dec := svc.Evaluate(context.Background(), corruptedItem("/a.mkv"))
	if dec.Action != ActionDelete || dec.Reason != ReasonFileRemoved {
		t.Fatalf("got %+v; want delete/file_removed", dec)
	}
}

func TestEvaluate_DeadUnownedDeletes(t *testing.T) {
	store := &fakeStore{}
	meta := &fakeMeta{}
	res := &fakeResolver{byPath: map[string]model.Ownership{"/a.mkv": {Status: model.OwnershipUnowned}}}
	svc := NewService(cfgGetter(true, 50, 500), store, meta, res)

	if got := svc.ProcessItem(context.Background(), corruptedItem("/a.mkv"), SourceEnterCorrupted); !got {
		t.Fatal("expected delete")
	}
	if len(store.deleted) != 1 || len(meta.deleted) != 1 {
		t.Fatalf("expected row+meta deletion, got rows=%v meta=%v", store.deleted, meta.deleted)
	}
}

func TestEvaluate_DeadReplacedDeletes(t *testing.T) {
	store := &fakeStore{}
	meta := &fakeMeta{}
	res := &fakeResolver{byPath: map[string]model.Ownership{
		"/a.mkv": {Status: model.OwnershipReplaced, ReplacementID: 42, InstanceType: "radarr", InstanceName: "r1"},
	}}
	svc := NewService(cfgGetter(true, 50, 500), store, meta, res)

	dec := svc.Evaluate(context.Background(), corruptedItem("/a.mkv"))
	if dec.Action != ActionDelete || dec.Reason != ReasonDeadReplaced || dec.Ownership.ReplacementID != 42 {
		t.Fatalf("got %+v; want delete/dead_replaced/repl=42", dec)
	}
}

func TestEvaluate_OwnedKept(t *testing.T) {
	res := &fakeResolver{byPath: map[string]model.Ownership{"/a.mkv": {Status: model.OwnershipOwned}}}
	svc := NewService(cfgGetter(true, 50, 500), &fakeStore{}, &fakeMeta{}, res)

	dec := svc.Evaluate(context.Background(), corruptedItem("/a.mkv"))
	if dec.Action != ActionKeep || dec.Reason != ReasonOwned {
		t.Fatalf("got %+v; want keep/owned", dec)
	}
}

func TestEvaluate_UnknownKeptFailClosed(t *testing.T) {
	res := &fakeResolver{byPath: map[string]model.Ownership{"/a.mkv": {Status: model.OwnershipUnknown}}}
	svc := NewService(cfgGetter(true, 50, 500), &fakeStore{}, &fakeMeta{}, res)

	dec := svc.Evaluate(context.Background(), corruptedItem("/a.mkv"))
	if dec.Action != ActionKeep || dec.Reason != ReasonUnknown {
		t.Fatalf("got %+v; want keep/unknown (fail closed)", dec)
	}
}

func TestEvaluate_MetaExistsErrorFailsClosed(t *testing.T) {
	meta := &fakeMeta{existsErr: errors.New("io error")}
	svc := NewService(cfgGetter(true, 50, 500), &fakeStore{}, meta, &fakeResolver{})

	dec := svc.Evaluate(context.Background(), corruptedItem("/a.mkv"))
	if dec.Action != ActionKeep {
		t.Fatalf("got %+v; want keep on meta error (fail closed)", dec)
	}
}

func TestProcessItem_StatusGuardSkipsMetaDelete(t *testing.T) {
	// The record is no longer corrupted at delete time -> guarded out, .meta kept.
	store := &fakeStore{status: map[string]database.HealthStatus{"/a.mkv": database.HealthStatusHealthy}}
	meta := &fakeMeta{}
	res := &fakeResolver{byPath: map[string]model.Ownership{"/a.mkv": {Status: model.OwnershipUnowned}}}
	svc := NewService(cfgGetter(true, 50, 500), store, meta, res)

	if got := svc.ProcessItem(context.Background(), corruptedItem("/a.mkv"), SourceWebhook); got {
		t.Fatal("expected no delete (status guarded)")
	}
	if len(store.deleted) != 0 {
		t.Errorf("row should not be deleted, got %v", store.deleted)
	}
	if len(meta.deleted) != 0 {
		t.Errorf("meta must NOT be deleted when status guard fails, got %v", meta.deleted)
	}
}

func TestProcessItem_DisabledIsNoop(t *testing.T) {
	store := &fakeStore{}
	meta := &fakeMeta{}
	res := &fakeResolver{byPath: map[string]model.Ownership{"/a.mkv": {Status: model.OwnershipUnowned}}}
	svc := NewService(cfgGetter(false, 50, 500), store, meta, res) // disabled

	if got := svc.ProcessItem(context.Background(), corruptedItem("/a.mkv"), SourceEnterCorrupted); got {
		t.Fatal("disabled triage must not delete")
	}
	if len(store.deleted) != 0 || len(meta.deleted) != 0 {
		t.Fatal("disabled triage must perform no writes")
	}
}

func TestRun_PerRunCap(t *testing.T) {
	store := &fakeStore{}
	meta := &fakeMeta{}
	res := &fakeResolver{byPath: map[string]model.Ownership{
		"/1": {Status: model.OwnershipUnowned},
		"/2": {Status: model.OwnershipUnowned},
		"/3": {Status: model.OwnershipUnowned},
		"/4": {Status: model.OwnershipUnowned},
	}}
	svc := NewService(cfgGetter(true, 2, 500), store, meta, res) // cap=2

	st := svc.Run(context.Background(), []*database.FileHealth{
		corruptedItem("/1"), corruptedItem("/2"), corruptedItem("/3"), corruptedItem("/4"),
	}, SourceBackstop)

	if st.Deleted != 2 || !st.Capped {
		t.Fatalf("got deleted=%d capped=%v; want 2/true", st.Deleted, st.Capped)
	}
	if len(store.deleted) != 2 {
		t.Errorf("expected exactly 2 row deletions, got %v", store.deleted)
	}
}

func TestRun_MassEventAbort(t *testing.T) {
	store := &fakeStore{}
	meta := &fakeMeta{}
	res := &fakeResolver{byPath: map[string]model.Ownership{
		"/1": {Status: model.OwnershipUnowned},
		"/2": {Status: model.OwnershipUnowned},
		"/3": {Status: model.OwnershipUnowned},
	}}
	svc := NewService(cfgGetter(true, 50, 2), store, meta, res) // threshold=2, 3 candidates

	st := svc.Run(context.Background(), []*database.FileHealth{
		corruptedItem("/1"), corruptedItem("/2"), corruptedItem("/3"),
	}, SourceBackstop)

	if !st.Aborted || st.Deleted != 0 {
		t.Fatalf("got aborted=%v deleted=%d; want true/0", st.Aborted, st.Deleted)
	}
	if len(store.deleted) != 0 {
		t.Errorf("mass-event abort must delete nothing, got %v", store.deleted)
	}
}

func TestRun_PanicIsolation(t *testing.T) {
	store := &fakeStore{}
	meta := &fakeMeta{}
	res := &fakeResolver{
		byPath: map[string]model.Ownership{
			"/1": {Status: model.OwnershipUnowned},
			"/3": {Status: model.OwnershipUnowned},
		},
		panicOn: "/2",
	}
	svc := NewService(cfgGetter(true, 50, 500), store, meta, res)

	st := svc.Run(context.Background(), []*database.FileHealth{
		corruptedItem("/1"), corruptedItem("/2"), corruptedItem("/3"),
	}, SourceBackstop)

	if st.Deleted != 2 || st.Errors != 1 {
		t.Fatalf("got deleted=%d errors=%d; want 2/1 (panic isolated)", st.Deleted, st.Errors)
	}
}

func TestRun_FunnelCounts(t *testing.T) {
	store := &fakeStore{}
	meta := &fakeMeta{exists: map[string]bool{"/removed": false}}
	res := &fakeResolver{byPath: map[string]model.Ownership{
		"/unowned":  {Status: model.OwnershipUnowned},
		"/replaced": {Status: model.OwnershipReplaced, ReplacementID: 9},
		"/owned":    {Status: model.OwnershipOwned},
		"/unknown":  {Status: model.OwnershipUnknown},
	}}
	svc := NewService(cfgGetter(true, 50, 500), store, meta, res)

	st := svc.Run(context.Background(), []*database.FileHealth{
		corruptedItem("/removed"), corruptedItem("/unowned"), corruptedItem("/replaced"),
		corruptedItem("/owned"), corruptedItem("/unknown"),
	}, SourceBackstop)

	if st.Considered != 5 || st.Deleted != 3 {
		t.Fatalf("considered=%d deleted=%d; want 5/3", st.Considered, st.Deleted)
	}
	if st.ByReason[ReasonFileRemoved] != 1 || st.ByReason[ReasonDeadUnowned] != 1 || st.ByReason[ReasonDeadReplaced] != 1 {
		t.Errorf("byReason = %+v", st.ByReason)
	}
	if st.SkippedOwned != 1 || st.SkippedUnknown != 1 {
		t.Errorf("skippedOwned=%d skippedUnknown=%d; want 1/1", st.SkippedOwned, st.SkippedUnknown)
	}
}

func TestSweep_UsesStoreList(t *testing.T) {
	store := &fakeStore{listResp: []*database.FileHealth{corruptedItem("/a"), corruptedItem("/b")}}
	meta := &fakeMeta{}
	res := &fakeResolver{byPath: map[string]model.Ownership{
		"/a": {Status: model.OwnershipUnowned},
		"/b": {Status: model.OwnershipOwned},
	}}
	svc := NewService(cfgGetter(true, 50, 500), store, meta, res)

	st, err := svc.Sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep error: %v", err)
	}
	if st.Deleted != 1 || st.SkippedOwned != 1 {
		t.Fatalf("got deleted=%d skippedOwned=%d; want 1/1", st.Deleted, st.SkippedOwned)
	}
}

func TestSweep_ListErrorPropagates(t *testing.T) {
	store := &fakeStore{listErr: errors.New("db down")}
	svc := NewService(cfgGetter(true, 50, 500), store, &fakeMeta{}, &fakeResolver{})
	if _, err := svc.Sweep(context.Background()); err == nil {
		t.Fatal("expected error from sweep when list fails")
	}
}

func TestAdaptiveInterval(t *testing.T) {
	base := 60 * time.Minute
	if got := AdaptiveInterval(base, Stats{Aborted: true}); got != base*4 {
		t.Errorf("aborted: got %v; want %v", got, base*4)
	}
	if got := AdaptiveInterval(base, Stats{Deleted: 3}); got != base/2 {
		t.Errorf("worked: got %v; want %v", got, base/2)
	}
	if got := AdaptiveInterval(base, Stats{Capped: true}); got != base/2 {
		t.Errorf("capped: got %v; want %v", got, base/2)
	}
	if got := AdaptiveInterval(base, Stats{}); got != base*2 {
		t.Errorf("clean: got %v; want %v", got, base*2)
	}
	// Floor at 1 minute.
	if got := AdaptiveInterval(1*time.Minute, Stats{Deleted: 1}); got != time.Minute {
		t.Errorf("floor: got %v; want 1m", got)
	}
}

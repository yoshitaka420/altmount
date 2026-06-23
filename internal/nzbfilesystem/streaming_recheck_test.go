package nzbfilesystem

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/javi11/altmount/internal/config"
	"github.com/javi11/altmount/internal/database"
	"github.com/javi11/altmount/internal/metadata"
	metapb "github.com/javi11/altmount/internal/metadata/proto"
	"github.com/javi11/altmount/internal/usenet"
)

// streaming_recheck_test.go pins the streaming-failure routing: a missing
// article (430) is enqueued for the tolerance-aware health re-check WITHOUT
// condemning (no .meta move, no repair_triggered), while content/yEnc
// corruption is condemned directly exactly as before.

func newHealthRepoForTest(t *testing.T) *database.HealthRepository {
	t.Helper()
	db, err := sql.Open("sqlite3", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`
		CREATE TABLE file_health (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			file_path TEXT NOT NULL UNIQUE,
			library_path TEXT,
			status TEXT NOT NULL,
			last_checked DATETIME,
			last_error TEXT,
			retry_count INTEGER DEFAULT 0,
			max_retries INTEGER DEFAULT 3,
			repair_retry_count INTEGER DEFAULT 0,
			max_repair_retries INTEGER DEFAULT 3,
			source_nzb_path TEXT,
			error_details TEXT,
			metadata TEXT,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			release_date DATETIME,
			scheduled_check_at DATETIME,
			priority INTEGER DEFAULT 0,
			streaming_failure_count INTEGER DEFAULT 0,
			is_masked BOOLEAN DEFAULT FALSE,
			indexer TEXT DEFAULT NULL
		);
	`); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	return database.NewHealthRepository(db, database.DialectSQLite)
}

func maskingDisabledConfig() config.ConfigGetter {
	disabled := false
	c := &config.Config{}
	c.Streaming.FailureMasking.Enabled = &disabled
	return func() *config.Config { return c }
}

func newRecheckMVF(name string, ms *metadata.MetadataService, repo *database.HealthRepository) *MetadataVirtualFile {
	return &MetadataVirtualFile{
		name:             name,
		meta:             &fileHandleMeta{FileSize: 1},
		metadataService:  ms,
		healthRepository: repo,
		repairCoalescer:  nil, // nil-safe: ShouldTrigger returns true, EnqueueRefresh no-ops
		configGetter:     maskingDisabledConfig(),
		ctx:              context.Background(),
	}
}

// TestUpdateFileHealthOnError_MissingArticle_EnqueuesRecheckNoMove: a 430 must
// route to a priority 'pending' re-check and must NOT move the .meta or set
// repair_triggered.
func TestUpdateFileHealthOnError_MissingArticle_EnqueuesRecheckNoMove(t *testing.T) {
	root := t.TempDir()
	ms := metadata.NewMetadataService(root)
	repo := newHealthRepoForTest(t)
	ctx := context.Background()
	const name = "complete/show.s01e01.mkv"

	// Seed a non-terminal record + the original .meta on disk.
	if err := repo.UpdateFileHealthScheduled(ctx, name, database.HealthStatusChecking, nil, nil, nil, false, time.Now().UTC()); err != nil {
		t.Fatalf("seed health: %v", err)
	}
	if err := ms.WriteFileMetadata(name, &metapb.FileMetadata{FileSize: 1}); err != nil {
		t.Fatalf("write meta: %v", err)
	}

	mvf := newRecheckMVF(name, ms, repo)
	mvf.updateFileHealthOnError(&usenet.DataCorruptionError{UnderlyingErr: errors.New("430 no such article"), Missing: true}, true)

	// DB: routed to the check queue (pending), NOT repair_triggered.
	got, err := repo.GetFileHealth(ctx, name)
	if err != nil || got == nil {
		t.Fatalf("GetFileHealth: %v (nil=%v)", err, got == nil)
	}
	if got.Status != database.HealthStatusPending {
		t.Errorf("status = %q; want %q (430 must enqueue a recheck, not condemn)", got.Status, database.HealthStatusPending)
	}

	// .meta NOT moved: original still readable, no corrupted_metadata copy.
	if m, _ := ms.ReadFileMetadata(name); m == nil {
		t.Errorf("original .meta was moved/removed; a 430 must not condemn the file")
	}
	if _, err := os.Stat(filepath.Join(root, "corrupted_metadata", "complete", "show.s01e01.mkv.meta")); err == nil {
		t.Errorf("a corrupted_metadata copy exists; a 430 must not move the .meta")
	}
}

// TestUpdateFileHealthOnError_MissingArticle_LeavesInFlightRepair: a 430 must not
// yank a record already in repair (repair_triggered) back into the check queue.
func TestUpdateFileHealthOnError_MissingArticle_LeavesInFlightRepair(t *testing.T) {
	root := t.TempDir()
	ms := metadata.NewMetadataService(root)
	repo := newHealthRepoForTest(t)
	ctx := context.Background()
	const name = "complete/inflight.mkv"

	if err := repo.UpdateFileHealthScheduled(ctx, name, database.HealthStatusRepairTriggered, nil, nil, nil, true, time.Now().UTC()); err != nil {
		t.Fatalf("seed health: %v", err)
	}
	if err := ms.WriteFileMetadata(name, &metapb.FileMetadata{FileSize: 1}); err != nil {
		t.Fatalf("write meta: %v", err)
	}

	mvf := newRecheckMVF(name, ms, repo)
	mvf.updateFileHealthOnError(&usenet.DataCorruptionError{UnderlyingErr: errors.New("430"), Missing: true}, true)

	got, err := repo.GetFileHealth(ctx, name)
	if err != nil || got == nil {
		t.Fatalf("GetFileHealth: %v", err)
	}
	if got.Status != database.HealthStatusRepairTriggered {
		t.Errorf("status = %q; want %q (in-flight repair must be left untouched)", got.Status, database.HealthStatusRepairTriggered)
	}
}

// TestUpdateFileHealthOnError_ContentCorruption_CondemnsAndMoves: yEnc/content
// corruption (Missing=false) must condemn directly — set repair_triggered and
// move the .meta to corrupted_metadata (file has a library path).
func TestUpdateFileHealthOnError_ContentCorruption_CondemnsAndMoves(t *testing.T) {
	root := t.TempDir()
	ms := metadata.NewMetadataService(root)
	repo := newHealthRepoForTest(t)
	ctx := context.Background()
	const name = "complete/corrupt.mkv"

	if err := repo.UpdateFileHealthScheduled(ctx, name, database.HealthStatusChecking, nil, nil, nil, false, time.Now().UTC()); err != nil {
		t.Fatalf("seed health: %v", err)
	}
	// MoveToCorrupted only runs when the record has a library path.
	if err := repo.UpdateLibraryPath(ctx, name, "/library/corrupt.mkv"); err != nil {
		t.Fatalf("set library path: %v", err)
	}
	if err := ms.WriteFileMetadata(name, &metapb.FileMetadata{FileSize: 1}); err != nil {
		t.Fatalf("write meta: %v", err)
	}

	mvf := newRecheckMVF(name, ms, repo)
	mvf.updateFileHealthOnError(&usenet.DataCorruptionError{UnderlyingErr: errors.New("yenc: data corruption detected"), Missing: false}, false)

	got, err := repo.GetFileHealth(ctx, name)
	if err != nil || got == nil {
		t.Fatalf("GetFileHealth: %v", err)
	}
	if got.Status != database.HealthStatusRepairTriggered {
		t.Errorf("status = %q; want %q (content corruption condemns directly)", got.Status, database.HealthStatusRepairTriggered)
	}

	// .meta moved to the safety folder: original gone, corrupted copy present.
	if m, _ := ms.ReadFileMetadata(name); m != nil {
		t.Errorf("original .meta still present; content corruption must move it to corrupted_metadata")
	}
	if _, err := os.Stat(filepath.Join(root, "corrupted_metadata", "complete", "corrupt.mkv.meta")); err != nil {
		t.Errorf("expected .meta in corrupted_metadata, stat err: %v", err)
	}
}

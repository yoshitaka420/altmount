package database

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConcurrentQueueItemClaims(t *testing.T) {
	// Setup: In-memory SQLite with shared cache for multi-connection testing
	db, err := sql.Open("sqlite3", "file:test_concurrent_claims?mode=memory&cache=shared")
	require.NoError(t, err, "Failed to open in-memory database")
	defer db.Close()

	// Create schema and seed with ONE pending item
	setupQueueSchema(t, db)
	insertQueueItem(t, db, 1, "test.nzb", "pending")

	// Configure connection pool (simulate multiple workers)
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)

	repo := NewRepository(db, DialectSQLite)

	// Test: Launch 10 concurrent workers trying to claim
	numWorkers := 10
	results := make(chan *ImportQueueItem, numWorkers)
	errors := make(chan error, numWorkers)
	var wg sync.WaitGroup

	for i := range numWorkers {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			item, err := repo.ClaimNextQueueItem(context.Background())
			if err != nil {
				errors <- err
				return
			}
			results <- item
		}(i)
	}

	wg.Wait()
	close(results)
	close(errors)

	// Verify: Only database lock errors are acceptable
	// (immediate transactions fail fast under contention)
	var lockErrorCount int
	for err := range errors {
		if err != nil && (err.Error() == "failed to claim queue item: database table is locked" ||
			err.Error() == "failed to claim queue item: database is locked") {
			lockErrorCount++
		} else if err != nil {
			t.Errorf("Unexpected non-lock error from worker: %v", err)
		}
	}
	t.Logf("Lock errors (expected): %d", lockErrorCount)

	// Verify: Exactly ONE worker claimed the item
	var claimedCount int
	var claimedItem *ImportQueueItem
	for item := range results {
		if item != nil {
			claimedCount++
			claimedItem = item
		}
	}

	// Some workers may fail with "database table is locked" - this is expected
	// behavior with immediate transactions and concurrent access
	t.Logf("Claimed: %d items, Lock errors: %d", claimedCount, numWorkers-claimedCount)

	assert.Equal(t, 1, claimedCount, "Exactly one worker should claim the item")
	assert.NotNil(t, claimedItem, "Claimed item should not be nil")
	if claimedItem != nil {
		assert.Equal(t, int64(1), claimedItem.ID, "Claimed item ID should be 1")
		assert.Equal(t, QueueStatusProcessing, claimedItem.Status, "Claimed item status should be processing")
		assert.Equal(t, "test.nzb", claimedItem.NzbPath, "Claimed item path should match")
	}

	// Verify: Item in database is marked as processing
	status := getQueueItemStatus(t, db, 1)
	assert.Equal(t, "processing", status, "Item status in database should be processing")
}

func TestConcurrentQueueItemClaims_MultipleItems(t *testing.T) {
	// Setup: Test with multiple pending items
	db, err := sql.Open("sqlite3", "file:test_concurrent_multiple?mode=memory&cache=shared")
	require.NoError(t, err, "Failed to open in-memory database")
	defer db.Close()

	setupQueueSchema(t, db)

	// Insert 5 pending items
	for i := int64(1); i <= 5; i++ {
		insertQueueItem(t, db, i, "test"+string(rune('0'+i))+".nzb", "pending")
	}

	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)

	repo := NewRepository(db, DialectSQLite)

	// Test: Launch 10 concurrent workers (more workers than items)
	numWorkers := 10
	claimedIDs := make(chan int64, numWorkers)
	errors := make(chan error, numWorkers)
	var wg sync.WaitGroup

	for i := range numWorkers {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			item, err := repo.ClaimNextQueueItem(context.Background())
			if err != nil {
				errors <- err
				return
			}
			if item != nil {
				claimedIDs <- item.ID
			}
		}(i)
	}

	wg.Wait()
	close(claimedIDs)
	close(errors)

	// Count lock errors (expected under contention)
	var lockErrorCount int
	for err := range errors {
		if err != nil && (err.Error() == "failed to claim queue item: database table is locked" ||
			err.Error() == "failed to claim queue item: database is locked") {
			lockErrorCount++
		} else if err != nil {
			t.Errorf("Unexpected non-lock error: %v", err)
		}
	}
	t.Logf("Lock errors (expected due to contention): %d", lockErrorCount)

	// Verify: No more than 5 items were claimed (no duplicates)
	claimed := make(map[int64]bool)
	for id := range claimedIDs {
		if claimed[id] {
			t.Errorf("Item %d was claimed more than once!", id)
		}
		claimed[id] = true
	}

	// With lock errors, we may claim fewer than 5 items, but should claim at least 1
	assert.GreaterOrEqual(t, len(claimed), 1, "At least 1 item should be claimed")
	assert.LessOrEqual(t, len(claimed), 5, "No more than 5 items should be claimed")

	// Verify: Claimed items are marked as processing
	processingCount := countQueueItemsByStatus(t, db, "processing")
	assert.Equal(t, len(claimed), processingCount, "All claimed items should be marked as processing")
	t.Logf("Successfully claimed %d out of 5 items", len(claimed))
}

func TestClaimNextQueueItem_NoAvailableItems(t *testing.T) {
	// Setup: Empty queue
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	defer db.Close()

	setupQueueSchema(t, db)

	repo := NewRepository(db, DialectSQLite)

	// Test: Try to claim from empty queue
	item, err := repo.ClaimNextQueueItem(context.Background())

	// Verify: No error, but nil item
	require.NoError(t, err, "Should not error on empty queue")
	assert.Nil(t, item, "Item should be nil when queue is empty")
}

func TestClaimNextQueueItem_PriorityOrdering(t *testing.T) {
	// Setup: Items with different priorities
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	defer db.Close()

	setupQueueSchema(t, db)

	// Insert items with different priorities (1=high, 2=normal, 3=low)
	_, err = db.Exec(`
		INSERT INTO import_queue (id, nzb_path, status, priority) VALUES
		(1, 'low.nzb', 'pending', 3),
		(2, 'high.nzb', 'pending', 1),
		(3, 'normal.nzb', 'pending', 2)
	`)
	require.NoError(t, err)

	repo := NewRepository(db, DialectSQLite)

	// Test: Claim items in priority order
	item1, err := repo.ClaimNextQueueItem(context.Background())
	require.NoError(t, err)
	require.NotNil(t, item1)
	assert.Equal(t, int64(2), item1.ID, "Should claim high priority item first")

	item2, err := repo.ClaimNextQueueItem(context.Background())
	require.NoError(t, err)
	require.NotNil(t, item2)
	assert.Equal(t, int64(3), item2.ID, "Should claim normal priority item second")

	item3, err := repo.ClaimNextQueueItem(context.Background())
	require.NoError(t, err)
	require.NotNil(t, item3)
	assert.Equal(t, int64(1), item3.ID, "Should claim low priority item last")
}

func TestListQueueItems_HideStremioCompleted(t *testing.T) {
	db, err := sql.Open("sqlite3", "file:test_hide_stremio?mode=memory&cache=shared")
	require.NoError(t, err, "Failed to open in-memory database")
	defer db.Close()

	setupQueueSchema(t, db)
	repo := NewRepository(db, DialectSQLite)
	ctx := context.Background()

	now := time.Now()
	oldCompleted := now.Add(-10 * time.Minute)
	justCompleted := now.Add(-5 * time.Second)

	insert := func(id int64, nzbPath string, downloadID *string, status string, completedAt *time.Time) {
		_, err := db.Exec(
			`INSERT INTO import_queue (id, nzb_path, download_id, status, completed_at, priority) VALUES (?, ?, ?, ?, ?, 1)`,
			id, nzbPath, downloadID, status, completedAt,
		)
		require.NoError(t, err)
	}

	stremioOld := "stremio:aaa"
	stremioFresh := "stremio:bbb"
	stremioFailed := "stremio:ccc"
	arrID := "sonarr-guid"

	insert(1, "arr-no-id.nzb", nil, "completed", &oldCompleted)
	insert(2, "arr-with-id.nzb", &arrID, "completed", &oldCompleted)
	insert(3, "stremio-old.nzb", &stremioOld, "completed", &oldCompleted)
	insert(4, "stremio-fresh.nzb", &stremioFresh, "completed", &justCompleted)
	insert(5, "stremio-failed.nzb", &stremioFailed, "failed", nil)

	// Nil cutoff: nothing hidden
	items, err := repo.ListQueueItems(ctx, nil, "", "", 100, 0, "created_at", "asc", nil)
	require.NoError(t, err)
	assert.Len(t, items, 5, "nil cutoff must return all rows")

	count, err := repo.CountQueueItems(ctx, nil, "", "", nil)
	require.NoError(t, err)
	assert.Equal(t, 5, count)

	// Cutoff 60s ago: only the old completed stremio row is hidden
	cutoff := now.Add(-60 * time.Second)
	items, err = repo.ListQueueItems(ctx, nil, "", "", 100, 0, "created_at", "asc", &cutoff)
	require.NoError(t, err)
	ids := make([]int64, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	assert.ElementsMatch(t, []int64{1, 2, 4, 5}, ids,
		"only the completed stremio row older than the cutoff should be hidden")

	count, err = repo.CountQueueItems(ctx, nil, "", "", &cutoff)
	require.NoError(t, err)
	assert.Equal(t, len(items), count, "count must agree with list")

	// Status filter still composes with the hide condition
	completed := QueueStatusCompleted
	items, err = repo.ListQueueItems(ctx, &completed, "", "", 100, 0, "created_at", "asc", &cutoff)
	require.NoError(t, err)
	ids = ids[:0]
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	assert.ElementsMatch(t, []int64{1, 2, 4}, ids)

	// Queue stats must agree with the filtered listing
	_, err = db.Exec(`
		CREATE TABLE queue_stats (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			total_queued INTEGER NOT NULL DEFAULT 0,
			total_processing INTEGER NOT NULL DEFAULT 0,
			total_completed INTEGER NOT NULL DEFAULT 0,
			total_failed INTEGER NOT NULL DEFAULT 0,
			avg_processing_time_ms INTEGER DEFAULT NULL,
			last_updated DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		INSERT INTO queue_stats (total_queued, total_processing, total_completed, total_failed) VALUES (0, 0, 0, 0);
	`)
	require.NoError(t, err)

	stats, err := repo.GetQueueStats(ctx, nil)
	require.NoError(t, err)
	assert.Equal(t, 4, stats.TotalCompleted, "nil cutoff counts all completed rows")

	stats, err = repo.GetQueueStats(ctx, &cutoff)
	require.NoError(t, err)
	assert.Equal(t, 3, stats.TotalCompleted, "cutoff excludes the hidden stremio row")
	assert.Equal(t, 1, stats.TotalFailed, "failed count unaffected")
}

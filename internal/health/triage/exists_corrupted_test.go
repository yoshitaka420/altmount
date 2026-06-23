package triage

import (
	"context"
	"testing"

	"github.com/javi11/altmount/internal/config"
	"github.com/javi11/altmount/internal/database"
	"github.com/javi11/altmount/internal/metadata"
	metapb "github.com/javi11/altmount/internal/metadata/proto"
)

// TestMetaStoreExists_CorruptedMetadataAware pins the fix: triage's Exists() must
// distinguish "an arr removed the file" (original .meta gone AND no copy in
// corrupted_metadata) from "AltMount hid the .meta in corrupted_metadata when it
// condemned the file" (a copy is present). The latter must report exists=true so
// Evaluate routes through the ownership gate instead of file_removed → delete.
func TestMetaStoreExists_CorruptedMetadataAware(t *testing.T) {
	ctx := context.Background()
	const vpath = "tv/Show/Show.S01E03.mkv"
	cfg := func() *config.Config { return &config.Config{} } // MountPath "" → rel == FilePath
	item := &database.FileHealth{FilePath: vpath}

	writeOriginal := func(t *testing.T, ms *metadata.MetadataService) {
		t.Helper()
		if err := ms.WriteFileMetadata(vpath, &metapb.FileMetadata{FileSize: 1}); err != nil {
			t.Fatalf("WriteFileMetadata: %v", err)
		}
	}

	t.Run("original .meta present -> exists", func(t *testing.T) {
		ms := metadata.NewMetadataService(t.TempDir())
		writeOriginal(t, ms)
		got, err := NewMetaStore(ms, cfg).Exists(ctx, item)
		if err != nil || !got {
			t.Fatalf("Exists = (%v, %v); want (true, nil)", got, err)
		}
	})

	t.Run("AltMount moved .meta to corrupted_metadata -> exists (dead-and-hidden)", func(t *testing.T) {
		ms := metadata.NewMetadataService(t.TempDir())
		writeOriginal(t, ms)
		if err := ms.MoveToCorrupted(ctx, vpath); err != nil {
			t.Fatalf("MoveToCorrupted: %v", err)
		}
		// Precondition: the original path is now empty (this is exactly what made
		// the old Exists() report file_removed).
		if m, _ := ms.ReadFileMetadata(vpath); m != nil {
			t.Fatalf("precondition failed: original .meta should be gone after move")
		}
		got, err := NewMetaStore(ms, cfg).Exists(ctx, item)
		if err != nil || !got {
			t.Fatalf("Exists after move = (%v, %v); want (true, nil) — moved-to-corrupted must NOT be file_removed", got, err)
		}
	})

	t.Run("genuinely removed (nothing on disk) -> not exists", func(t *testing.T) {
		ms := metadata.NewMetadataService(t.TempDir())
		got, err := NewMetaStore(ms, cfg).Exists(ctx, item)
		if err != nil || got {
			t.Fatalf("Exists = (%v, %v); want (false, nil) — file_removed cleanup must still apply", got, err)
		}
	})

	t.Run("MountPath is stripped when building the corrupted_metadata path", func(t *testing.T) {
		// FilePath carries a mount prefix; rel must strip it so the corrupted copy
		// (stored under the relative path) is still found.
		cfgMounted := func() *config.Config { return &config.Config{MountPath: "/mnt/altmount"} }
		mountedItem := &database.FileHealth{FilePath: "/mnt/altmount/" + vpath}
		ms := metadata.NewMetadataService(t.TempDir())
		writeOriginal(t, ms)
		if err := ms.MoveToCorrupted(ctx, vpath); err != nil {
			t.Fatalf("MoveToCorrupted: %v", err)
		}
		got, err := NewMetaStore(ms, cfgMounted).Exists(ctx, mountedItem)
		if err != nil || !got {
			t.Fatalf("Exists (mounted) = (%v, %v); want (true, nil)", got, err)
		}
	})
}

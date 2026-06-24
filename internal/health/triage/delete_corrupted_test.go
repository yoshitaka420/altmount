package triage

import (
	"context"
	"testing"

	"github.com/javi11/altmount/internal/config"
	"github.com/javi11/altmount/internal/database"
	"github.com/javi11/altmount/internal/metadata"
	metapb "github.com/javi11/altmount/internal/metadata/proto"
)

// TestMetaStoreDelete_RemovesCorruptedMetadataCopy pins the orphan-cleanup
// follow-up to the file_removed guard: when triage deletes a condemned record
// whose .meta AltMount had moved to corrupted_metadata, metaStore.Delete must
// also remove that safety copy — otherwise it is left orphaned on disk.
func TestMetaStoreDelete_RemovesCorruptedMetadataCopy(t *testing.T) {
	ctx := context.Background()
	cfg := func() *config.Config { return &config.Config{} } // MountPath "" → rel == FilePath

	t.Run("moved-to-corrupted_metadata: Delete removes the safety copy", func(t *testing.T) {
		const vpath = "tv/Show/Show.S01E03.mkv"
		ms := metadata.NewMetadataService(t.TempDir())
		if err := ms.WriteFileMetadata(vpath, &metapb.FileMetadata{FileSize: 1}); err != nil {
			t.Fatalf("WriteFileMetadata: %v", err)
		}
		if err := ms.MoveToCorrupted(ctx, vpath); err != nil {
			t.Fatalf("MoveToCorrupted: %v", err)
		}
		// Precondition: original gone, safety copy present.
		if m, _ := ms.ReadFileMetadata(vpath); m != nil {
			t.Fatalf("precondition: original .meta should be gone after move")
		}
		if !ms.CorruptedMetadataExists(vpath) {
			t.Fatalf("precondition: corrupted_metadata copy should exist after move")
		}

		if err := NewMetaStore(ms, cfg).Delete(ctx, &database.FileHealth{FilePath: vpath}); err != nil {
			t.Fatalf("Delete: %v", err)
		}

		if ms.CorruptedMetadataExists(vpath) {
			t.Errorf("corrupted_metadata copy still present after Delete — orphaned on disk")
		}
	})

	t.Run("not-yet-moved: Delete removes the original, no error on the absent copy", func(t *testing.T) {
		const vpath = "tv/Show/Show.S01E04.mkv"
		ms := metadata.NewMetadataService(t.TempDir())
		if err := ms.WriteFileMetadata(vpath, &metapb.FileMetadata{FileSize: 1}); err != nil {
			t.Fatalf("WriteFileMetadata: %v", err)
		}
		// No MoveToCorrupted: the .meta is still at the original path.

		if err := NewMetaStore(ms, cfg).Delete(ctx, &database.FileHealth{FilePath: vpath}); err != nil {
			t.Fatalf("Delete: %v", err)
		}

		if m, _ := ms.ReadFileMetadata(vpath); m != nil {
			t.Errorf("original .meta still present after Delete")
		}
		if ms.CorruptedMetadataExists(vpath) {
			t.Errorf("a corrupted_metadata copy unexpectedly exists")
		}
	})
}

// TestDeleteCorruptedMetadata_Idempotent pins that the metadata helper is a
// no-op (no error) when there is no corrupted_metadata copy to remove.
func TestDeleteCorruptedMetadata_Idempotent(t *testing.T) {
	ms := metadata.NewMetadataService(t.TempDir())
	if err := ms.DeleteCorruptedMetadata("tv/none/missing.mkv"); err != nil {
		t.Errorf("DeleteCorruptedMetadata on a missing copy = %v; want nil (idempotent)", err)
	}
}

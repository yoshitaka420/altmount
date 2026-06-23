package health

import (
	"context"
	"testing"

	"github.com/javi11/altmount/internal/config"
	metapb "github.com/javi11/altmount/internal/metadata/proto"
	"github.com/javi11/altmount/internal/pool"
	"github.com/javi11/altmount/internal/testsupport/fakepool"
	"github.com/javi11/altmount/internal/testsupport/segments"
	"github.com/javi11/nntppool/v4"
)

// checker_tolerance_test.go pins the acceptable_missing_segments_percentage
// tolerance wired into checkSingleFile. Until this change the knob was dead
// config (never read) and any missing segment condemned the file.

// contiguousSegments builds n perfectly sequential segments of `size` bytes,
// so CheckMetadataIntegrity (which checks contiguity against fileSize) passes.
func contiguousSegments(n, size int) []*metapb.SegmentData {
	segs := make([]*metapb.SegmentData, n)
	for i := 0; i < n; i++ {
		segs[i] = &metapb.SegmentData{
			Id:          segments.MessageID(i),
			SegmentSize: int64(size),
			StartOffset: int64(i * size),
			EndOffset:   int64(i*size + size - 1),
		}
	}
	return segs
}

func toleranceConfig(acceptablePct float64) config.ConfigGetter {
	c := &config.Config{}
	c.Health.AcceptableMissingSegmentsPercentage = acceptablePct
	return func() *config.Config { return c }
}

// TestCheckSingleFile_ConfigDrivenSampledTolerance proves a configured value
// changes condemnation through the real SAMPLED path (segment_sample_percentage),
// not just force-full: with 1000 segments at 5% sampling the validator checks 50
// segments; one missing (segment 0, always in the strategic first-3 sample) is
// 2% of the sample. acceptable=3 keeps it; acceptable=0 condemns it.
func TestCheckSingleFile_ConfigDrivenSampledTolerance(t *testing.T) {
	const segCount, segSize = 1000, 100

	run := func(t *testing.T, acceptablePct float64) EventType {
		segs := contiguousSegments(segCount, segSize)
		fp := fakepool.New()
		fp.SetBehavior(segments.MessageID(0), fakepool.SegmentBehavior{Err: nntppool.ErrArticleNotFound})
		mgr := &checkerTestPoolManager{client: fp}

		c := &config.Config{}
		c.Health.AcceptableMissingSegmentsPercentage = acceptablePct
		c.Health.SegmentSamplePercentage = 5 // sampled, not full
		hc := NewHealthChecker(nil, nil, mgr, func() *config.Config { return c }, nil)

		input := healthCheckInput{fileSize: int64(segCount * segSize), segments: segs}
		return hc.checkSingleFile(context.Background(), "tv/show.mkv", input).Type // no ForceFullCheck → samples
	}

	t.Run("config=3 → 1 missing sampled segment kept", func(t *testing.T) {
		if got := run(t, 3); got != EventTypeFileHealthy {
			t.Fatalf("event = %q; want %q (config tolerance 3 must keep a single sampled miss)", got, EventTypeFileHealthy)
		}
	})
	t.Run("config=0 → 1 missing condemned", func(t *testing.T) {
		if got := run(t, 0); got != EventTypeFileCorrupted {
			t.Fatalf("event = %q; want %q (default 0 must condemn on any missing)", got, EventTypeFileCorrupted)
		}
	})
}

func TestCheckSingleFile_AcceptableMissingTolerance(t *testing.T) {
	const (
		segCount = 100
		segSize  = 100
	)

	tests := []struct {
		name          string
		acceptablePct float64
		missing       int
		wantType      EventType
	}{
		{
			name:          "2% missing under 3% tolerance → healthy",
			acceptablePct: 3,
			missing:       2,
			wantType:      EventTypeFileHealthy,
		},
		{
			name:          "5% missing over 3% tolerance → corrupted",
			acceptablePct: 3,
			missing:       5,
			wantType:      EventTypeFileCorrupted,
		},
		{
			name:          "default 0 tolerance + 1 missing → corrupted (back-compat)",
			acceptablePct: 0,
			missing:       1,
			wantType:      EventTypeFileCorrupted,
		},
		{
			name:          "exactly at tolerance is not over → healthy",
			acceptablePct: 3,
			missing:       3,
			wantType:      EventTypeFileHealthy,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			segs := contiguousSegments(segCount, segSize)

			fp := fakepool.New()
			// Mark the first `missing` segments as permanently absent (430); the
			// rest default to present (Stat returns nil).
			for i := 0; i < tt.missing; i++ {
				fp.SetBehavior(segments.MessageID(i), fakepool.SegmentBehavior{
					Err: nntppool.ErrArticleNotFound,
				})
			}
			mgr := &checkerTestPoolManager{client: fp}

			hc := NewHealthChecker(nil, nil, mgr, toleranceConfig(tt.acceptablePct), nil)

			input := healthCheckInput{
				fileSize: int64(segCount * segSize),
				segments: segs,
			}
			// ForceFullCheck → 100% sampling so TotalChecked == segCount and the
			// missing percentage is exact and deterministic.
			event := hc.checkSingleFile(context.Background(), "tv/show.mkv", input, CheckOptions{ForceFullCheck: true})

			if event.Type != tt.wantType {
				t.Fatalf("event.Type = %q; want %q (err=%v)", event.Type, tt.wantType, event.Error)
			}
		})
	}
}

// checkerTestPoolManager is a minimal pool.Manager wrapping a fakepool.Client.
type checkerTestPoolManager struct {
	client pool.NntpClient
}

var _ pool.Manager = (*checkerTestPoolManager)(nil)

func (m *checkerTestPoolManager) GetPool() (pool.NntpClient, error)        { return m.client, nil }
func (m *checkerTestPoolManager) SetProviders(_ []nntppool.Provider) error { return nil }
func (m *checkerTestPoolManager) ClearPool() error                         { return nil }
func (m *checkerTestPoolManager) HasPool() bool                            { return true }
func (m *checkerTestPoolManager) GetMetrics() (pool.MetricsSnapshot, error) {
	return pool.MetricsSnapshot{}, nil
}
func (m *checkerTestPoolManager) ResetMetrics(_ context.Context, _, _ bool) error { return nil }
func (m *checkerTestPoolManager) ResetProviderErrors(_ context.Context) error     { return nil }
func (m *checkerTestPoolManager) IncArticlesDownloaded()                          {}
func (m *checkerTestPoolManager) UpdateDownloadProgress(_ string, _ int64)        {}
func (m *checkerTestPoolManager) IncArticlesPosted()                              {}
func (m *checkerTestPoolManager) AddProvider(_ nntppool.Provider) error           { return nil }
func (m *checkerTestPoolManager) RemoveProvider(_ string) error                   { return nil }
func (m *checkerTestPoolManager) ResetProviderQuota(_ context.Context, _ string) error {
	return nil
}
func (m *checkerTestPoolManager) SetProviderIDs(_ map[string]string) {}
func (m *checkerTestPoolManager) AcquireImportSlot(_ context.Context) (func(), error) {
	return func() {}, nil
}
func (m *checkerTestPoolManager) SetAdmissionCaps(_ int, _ int)               {}
func (m *checkerTestPoolManager) SetStreamSource(_ pool.StreamActivitySource) {}
func (m *checkerTestPoolManager) NotifyStreamChange()                         {}

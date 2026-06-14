package usenet

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/javi11/altmount/internal/pool"
	"github.com/javi11/altmount/internal/testsupport/fakepool"
	"github.com/javi11/altmount/internal/testsupport/segments"
	"github.com/javi11/nntppool/v4"
)

// usenet_reader_zerofill_test.go pins the mid-stream zero-fill behaviour:
// an isolated permanently-missing segment is substituted with a correctly
// sized buffer of zeros so playback survives, while the guard rails
// (first-segment guard, count cap, fraction cap, master switch) keep a
// genuinely-dead release failing honestly.

// discardReader builds a bare UsenetReader for unit-testing the pure
// decision in maybeZeroFill without spinning up the download pipeline. Only
// the fields maybeZeroFill touches are populated.
func discardReader(rg *segmentRange, opts ZeroFillOptions) *UsenetReader {
	return &UsenetReader{
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		rg:       rg,
		zeroFill: opts,
	}
}

// TestZeroFillSegmentLen pins the single highest-risk detail: the fill length
// must equal max(SegmentSize, End+1) so the buffer the consumer indexes into
// (seek to Start, read End-Start+1) is always in bounds and never desyncs the
// offsets of following segments.
func TestZeroFillSegmentLen(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		start, end  int64
		segmentSize int64
		want        int64
	}{
		{name: "full middle segment", start: 0, end: 767999, segmentSize: 768000, want: 768000},
		{name: "trimmed tail segment", start: 0, end: 100, segmentSize: 768000, want: 768000},
		{name: "trimmed front segment", start: 500, end: 767999, segmentSize: 768000, want: 768000},
		{name: "end beyond size falls back to End+1", start: 0, end: 20, segmentSize: 10, want: 21},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			seg := newSegment("id", tc.start, tc.end, tc.segmentSize, nil)
			if got := zeroFillSegmentLen(seg); got != tc.want {
				t.Errorf("zeroFillSegmentLen = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestMaybeZeroFill_DisabledByDefault pins that the zero value of
// ZeroFillOptions never fills — the feature is strictly opt-in via WithZeroFill.
func TestMaybeZeroFill_DisabledByDefault(t *testing.T) {
	t.Parallel()
	rg := &segmentRange{start: 0, end: 9999, startSegIdx: 0}
	r := discardReader(rg, ZeroFillOptions{}) // zero value: disabled
	seg := newSegment("seg-5", 0, 999, 1000, nil)

	if _, ok := r.maybeZeroFill(context.Background(), seg, 5); ok {
		t.Fatal("maybeZeroFill returned ok with disabled options")
	}
	if got := r.zeroFilledCount.Load(); got != 0 {
		t.Errorf("zeroFilledCount = %d, want 0 when disabled", got)
	}
}

// TestMaybeZeroFill_FirstSegmentGuard pins that the file's first article
// (absolute loader index 0) is never zero-filled, even when the local index
// would otherwise qualify.
func TestMaybeZeroFill_FirstSegmentGuard(t *testing.T) {
	t.Parallel()
	opts := ZeroFillOptions{Enabled: true, MaxSegments: 20, MaxFraction: 1.0}

	// Range starting at the file's first article: local 0 == absolute 0 -> guarded.
	t.Run("range at file start", func(t *testing.T) {
		t.Parallel()
		rg := &segmentRange{start: 0, end: 9999, startSegIdx: 0}
		r := discardReader(rg, opts)
		seg := newSegment("seg-0", 0, 999, 1000, nil)
		if _, ok := r.maybeZeroFill(context.Background(), seg, 0); ok {
			t.Fatal("maybeZeroFill filled the file's first article")
		}
	})

	// Range seeked into the middle of the file: local 0 maps to absolute 5,
	// so the first segment of this range is NOT the file's first article and
	// is eligible for zero-fill.
	t.Run("range seeked past file start", func(t *testing.T) {
		t.Parallel()
		rg := &segmentRange{start: 5000, end: 14999, startSegIdx: 5}
		r := discardReader(rg, opts)
		seg := newSegment("seg-5", 0, 999, 1000, nil)
		if _, ok := r.maybeZeroFill(context.Background(), seg, 0); !ok {
			t.Fatal("maybeZeroFill refused a non-first article after a seek")
		}
	})
}

// TestMaybeZeroFill_CountCap pins the per-stream count cap: once MaxSegments
// fills have happened, further misses fail honestly.
func TestMaybeZeroFill_CountCap(t *testing.T) {
	t.Parallel()
	// Large range so the fraction cap never trips before the count cap.
	rg := &segmentRange{start: 0, end: 1 << 30, startSegIdx: 0}
	opts := ZeroFillOptions{Enabled: true, MaxSegments: 3, MaxFraction: 1.0}
	r := discardReader(rg, opts)

	for i := 1; i <= 3; i++ {
		seg := newSegment("seg", 0, 999, 1000, nil)
		if _, ok := r.maybeZeroFill(context.Background(), seg, i); !ok {
			t.Fatalf("fill %d within cap rejected", i)
		}
	}
	// Fourth fill is over the cap.
	seg := newSegment("seg", 0, 999, 1000, nil)
	if _, ok := r.maybeZeroFill(context.Background(), seg, 4); ok {
		t.Fatal("maybeZeroFill exceeded the count cap")
	}
	if got := r.zeroFilledCount.Load(); got != 3 {
		t.Errorf("zeroFilledCount = %d, want 3 (over-cap fill must not increment)", got)
	}
}

// TestMaybeZeroFill_FractionCap pins the per-stream fraction cap: zero-filled
// bytes may not exceed MaxFraction of the streamed range.
func TestMaybeZeroFill_FractionCap(t *testing.T) {
	t.Parallel()
	// Range of 10_000 bytes, 0.02 fraction => 200-byte budget. A single
	// 1000-byte fill already exceeds it, so the very first miss fails honestly.
	rg := &segmentRange{start: 0, end: 9999, startSegIdx: 0}
	opts := ZeroFillOptions{Enabled: true, MaxSegments: 20, MaxFraction: 0.02}
	r := discardReader(rg, opts)

	seg := newSegment("seg", 0, 999, 1000, nil)
	if _, ok := r.maybeZeroFill(context.Background(), seg, 1); ok {
		t.Fatal("maybeZeroFill exceeded the fraction cap")
	}
	if got := r.zeroFilledBytes.Load(); got != 0 {
		t.Errorf("zeroFilledBytes = %d, want 0 when fraction cap rejects", got)
	}
}

// TestMaybeZeroFill_SuccessIncrementsCounters pins the happy path: an allowed
// fill returns a correctly-sized zero buffer and advances both counters.
func TestMaybeZeroFill_SuccessIncrementsCounters(t *testing.T) {
	t.Parallel()
	rg := &segmentRange{start: 0, end: 99999, startSegIdx: 0}
	opts := ZeroFillOptions{Enabled: true, MaxSegments: 20, MaxFraction: 0.5}
	r := discardReader(rg, opts)

	seg := newSegment("seg-7", 0, 999, 1000, nil)
	fill, ok := r.maybeZeroFill(context.Background(), seg, 7)
	if !ok {
		t.Fatal("maybeZeroFill rejected a fill within budget")
	}
	if int64(len(fill)) != 1000 {
		t.Errorf("fill length = %d, want 1000", len(fill))
	}
	for i, b := range fill {
		if b != 0 {
			t.Fatalf("fill[%d] = %d, want 0", i, b)
		}
	}
	if got := r.zeroFilledCount.Load(); got != 1 {
		t.Errorf("zeroFilledCount = %d, want 1", got)
	}
	if got := r.zeroFilledBytes.Load(); got != 1000 {
		t.Errorf("zeroFilledBytes = %d, want 1000", got)
	}
}

// newZeroFillReaderForTest constructs a fully-wired UsenetReader (download
// pipeline running) with zero-fill enabled, mirroring newReaderForTest but
// threading the WithZeroFill option through.
func newZeroFillReaderForTest(t testing.TB, ctx context.Context, fp *fakepool.Client, rg *segmentRange, maxPrefetch int, opts ZeroFillOptions) *UsenetReader {
	t.Helper()
	getter := func() (pool.NntpClient, error) { return fp, nil }
	ur, err := NewUsenetReader(ctx, getter, rg, maxPrefetch, noopMetrics{}, "test-stream", nil, WithZeroFill(opts))
	if err != nil {
		t.Fatalf("NewUsenetReader: %v", err)
	}
	t.Cleanup(func() { _ = ur.Close() })
	return ur
}

// TestZeroFill_MiddleSegmentReadsThrough is the headline integration test:
// a single missing middle segment is replaced with zeros, the stream reads to
// completion, and the neighbouring segments are intact.
func TestZeroFill_MiddleSegmentReadsThrough(t *testing.T) {
	t.Parallel()
	const (
		segCount    = 8
		segSize     = 128
		maxPrefetch = 8
		missing     = 3 // a middle segment (not the first)
	)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fp := fakepool.New()
	for i := 0; i < segCount; i++ {
		if i == missing {
			fp.SetBehavior(segments.MessageID(i), fakepool.SegmentBehavior{
				Err: nntppool.ErrArticleNotFound,
			})
			continue
		}
		fp.SetBehavior(segments.MessageID(i), fakepool.SegmentBehavior{
			Bytes: segments.Payload(i, segSize),
		})
	}

	rg := buildEagerRange(ctx, t, segCount, segSize)
	// Generous budget so the single small-file fill clears the fraction cap
	// (1/8 of the range); the caps themselves are unit-tested above.
	ur := newZeroFillReaderForTest(t, ctx, fp, rg, maxPrefetch,
		ZeroFillOptions{Enabled: true, MaxSegments: 20, MaxFraction: 1.0})
	ur.Start()

	got, err := io.ReadAll(ur)
	if err != nil {
		t.Fatalf("ReadAll: stream did not survive a missing middle segment: %v", err)
	}

	// Build the expected stream: real payloads everywhere except the missing
	// segment, which must be exactly segSize zero bytes.
	want := make([]byte, 0, segCount*segSize)
	for i := 0; i < segCount; i++ {
		if i == missing {
			want = append(want, make([]byte, segSize)...)
			continue
		}
		want = append(want, segments.Payload(i, segSize)...)
	}

	if len(got) != len(want) {
		t.Fatalf("stream length = %d, want %d (offset desync)", len(got), len(want))
	}
	if !bytes.Equal(got, want) {
		t.Error("reassembled stream mismatch: missing segment not cleanly zero-filled or neighbour corrupted")
	}

	// The missing region must be all zeros.
	zeroRegion := got[missing*segSize : (missing+1)*segSize]
	for i, b := range zeroRegion {
		if b != 0 {
			t.Fatalf("zero-filled region byte %d = %d, want 0", i, b)
		}
	}
}

// TestZeroFill_DisabledStillFails pins that without zero-fill a missing middle
// segment still fails the stream — the feature must not change behaviour when
// off.
func TestZeroFill_DisabledStillFails(t *testing.T) {
	t.Parallel()
	const (
		segCount    = 8
		segSize     = 128
		maxPrefetch = 8
		missing     = 3
	)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fp := fakepool.New()
	for i := 0; i < segCount; i++ {
		if i == missing {
			fp.SetBehavior(segments.MessageID(i), fakepool.SegmentBehavior{
				Err: nntppool.ErrArticleNotFound,
			})
			continue
		}
		fp.SetBehavior(segments.MessageID(i), fakepool.SegmentBehavior{
			Bytes: segments.Payload(i, segSize),
		})
	}

	rg := buildEagerRange(ctx, t, segCount, segSize)
	// No WithZeroFill option => disabled.
	ur := newReaderForTest(t, ctx, fp, rg, maxPrefetch)
	ur.Start()

	if _, err := io.ReadAll(ur); err == nil {
		t.Fatal("expected ReadAll to fail with zero-fill disabled, got nil error")
	}
}

// TestZeroFill_FirstSegmentMissingFails pins that the first-segment guard holds
// end-to-end: even with zero-fill on, a missing first article fails the stream.
func TestZeroFill_FirstSegmentMissingFails(t *testing.T) {
	t.Parallel()
	const (
		segCount    = 8
		segSize     = 128
		maxPrefetch = 8
	)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fp := fakepool.New()
	fp.SetBehavior(segments.MessageID(0), fakepool.SegmentBehavior{
		Err: nntppool.ErrArticleNotFound,
	})
	for i := 1; i < segCount; i++ {
		fp.SetBehavior(segments.MessageID(i), fakepool.SegmentBehavior{
			Bytes: segments.Payload(i, segSize),
		})
	}

	rg := buildEagerRange(ctx, t, segCount, segSize)
	ur := newZeroFillReaderForTest(t, ctx, fp, rg, maxPrefetch,
		ZeroFillOptions{Enabled: true, MaxSegments: 20, MaxFraction: 1.0})
	ur.Start()

	if _, err := io.ReadAll(ur); err == nil {
		t.Fatal("expected ReadAll to fail when the first article is missing, got nil error")
	}
}

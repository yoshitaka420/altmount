package usenet

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/javi11/altmount/internal/pool"
	"github.com/javi11/altmount/internal/testsupport/fakepool"
	"github.com/javi11/altmount/internal/testsupport/segments"
	"github.com/javi11/nntppool/v4"
)

// newZeroFillReader constructs a reader over an in-memory eager range bound to
// fp, with the supplied zero-fill policy. It is the shared rig for the
// end-to-end tests below.
func newZeroFillReader(t testing.TB, ctx context.Context, fp *fakepool.Client, rg *segmentRange, zf ZeroFillOptions) *UsenetReader {
	t.Helper()
	getter := func() (pool.NntpClient, error) { return fp, nil }
	ur, err := NewUsenetReader(ctx, getter, rg, 60, noopMetrics{}, "zerofill-test", nil, WithZeroFill(zf))
	if err != nil {
		t.Fatalf("NewUsenetReader: %v", err)
	}
	t.Cleanup(func() { _ = ur.Close() })
	return ur
}

// readerWithRange builds a bare UsenetReader for unit-testing the zero-fill
// decision helpers in isolation (no download manager, no network). Only the
// fields maybeZeroFill touches are populated.
func readerWithRange(startSegIdx int, rangeStart, rangeEnd int64, zf ZeroFillOptions) *UsenetReader {
	return &UsenetReader{
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		rg:       &segmentRange{startSegIdx: startSegIdx, start: rangeStart, end: rangeEnd},
		zeroFill: zf,
	}
}

func TestZeroFillSegmentLen(t *testing.T) {
	tests := []struct {
		name string
		seg  *segment
		want int64
	}{
		{"full segment", newSegment("a", 0, 99, 100, nil), 100},
		{"trimmed tail uses full article size", newSegment("a", 0, 40, 100, nil), 100},
		{"trimmed head uses full article size", newSegment("a", 60, 99, 100, nil), 100},
		{"missing SegmentSize falls back to End+1", newSegment("a", 0, 49, 0, nil), 50},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := zeroFillSegmentLen(tc.seg); got != tc.want {
				t.Fatalf("zeroFillSegmentLen = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestMaybeZeroFill_DisabledByDefault(t *testing.T) {
	// Zero-value options = disabled: the safe default for import/par2 readers.
	b := readerWithRange(0, 0, 1000, ZeroFillOptions{})
	if _, ok := b.maybeZeroFill(context.Background(), newSegment("x", 0, 99, 100, nil), 3); ok {
		t.Fatal("zero-fill must be disabled when options are the zero value")
	}
}

func TestMaybeZeroFill_FirstSegmentGuard(t *testing.T) {
	zf := ZeroFillOptions{Enabled: true, MaxSegments: 100, MaxFraction: 1}

	// File's first segment (startSegIdx 0 + local 0) is never zero-filled.
	b := readerWithRange(0, 0, 100000, zf)
	if _, ok := b.maybeZeroFill(context.Background(), newSegment("x", 0, 99, 100, nil), 0); ok {
		t.Fatal("the file's first segment must not be zero-filled")
	}

	// A non-first segment of the same file is eligible.
	b = readerWithRange(0, 0, 100000, zf)
	if _, ok := b.maybeZeroFill(context.Background(), newSegment("x", 0, 99, 100, nil), 1); !ok {
		t.Fatal("a non-first segment should be eligible for zero-fill")
	}

	// A range that starts mid-file (startSegIdx > 0) has no file-first segment,
	// so even local index 0 is eligible (it is not article 0 of the file).
	b = readerWithRange(5, 50000, 100000, zf)
	if _, ok := b.maybeZeroFill(context.Background(), newSegment("x", 0, 99, 100, nil), 0); !ok {
		t.Fatal("local index 0 of a mid-file range should be eligible")
	}
}

func TestMaybeZeroFill_CountBudget(t *testing.T) {
	// Unbounded fraction (0), count capped at 2.
	b := readerWithRange(0, 0, 1<<30, ZeroFillOptions{Enabled: true, MaxSegments: 2, MaxFraction: 0})
	seg := newSegment("x", 0, 99, 100, nil)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if _, ok := b.maybeZeroFill(ctx, seg, 1); !ok {
			t.Fatalf("fill %d should be within the count budget", i)
		}
	}
	if _, ok := b.maybeZeroFill(ctx, seg, 1); ok {
		t.Fatal("third fill should exceed the count budget")
	}
}

func TestMaybeZeroFill_FractionBudget(t *testing.T) {
	// Range = 100 bytes, fraction cap 0.5 => 50 bytes. Each fill is 30 bytes:
	// the first is allowed (30 <= 50), the second projects to 60 > 50 and fails.
	b := readerWithRange(0, 0, 99, ZeroFillOptions{Enabled: true, MaxSegments: 0, MaxFraction: 0.5})
	seg := newSegment("x", 0, 29, 30, nil)
	ctx := context.Background()

	if _, ok := b.maybeZeroFill(ctx, seg, 1); !ok {
		t.Fatal("first fill should fit within the fraction budget")
	}
	if _, ok := b.maybeZeroFill(ctx, seg, 1); ok {
		t.Fatal("second fill should exceed the fraction budget")
	}
}

func TestMaybeZeroFill_Success(t *testing.T) {
	b := readerWithRange(0, 0, 1<<30, ZeroFillOptions{Enabled: true, MaxSegments: 10, MaxFraction: 1})
	seg := newSegment("x", 0, 99, 100, nil)

	fill, ok := b.maybeZeroFill(context.Background(), seg, 4)
	if !ok {
		t.Fatal("expected zero-fill to succeed")
	}
	if int64(len(fill)) != zeroFillSegmentLen(seg) {
		t.Fatalf("fill len = %d, want %d", len(fill), zeroFillSegmentLen(seg))
	}
	for i, v := range fill {
		if v != 0 {
			t.Fatalf("fill byte %d = %x, want 0", i, v)
		}
	}
	if got := b.zeroFilledCount.Load(); got != 1 {
		t.Fatalf("zeroFilledCount = %d, want 1", got)
	}
	if got := b.zeroFilledBytes.Load(); got != zeroFillSegmentLen(seg) {
		t.Fatalf("zeroFilledBytes = %d, want %d", got, zeroFillSegmentLen(seg))
	}
}

// TestZeroFill_StreamSurvivesMissingMiddleSegment is the end-to-end proof: a
// permanently-missing middle article is replaced by correctly-sized zeros and
// the full stream reads through, with surrounding segments intact.
func TestZeroFill_StreamSurvivesMissingMiddleSegment(t *testing.T) {
	ctx := context.Background()
	const n, segSize = 5, 16
	rg := buildEagerRange(ctx, t, n, segSize)

	fp := fakepool.New()
	fp.SetDefaultBehavior(fakepool.SegmentBehavior{Bytes: bytes.Repeat([]byte{0xAB}, segSize)})
	const missing = 2
	fp.SetBehavior(segments.MessageID(missing), fakepool.SegmentBehavior{Err: nntppool.ErrArticleNotFound})

	ur := newZeroFillReader(t, ctx, fp, rg, ZeroFillOptions{Enabled: true, MaxSegments: 20, MaxFraction: 0.9})

	got, err := io.ReadAll(ur)
	if err != nil {
		t.Fatalf("ReadAll over a single missing segment should succeed, got: %v", err)
	}
	if len(got) != n*segSize {
		t.Fatalf("read %d bytes, want %d", len(got), n*segSize)
	}
	// The missing segment's region is zeros.
	for i := missing * segSize; i < (missing+1)*segSize; i++ {
		if got[i] != 0 {
			t.Fatalf("byte %d = %x in the missing region, want 0", i, got[i])
		}
	}
	// A neighbouring segment is intact.
	if got[0] != 0xAB || got[(missing+1)*segSize] != 0xAB {
		t.Fatal("segments adjacent to the missing one were corrupted")
	}
}

// TestZeroFill_DisabledFailsOnMissingSegment guards the default: without the
// option a missing middle segment still fails the read (so import/par2 readers,
// which never opt in, keep failing honestly).
func TestZeroFill_DisabledFailsOnMissingSegment(t *testing.T) {
	ctx := context.Background()
	const n, segSize = 5, 16
	rg := buildEagerRange(ctx, t, n, segSize)

	fp := fakepool.New()
	fp.SetDefaultBehavior(fakepool.SegmentBehavior{Bytes: bytes.Repeat([]byte{0xAB}, segSize)})
	fp.SetBehavior(segments.MessageID(2), fakepool.SegmentBehavior{Err: nntppool.ErrArticleNotFound})

	getter := func() (pool.NntpClient, error) { return fp, nil }
	ur, err := NewUsenetReader(ctx, getter, rg, 60, noopMetrics{}, "zerofill-off", nil)
	if err != nil {
		t.Fatalf("NewUsenetReader: %v", err)
	}
	t.Cleanup(func() { _ = ur.Close() })

	if _, err := io.ReadAll(ur); err == nil || !errors.Is(err, nntppool.ErrArticleNotFound) {
		t.Fatalf("expected ErrArticleNotFound with zero-fill disabled, got: %v", err)
	}
}

// TestZeroFill_FirstSegmentNotFilledEndToEnd confirms the first-segment guard
// holds through the real read path: a missing first article fails even with
// zero-fill enabled.
func TestZeroFill_FirstSegmentNotFilledEndToEnd(t *testing.T) {
	ctx := context.Background()
	const n, segSize = 4, 16
	rg := buildEagerRange(ctx, t, n, segSize)

	fp := fakepool.New()
	fp.SetDefaultBehavior(fakepool.SegmentBehavior{Bytes: bytes.Repeat([]byte{0xAB}, segSize)})
	fp.SetBehavior(segments.MessageID(0), fakepool.SegmentBehavior{Err: nntppool.ErrArticleNotFound})

	ur := newZeroFillReader(t, ctx, fp, rg, ZeroFillOptions{Enabled: true, MaxSegments: 20, MaxFraction: 0.9})

	if _, err := io.ReadAll(ur); err == nil || !errors.Is(err, nntppool.ErrArticleNotFound) {
		t.Fatalf("missing first segment must fail even with zero-fill on, got: %v", err)
	}
}

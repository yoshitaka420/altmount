package usenet

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/javi11/altmount/internal/testsupport/fakepool"
	"github.com/javi11/altmount/internal/testsupport/segments"
	"github.com/javi11/nntppool/v4"
)

// usenet_reader_missing_flag_test.go pins the DataCorruptionError.Missing
// classification used by the streaming health path to decide between a
// tolerance-aware re-check (missing article / 430) and a direct condemn
// (content/yEnc corruption).

// TestDataCorruptionError_Missing_TrueOnArticleNotFound: a missing article
// (NNTP 430 / ErrArticleNotFound) must surface as DataCorruptionError{Missing:true}.
func TestDataCorruptionError_Missing_TrueOnArticleNotFound(t *testing.T) {
	t.Parallel()
	const (
		segCount    = 1
		segSize     = 16
		maxPrefetch = 1
	)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fp := fakepool.New()
	fp.SetBehavior(segments.MessageID(0), fakepool.SegmentBehavior{
		Err: nntppool.ErrArticleNotFound,
	})

	rg := buildEagerRange(ctx, t, segCount, segSize)
	ur := newReaderForTest(t, ctx, fp, rg, maxPrefetch)
	ur.Start()

	_, err := io.ReadAll(ur)

	var dce *DataCorruptionError
	if !errors.As(err, &dce) {
		t.Fatalf("expected *DataCorruptionError, got %T: %v", err, err)
	}
	if !dce.Missing {
		t.Errorf("ErrArticleNotFound must set Missing=true (430 → tolerance-aware recheck), got Missing=false")
	}
}

// TestDataCorruptionError_Missing_FalseOnContentCorruption: content corruption
// (the article exists but decoding fails — "data corruption detected") must
// surface as DataCorruptionError{Missing:false} so it is condemned directly and
// never routed through an availability re-check that would wrongly pass it.
func TestDataCorruptionError_Missing_FalseOnContentCorruption(t *testing.T) {
	t.Parallel()
	const (
		segCount    = 1
		segSize     = 16
		maxPrefetch = 1
	)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fp := fakepool.New()
	// The reader classifies content corruption by the substring "data corruption
	// detected" in the download error (see downloadSegmentWithRetry).
	fp.SetBehavior(segments.MessageID(0), fakepool.SegmentBehavior{
		Err: errors.New("yenc: data corruption detected (crc mismatch)"),
	})

	rg := buildEagerRange(ctx, t, segCount, segSize)
	ur := newReaderForTest(t, ctx, fp, rg, maxPrefetch)
	ur.Start()

	_, err := io.ReadAll(ur)

	var dce *DataCorruptionError
	if !errors.As(err, &dce) {
		t.Fatalf("expected *DataCorruptionError, got %T: %v", err, err)
	}
	if dce.Missing {
		t.Errorf("content corruption must set Missing=false (condemn directly), got Missing=true")
	}
}

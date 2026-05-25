package usenet

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/javi11/altmount/internal/testsupport/fakepool"
	"github.com/javi11/altmount/internal/testsupport/segments"
	"github.com/javi11/nntppool/v4"
)

// usenet_reader_integrity_test.go pins the segment-integrity contract added to
// downloadSegmentWithRetry. nntppool verifies the yEnc pcrc32 trailer when an
// article carries one, but it never checks the decoded length, so a truncated
// or desynchronised response (the field-observed symptom of aggressive
// pipelining, inflight_requests > 1) can return short data with a nil error.
// The reader must reject such a segment rather than stream corrupt bytes.

// TestIntegrity_LengthMismatch_RejectsAndRetries pins that a body whose decoded
// length disagrees with its own declared yEnc PartSize is treated as retryable
// corruption: the segment is retried (Attempts=2) and the read ultimately fails
// instead of returning the short payload.
func TestIntegrity_LengthMismatch_RejectsAndRetries(t *testing.T) {
	t.Parallel()
	const segSize = 64
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fp := fakepool.New()
	// Decoded payload is segSize bytes, but the article declares a larger
	// PartSize — i.e. the response was truncated on the wire.
	fp.SetBehavior(segments.MessageID(0), fakepool.SegmentBehavior{
		Bytes:    segments.Payload(0, segSize),
		PartSize: int64(segSize + 1),
	})

	rg := buildEagerRange(ctx, t, 1, segSize)
	ur := newReaderForTest(t, ctx, fp, rg, 1)
	ur.Start()

	if _, err := io.ReadAll(ur); err == nil {
		t.Fatal("expected read to fail on yEnc length mismatch, got nil error")
	}

	// retry.Attempts(2): the bad segment must be tried exactly twice, never
	// silently accepted on the first short read.
	if got := fp.PerMessageCalls(segments.MessageID(0)); got != 2 {
		t.Errorf("segment 0 issued %d BodyPriority calls, want 2 (one retry on corruption)", got)
	}
}

// TestIntegrity_CRCMismatch_RejectsAndRetries pins that nntppool.ErrCRCMismatch
// is classified as retryable corruption (not a permanent failure, and not
// silently accepted): the segment is retried and the read fails.
func TestIntegrity_CRCMismatch_RejectsAndRetries(t *testing.T) {
	t.Parallel()
	const segSize = 64
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fp := fakepool.New()
	fp.SetBehavior(segments.MessageID(0), fakepool.SegmentBehavior{
		Err: nntppool.ErrCRCMismatch,
	})

	rg := buildEagerRange(ctx, t, 1, segSize)
	ur := newReaderForTest(t, ctx, fp, rg, 1)
	ur.Start()

	if _, err := io.ReadAll(ur); err == nil {
		t.Fatal("expected read to fail on yEnc CRC mismatch, got nil error")
	}

	if got := fp.PerMessageCalls(segments.MessageID(0)); got != 2 {
		t.Errorf("segment 0 issued %d BodyPriority calls, want 2 (one retry on CRC mismatch)", got)
	}
}

// TestIntegrity_ValidLength_Passes pins the negative: a body whose decoded
// length matches its declared PartSize streams through unmodified in a single
// call. This guards against the integrity check false-positiving on correct
// articles.
func TestIntegrity_ValidLength_Passes(t *testing.T) {
	t.Parallel()
	const segSize = 64
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	want := segments.Payload(0, segSize)
	fp := fakepool.New()
	fp.SetBehavior(segments.MessageID(0), fakepool.SegmentBehavior{
		Bytes:    want,
		PartSize: int64(segSize),
	})

	rg := buildEagerRange(ctx, t, 1, segSize)
	ur := newReaderForTest(t, ctx, fp, rg, 1)
	ur.Start()

	got, err := io.ReadAll(ur)
	if err != nil {
		t.Fatalf("read failed on a valid segment: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("payload mismatch: got %d bytes, want %d bytes", len(got), len(want))
	}
	if c := fp.PerMessageCalls(segments.MessageID(0)); c != 1 {
		t.Errorf("segment 0 issued %d BodyPriority calls, want 1 (no retry on valid data)", c)
	}
}

package usenet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/avast/retry-go/v4"
	"github.com/javi11/altmount/internal/pool"
	"github.com/javi11/altmount/internal/slogutil"
	"github.com/javi11/nntppool/v4"
)

const (
	defaultMaxPrefetch = 60 // Default to 60 segments prefetched ahead
)

var (
	_ io.ReadCloser = &UsenetReader{}
)

type MetricsTracker interface {
	IncArticlesDownloaded()
	IncArticlesPosted()
	UpdateDownloadProgress(id string, bytesDownloaded int64)
}

// SegmentStore is an optional cache for decoded segment data.
// Implementations must be safe for concurrent use.
type SegmentStore interface {
	Get(messageID string) ([]byte, bool)
	Put(messageID string, data []byte) error
}

type DataCorruptionError struct {
	UnderlyingErr error
	BytesRead     int64
	NoRetry       bool
}

func (e *DataCorruptionError) Error() string {
	return e.UnderlyingErr.Error()
}

func (e *DataCorruptionError) Unwrap() error {
	return e.UnderlyingErr
}

// ZeroFillOptions configures mid-stream zero-fill substitution for permanently
// missing segments (nntppool.ErrArticleNotFound) on a streaming reader. When a
// single isolated segment in the middle of an otherwise-healthy file is gone
// during playback, a correctly-sized buffer of zeros is returned in its place so
// the player skips a fraction of a second of corrupt audio/video and keeps going
// instead of failing the whole stream. The zero value is disabled — zero-fill
// only happens when Enabled is true and the reader is built WithZeroFill.
type ZeroFillOptions struct {
	// Enabled is the master switch for mid-stream zero-fill.
	Enabled bool
	// MaxSegments caps how many segments may be zero-filled in one stream.
	// Past it, the read fails normally and flows into failure-masking. <= 0
	// disables the count cap (not recommended).
	MaxSegments int
	// MaxFraction caps zero-filled bytes as a fraction of the streamed range.
	// Scales tolerance to size so a genuinely-broken release still fails. <= 0
	// disables the fraction cap (not recommended).
	MaxFraction float64
}

// ReaderOption customizes a UsenetReader at construction time.
type ReaderOption func(*UsenetReader)

// WithZeroFill enables mid-stream zero-fill on the reader using the supplied
// options. Intended only for plain (unencrypted, non-nested) streaming readers;
// import/PAR2 and encrypted/nested readers must fail honestly on a missing
// segment and must not use this option (a zeroed block corrupts chained
// decryption beyond the hole).
func WithZeroFill(o ZeroFillOptions) ReaderOption {
	return func(r *UsenetReader) {
		r.zeroFill = o
	}
}

type UsenetReader struct {
	log            *slog.Logger
	wg             sync.WaitGroup
	ctx            context.Context // Reader's context for cancellation
	cancel         context.CancelFunc
	rg             *segmentRange
	maxPrefetch    int // Maximum segments prefetched ahead of current read position
	init           chan any
	initDownload   sync.Once
	closeOnce      sync.Once
	totalBytesRead int64
	poolGetter     func() (pool.NntpClient, error) // Dynamic pool getter
	metricsTracker MetricsTracker
	streamID       string
	segmentStore   SegmentStore // optional, nil = no caching
	cond           *sync.Cond   // Signals downloadManager when reader advances

	// Prefetch-based download tracking
	nextToDownload int // Index of next segment to schedule

	// Tracing counters (atomic, no lock needed)
	inFlight atomic.Int32 // goroutines actively downloading right now

	// Zero-fill state (streaming resilience). The counters are atomic so the
	// check-then-increment in maybeZeroFill stays lock-free in download
	// goroutines; the budget is a soft bound (a few extra fills under
	// concurrency are harmless).
	zeroFill        ZeroFillOptions
	zeroFilledCount atomic.Int64
	zeroFilledBytes atomic.Int64

	mu sync.Mutex
}

func NewUsenetReader(
	ctx context.Context,
	poolGetter func() (pool.NntpClient, error),
	rg *segmentRange,
	maxPrefetch int,
	metricsTracker MetricsTracker,
	streamID string,
	segmentStore SegmentStore,
	opts ...ReaderOption,
) (*UsenetReader, error) {
	log := slog.Default().With("component", "usenet-reader")
	ctx, cancel := context.WithCancel(ctx)

	if maxPrefetch <= 0 {
		maxPrefetch = defaultMaxPrefetch
	}

	ur := &UsenetReader{
		log:            log,
		ctx:            ctx,
		cancel:         cancel,
		rg:             rg,
		init:           make(chan any, 1),
		maxPrefetch:    maxPrefetch,
		poolGetter:     poolGetter,
		metricsTracker: metricsTracker,
		streamID:       streamID,
		segmentStore:   segmentStore,
	}

	for _, opt := range opts {
		opt(ur)
	}

	ur.cond = sync.NewCond(&ur.mu)

	ur.wg.Go(func() {
		ur.downloadManager(ctx)
	})

	return ur, nil
}

// Start triggers the background download process manually.
// This is useful for pre-fetching data before the first Read call.
func (b *UsenetReader) Start() {
	b.initDownload.Do(func() {
		select {
		case b.init <- struct{}{}:
		default:
		}
	})
}

// Interrupt cancels the reader's context and signals any blocked Read
// to return. Non-blocking and idempotent; safe to call concurrently
// with Read or Close. The caller is still responsible for invoking
// Close to release goroutines and resources. Used by callers (like
// MetadataVirtualFile.Close) that need to abort an in-flight download
// without taking the file's own lock.
func (b *UsenetReader) Interrupt() {
	b.cancel()
	b.cond.Broadcast()
	b.mu.Lock()
	rg := b.rg
	b.mu.Unlock()
	if rg != nil {
		rg.CloseSegments()
	}
}

func (b *UsenetReader) Close() error {
	b.closeOnce.Do(func() {
		b.cancel()

		// Unblock downloadManager if it's waiting on the cond
		b.cond.Broadcast()

		// Unblock any pending reads waiting for data
		if b.rg != nil {
			b.rg.CloseSegments()
		}

		// Wait for goroutines with timeout. The cancel() above ensures all
		// goroutines will eventually terminate, so the waiter goroutine is
		// not a permanent leak — it cleans up once downloads finish.
		// A periodic Broadcast pokes goroutines that entered cond.Wait()
		// after the initial Broadcast above.
		done := make(chan struct{})
		go func() {
			b.wg.Wait()
			close(done)
		}()

		deadline := time.NewTimer(30 * time.Second)
		defer deadline.Stop()
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()

	loop:
		for {
			select {
			case <-done:
				break loop
			case <-deadline.C:
				b.log.WarnContext(b.ctx, "Timeout waiting for downloads to complete during close")
				break loop
			case <-ticker.C:
				b.cond.Broadcast()
			}
		}

		b.mu.Lock()
		if b.rg != nil {
			_ = b.rg.Clear()
			b.rg = nil
		}
		b.mu.Unlock()

		// Final wake for any goroutines that entered cond.Wait() after the loop
		b.cond.Broadcast()
	})

	return nil
}

// Read reads len(p) byte from the Buffer starting at the current offset.
// It returns the number of bytes read and an error if any.
// Returns io.EOF error if pointer is at the end of the Buffer.
func (b *UsenetReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	b.initDownload.Do(func() {
		select {
		case b.init <- struct{}{}:
		default:
		}
	})

	b.mu.Lock()
	rg := b.rg
	b.mu.Unlock()

	if rg == nil {
		return 0, io.ErrClosedPipe
	}

	s, err := rg.Get()
	if err != nil {
		b.mu.Lock()
		totalRead := b.totalBytesRead
		b.mu.Unlock()

		if b.isArticleNotFoundError(err) {
			if totalRead > 0 {
				return 0, &DataCorruptionError{
					UnderlyingErr: err,
					BytesRead:     totalRead,
				}
			} else {
				return 0, &DataCorruptionError{
					UnderlyingErr: err,
					BytesRead:     0,
				}
			}
		}
		return 0, io.EOF
	}

	n := 0
	for n < len(p) {
		nn, err := s.GetReaderContext(b.ctx).Read(p[n:])
		n += nn

		b.mu.Lock()
		b.totalBytesRead += int64(nn)
		totalRead := b.totalBytesRead
		b.mu.Unlock()

		if err != nil {
			if errors.Is(err, io.EOF) {
				// Segment fully read — move to next segment
				b.mu.Lock()
				rg := b.rg
				b.mu.Unlock()

				if rg == nil {
					return n, io.ErrClosedPipe
				}

				s, err = rg.Next()
				if err == nil {
					// Wake download manager — room for more prefetch
					b.cond.Signal()
				}

				if err != nil {
					if n > 0 {
						return n, nil
					}

					if b.isArticleNotFoundError(err) {
						if totalRead > 0 {
							return n, &DataCorruptionError{
								UnderlyingErr: err,
								BytesRead:     totalRead,
							}
						}
					}
					return n, io.EOF
				}
			} else {
				if b.isArticleNotFoundError(err) {
					return n, &DataCorruptionError{
						UnderlyingErr: err,
						BytesRead:     totalRead,
					}
				}
				return n, err
			}
		}
	}

	return n, nil
}

// isArticleNotFoundError checks if the error indicates articles were not found in providers
func (b *UsenetReader) isArticleNotFoundError(err error) bool {
	return errors.Is(err, nntppool.ErrArticleNotFound)
}

func (b *UsenetReader) GetBufferedOffset() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.rg == nil {
		return 0
	}

	if b.nextToDownload == 0 {
		return 0
	}

	idx := b.nextToDownload - 1
	s, err := b.rg.GetSegment(idx)
	if err != nil || s == nil {
		return 0
	}
	return s.Start + int64(s.SegmentSize)
}

// downloadSegmentWithRetry attempts to download a segment with retry logic for pool unavailability.
// segIdx is the range-local index of the segment, used by maybeZeroFill to apply
// the first-segment guard.
func (b *UsenetReader) downloadSegmentWithRetry(ctx context.Context, seg *segment, segIdx int) ([]byte, error) {
	// Cache HIT: skip NNTP entirely
	if b.segmentStore != nil {
		if data, ok := b.segmentStore.Get(seg.Id); ok {
			b.log.DebugContext(ctx, "segment cache hit",
				"segment_id", seg.Id,
				"size_bytes", len(data),
			)
			return data, nil
		}
	}

	// Fix B: hoist pool getter outside retry loop — pool errors are not retriable
	// per-download-attempt; if the pool is unavailable we fail fast.
	poolGetStart := time.Now()
	cp, poolErr := b.poolGetter()
	poolGetDur := time.Since(poolGetStart)
	if poolErr != nil {
		b.log.DebugContext(ctx, "pool get failed",
			"segment_id", seg.Id,
			"pool_get_dur", poolGetDur,
			"error", poolErr,
		)
		return nil, poolErr
	}
	if poolGetDur > 100*time.Millisecond {
		b.log.DebugContext(ctx, "slow pool get",
			"segment_id", seg.Id,
			"pool_get_dur", poolGetDur,
		)
	}

	segStart := time.Now()
	var resultBytes []byte
	err := retry.Do(
		func() error {
			// Fix C: reduce per-attempt timeout 30s → 15s to free stuck connections faster
			attemptCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()

			fetchStart := time.Now()
			result, err := cp.BodyPriority(attemptCtx, seg.Id)
			fetchDur := time.Since(fetchStart)
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					b.log.DebugContext(ctx, "segment download timed out after 15s",
						"segment_id", seg.Id,
						"fetch_dur", fetchDur,
					)
				}

				var bytesWritten int64
				if result != nil {
					bytesWritten = int64(result.BytesDecoded)
				}

				if strings.Contains(err.Error(), "data corruption detected") {
					return &DataCorruptionError{
						UnderlyingErr: err,
						BytesRead:     bytesWritten,
					}
				}

				return err
			}

			resultBytes = result.Bytes
			b.metricsTracker.IncArticlesDownloaded()
			b.metricsTracker.UpdateDownloadProgress(b.streamID, int64(len(resultBytes)))

			return nil
		},
		// Retry strategy (post-S1/S3 fix):
		// - ErrArticleNotFound: never retry (article is permanently gone).
		// - DeadlineExceeded: retry immediately, no backoff — a fresh
		//   nntppool connection is available via round-robin.
		// - Other errors: at most one retry (Attempts=2 total wire calls
		//   per failure), with exponential backoff + jitter to break
		//   thundering-herd synchronization across readers. Base=50ms,
		//   max jitter=100ms → first retry delay drawn from [50, 150]ms.
		retry.Attempts(2),
		retry.Delay(50*time.Millisecond),
		retry.MaxJitter(100*time.Millisecond),
		retry.DelayType(func(n uint, err error, config *retry.Config) time.Duration {
			if errors.Is(err, context.DeadlineExceeded) {
				return 0
			}
			return retry.CombineDelay(retry.BackOffDelay, retry.RandomDelay)(n, err, config)
		}),
		retry.RetryIf(func(err error) bool {
			if errors.Is(err, nntppool.ErrArticleNotFound) {
				return false // permanent failure — do not retry
			}
			return true
		}),
		retry.OnRetry(func(n uint, err error) {
			if !errors.Is(err, context.Canceled) && ctx.Err() == nil {
				b.log.DebugContext(ctx, "segment download retry",
					"attempt", n+1,
					"segment_id", seg.Id,
					"error", err,
					"elapsed", time.Since(segStart),
				)
			}
		}),
		retry.Context(ctx),
	)

	// Cache WRITE: tee-write after successful download (fire-and-forget)
	if b.segmentStore != nil && resultBytes != nil && err == nil {
		_ = b.segmentStore.Put(seg.Id, resultBytes)
	}

	if errors.Is(err, nntppool.ErrArticleNotFound) {
		// Streaming resilience: substitute zeros for an isolated, permanently
		// missing segment so the player skips a fraction of a second instead of
		// failing the whole stream. The fill is never cached (only successful
		// downloads above reach segmentStore.Put). Returns honestly on the first
		// segment, past the budget, or when zero-fill is disabled.
		if fill, ok := b.maybeZeroFill(ctx, seg, segIdx); ok {
			return fill, nil
		}
		b.log.DebugContext(ctx, "missing segment",
			"segment_id", seg.Id,
		)
	}

	return resultBytes, err
}

// zeroFillSegmentLen returns the exact byte length a zero-fill buffer must have
// to stand in for a missing segment. The consumer (segment.GetReaderContext)
// seeks to seg.Start then reads seg.End-seg.Start+1 bytes, so the buffer must be
// at least End+1 bytes long; it must also be at least the physical segment size
// so downstream offset math is identical to a real decoded article. Returning a
// wrong length here would desync every following segment's offsets.
func zeroFillSegmentLen(seg *segment) int64 {
	n := seg.SegmentSize
	if seg.End+1 > n {
		n = seg.End + 1
	}
	return n
}

// rangeLen returns the total byte length of the streamed range, or 0 if unknown.
func (b *UsenetReader) rangeLen() int64 {
	rg := b.rg
	if rg == nil {
		return 0
	}
	n := rg.end - rg.start + 1
	if n < 0 {
		return 0
	}
	return n
}

// maybeZeroFill decides whether a permanently-missing segment may be substituted
// with zeros so the stream survives. segIdx is the range-local index. It returns
// (fill, true) when the substitution is allowed and within budget, or
// (nil, false) when the read must fail honestly. The budget is a soft bound; the
// atomic check-then-increment can let a few extra fills through under
// concurrency, which is harmless.
func (b *UsenetReader) maybeZeroFill(ctx context.Context, seg *segment, segIdx int) ([]byte, bool) {
	if !b.zeroFill.Enabled {
		return nil, false
	}

	// First-segment guard: never zero-fill the file's first article (absolute
	// loader index 0). A missing first segment usually means a DMCA takedown /
	// wrong file and it carries the container header, so fail honestly and let
	// health/import logic react.
	absIdx := segIdx
	if b.rg != nil {
		absIdx = b.rg.loaderIndex(segIdx)
	}
	if absIdx == 0 {
		return nil, false
	}

	fillLen := zeroFillSegmentLen(seg)

	// Per-stream count cap.
	if b.zeroFill.MaxSegments > 0 && b.zeroFilledCount.Load() >= int64(b.zeroFill.MaxSegments) {
		return nil, false
	}

	// Per-stream fraction cap — scales tolerance to the streamed range so a
	// genuinely-broken file still fails and falls into the existing
	// failure-masking → ARR-repair path.
	if b.zeroFill.MaxFraction > 0 {
		if total := b.rangeLen(); total > 0 {
			projected := b.zeroFilledBytes.Load() + fillLen
			if float64(projected) > b.zeroFill.MaxFraction*float64(total) {
				return nil, false
			}
		}
	}

	b.zeroFilledCount.Add(1)
	b.zeroFilledBytes.Add(fillLen)

	b.log.WarnContext(ctx, "zero-filling missing segment to keep stream alive",
		"segment_id", seg.Id,
		"fill_bytes", fillLen,
		"zero_filled_segments", b.zeroFilledCount.Load(),
	)

	return make([]byte, fillLen), true
}

func (b *UsenetReader) downloadManager(ctx context.Context) {
	select {
	case _, ok := <-b.init:
		if !ok {
			return
		}
	case <-ctx.Done():
		return
	}

	if b.rg.Len() == 0 {
		return
	}

	totalSegments := b.rg.Len()

	for ctx.Err() == nil {
		b.mu.Lock()
		if b.rg == nil {
			b.mu.Unlock()
			return
		}

		// Check if all segments have been scheduled
		if b.nextToDownload >= totalSegments {
			b.mu.Unlock()
			break
		}

		// Limit how far ahead we prefetch beyond the current read position
		currentRead := b.rg.GetCurrentIndex()
		ahead := b.nextToDownload - currentRead
		if ahead >= b.maxPrefetch {
			b.cond.Wait()
			b.mu.Unlock()
			if ctx.Err() != nil {
				return
			}
			continue
		}

		// Schedule next segment for download
		idx := b.nextToDownload
		b.nextToDownload++
		b.mu.Unlock()

		seg, err := b.rg.GetSegment(idx)
		if err != nil || seg == nil {
			continue
		}

		b.inFlight.Add(1)
		go func(segIdx int, s *segment) {
			defer b.inFlight.Add(-1)
			defer b.cond.Signal()
			defer func() {
				if p := recover(); p != nil {
					b.log.ErrorContext(ctx, "Panic in download task:", "panic", p)
					s.SetError(fmt.Errorf("panic in download task: %v", p))
				}
			}()

			taskCtx := slogutil.With(ctx, "segment_id", s.Id, "segment_idx", segIdx)
			data, err := b.downloadSegmentWithRetry(taskCtx, s, segIdx)

			if err != nil {
				s.SetError(err)
			} else {
				s.SetData(data)
			}
		}(idx, seg)
	}

}

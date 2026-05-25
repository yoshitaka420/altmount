# Streaming Reliability Plan

Causes and fixes for two field-observed issues:

1. Video corruption when streaming with `inflight_requests: 10` (vs `1`) on NNTP
   providers, using the embedded rclone mount.
2. Higher CPU usage and more NNTP connections when using the native FUSE mount.

---

## Problem 1 — Corruption with `inflight_requests: 10`

### What `inflight=10` does

`config → ProviderConfig.InflightRequests → nntppool.Provider.Inflight` (default 10)
at `internal/config/manager.go:859-870`. With `Inflight > 1`, the client sends up to
N `BODY` commands on one TCP connection before reading any response, then reads the
responses back in order (`docs/docs/3. Configuration/providers.md:88-94`). Pipelining
hides latency and improves throughput.

### Root cause (most → least likely)

1. **No end-to-end integrity check on a decoded segment.** The download path stores
   whatever bytes come back (`internal/usenet/usenet_reader.go:371,396` →
   `SetData` → cache `Put` at `:439`) and never validates them. yEnc parts carry a
   per-part CRC32 trailer (`pcrc32=`); the multipart header also carries `crc32`.
   If the returned bytes are not validated against that checksum, any
   pipelining-induced byte error is silently accepted, cached, and streamed as a
   corrupt video. Pipelining makes provider/proxy errors more frequent — the missing
   checksum is *why* they reach the file.
2. **Provider/middlebox mishandling of pipelined responses.** Some providers or a
   TLS-terminating proxy desync request↔response ordering under depth-10 pipelining,
   delivering response N's bytes for request N±1. The project docs already recommend
   `inflight_requests: 1` as the first fix
   (`docs/docs/5. Troubleshooting/common-issues.md:768-794`).
3. **Connection returned to pool mid-response / state not reset**, so leftover bytes
   bleed into the next command on reuse. Lives inside `nntppool/v4`.
4. **Why "with embedded rclone" specifically:** rclone's VFS issues large sequential
   range reads → high prefetch fan-out (`max_prefetch`, default ~30) × `inflight=10`
   = up to ~300 in-flight `BODY` ops across the pool. Maximum concurrency = maximum
   corruption exposure. rclone's read pattern *amplifies* the underlying issue.

### Fixes (in order)

- **F1 (durable, do first): verify each segment before it is trusted. — DONE.**

  Research findings (nntppool/v4 v4.11.0 source):
  - nntppool **already computes** the yEnc CRC32 during decode and parses the
    `pcrc32`/`crc32` trailer (`reader.go:274,338-345`). `Body`/`BodyPriority`
    return `(*ArticleBody, ErrCRCMismatch)` — body non-nil — **only when**
    `ExpectedCRC != 0 && CRC != ExpectedCRC` (`client.go:206-219`).
  - **Gap:** when the article carries no CRC trailer (`ExpectedCRC == 0`) — or the
    `=yend`/`pcrc32` line was lost to a pipelining desync — nntppool returns the
    bytes with a **nil error**. It parses `EndSize`/`PartSize`/`BytesDecoded` but
    **never compares decoded length** to the declared size (`reader.go`), so a
    truncated/short response is accepted silently and streamed as corruption. This
    is the mechanism behind corruption appearing at `inflight_requests: 10`.
  - Metadata `SegmentSize` is **not** a safe yardstick: it is only rewritten to the
    decoded yEnc `PartSize` by `normalizeSegmentSizesWithYenc`
    (`importer/parser/parser.go:822+`); when normalization is skipped/fails (404s,
    fetch errors) it retains the NZB-reported **encoded** size, so a strict check
    against it would false-positive on every segment.

  Implementation (chosen, false-positive-free):
  - In `downloadSegmentWithRetry` (`internal/usenet/usenet_reader.go`), after a
    successful `BodyPriority`, reject any decode where
    `result.YEnc.PartSize > 0 && len(result.Bytes) != result.YEnc.PartSize`. This
    compares the bytes against the **article's own** declared part length (intrinsic
    to the fetched article, independent of NZB metadata), so it is guaranteed
    correct by the yEnc spec and cannot false-positive on a valid decode.
  - Classify `nntppool.ErrCRCMismatch` explicitly as retryable
    `DataCorruptionError` (it was previously caught only incidentally by the generic
    non-`ArticleNotFound` retry) and log it.
  - Both rejections surface as retryable `DataCorruptionError` → retried via
    round-robin (`retry.Attempts(2)`), and because caching only runs on `err == nil`
    (`usenet_reader.go:439`) a rejected segment is **never cached**.
  - Tests: `internal/usenet/usenet_reader_integrity_test.go`
    (length-mismatch rejects+retries, CRC-mismatch rejects+retries, valid-length
    passes in one call). `fakepool.SegmentBehavior` gained a `PartSize` field.
- **F2 (cheap correctness lever now): change the default to `inflight_requests: 1`**
  (or document loudly) until F1 lands. Trade throughput back via `max_connections`
  per the docs (`common-issues.md:789`). One-line change at `manager.go:861`.
- **F3 (config UX): surface the corruption→pipelining link** in provider config
  help/validation, recommending `1` when users report corruption.
- **F4 (upstream): confirm nntppool/v4 pipelining response-ordering and buffer-reset
  behavior.** If it does not verify `pcrc32`, that is an upstream patch; F1 protects
  us regardless.

---

## Problem 2 — Native FUSE: higher CPU + more connections

### Root cause

1. **Ephemeral reader per non-sequential read.** Sequential reads reuse one
   `UsenetReader`/pool session (`internal/nzbfilesystem/metadata_remote_file.go:1065-1106`).
   Any read where `off != readAtSharedNext` hits the ephemeral path (`:1108-1148`) →
   `createReaderAtOffset` builds a new `UsenetReader`, which spawns its own
   download-manager goroutine (`internal/usenet/usenet_reader.go:112-114`) and
   acquires its own pool slots (`:345`), prefetching up to ~60 segments. The kernel
   issues many small, not-perfectly-sequential reads → connection storms.
2. **No disk-local cache between FUSE and NNTP.** rclone's VFS (`--vfs-cache-mode`)
   absorbs small/repeat reads on local disk; the native FUSE path has no equivalent,
   so every cache miss = NNTP fetch.
3. **128 MB kernel readahead** (`internal/fuse/backend/hanwen/backend.go:65-67`,
   `internal/config/manager.go:786-787`) makes the kernel aggressively populate the
   page cache with hundreds of ~128 KB reads; under eviction these come back as
   scattered offsets → more ephemeral readers.
4. **Per-file mutex serializes reads** (`metadata_remote_file.go:1060-1061`) →
   ephemeral random reads block sequential ones; CPU burned on reader init + GC churn.
5. **Encrypted/nested files skip `randomReadCache`** (`:1159-1238`) → every random
   read rebuilds the full cipher→reader→segment chain.

### Fixes (in order)

- **G1: bound and reuse ephemeral readers.** Keep a small per-file-handle LRU (2-4)
  of open readers keyed by current offset window. A read whose offset falls in/near
  an existing reader's range reuses it instead of constructing a new one. Biggest
  lever. Where: `createReaderAtOffset` call site `:1108-1148`.
- **G2: route FUSE reads through `segcache`** (and `randomReadCache`) for
  encrypted/nested files too, so repeat/adjacent reads hit local cache instead of
  NNTP (`:1159-1238`).
- **G3: tune readahead down for the native mount** (e.g. 16-32 MB), configurable.
  `backend.go:65-67`.
- **G4: shrink the mutex critical section** — hold it only around shared-reader
  bookkeeping, not the whole download. `:1060-1061`.
- **G5: tolerance window for "sequential"** — treat `off` within a small delta of
  `readAtSharedNext` as sequential instead of forking an ephemeral reader.

---

## Suggested sequencing

1. **F1** (segment CRC verify) — fixes corruption at the source, provider-agnostic.
2. **F2** (default `inflight=1`) — immediate relief while F1 is built/validated.
3. **G1 + G2** (reader reuse + cache on FUSE path) — bulk of the CPU/connection win.
4. **G3-G5, F3, F4** — tuning + UX + upstream follow-up.

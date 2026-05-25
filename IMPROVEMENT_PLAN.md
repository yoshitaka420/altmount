# AltMount — Improvement & Fix Plan

> Working document for the `yoshitaka420/altmount` fork (upstream: `javi11/altmount`).
> Generated from a full codebase sweep + research, May 2026.

## What AltMount is

A Go WebDAV + FUSE server that mounts Usenet/NZB content as a read-only virtual
filesystem and **streams** media on demand (no full download). It integrates
Radarr/Sonarr, exposes a SABnzbd-compatible API, embeds rclone, and ships a
React 19 admin UI. ~295 Go files, 76 Go test files, **0 frontend tests**. Both
AltMount and its main rival [nzbdav](https://github.com/nzbdav-dev/nzbdav) are
self-described alpha software.

Priority key: **P0** = correctness/security must-fix · **P1** = high-value
reliability/perf · **P2** = maintainability/testing · **P3** = features.

---

## P0 — Correctness & Security

### 1. "Download complete" reported before the file is accessible
*Issues [#612](https://github.com/javi11/altmount/issues/612),
[#586](https://github.com/javi11/altmount/issues/586).*
The post-processor notifies ARRs after a **hardcoded 1s sleep** for FUSE/VFS
propagation (`internal/importer/postprocessor/coordinator.go:88-99`). Sonarr/Radarr
then try to import a symlink that may not exist yet; removing queue items
spuriously fires Radarr import events.
**Fix:** Replace the fixed sleep with a real readiness check — stat the
symlink/STRM target (or poll rclone VFS) with bounded retry before notifying.
Verify the library item exists (`library.FindLibraryItem`) before triggering a
rescan.

### 2. API keys stored in plaintext  — ⚠️ NOT a drop-in fix (deferred)
`internal/database/user_repository.go:272` stores keys verbatim.
**Correction after implementation attempt:** naive hash-at-rest is *wrong* here
and would break auth across the board. The key is used as a usable credential in
multiple raw-form flows that cannot be reconstructed from a hash:
- `internal/arrs/registrar/manager.go:39` embeds the **stored** key directly in
  the webhook URL (`?apikey=<stored>`) that Sonarr/Radarr send back.
- SABnzbd download clients hold the **raw** key and send it as `?apikey=`.
- Stremio/stream URLs use a **second** token form, `auth.HashAPIKey(raw)`
  (`internal/auth/hash.go`), compared via `subtle.ConstantTimeCompare` against
  `HashAPIKey(stored)` (`internal/api/stream_handler.go:102`,
  `internal/api/stremio_addon_handlers.go:502`).

A single `GetUserByAPIKey` lookup cannot accept both the raw form (webhook /
SABnzbd) and a pre-hashed form, and the raw key needed to (re)register clients
becomes unrecoverable once hashed. Note also that the supposed "non-constant-time
compare" at `api_key_middleware.go:54` is actually a SQL equality lookup, not a
byte compare, and the URL-token paths already use `ConstantTimeCompare`.

**Recommended safer fix:** *encrypt* the `api_key` column at rest (e.g. AES-GCM
with a key derived from `JWT_SECRET`), decrypt on read. This keeps every raw-key
flow working while ensuring a DB-only dump contains no usable keys. Requires
coordinated changes + integration testing across ARR auto-registration, SABnzbd,
stremio, stream, and the frontend "view key" UX — do it as its own tested change,
not blind.

### 3. "Ready" flag set before workers initialized
`cmd/altmount/cmd/serve.go:303` sets ready before health/import/mount subsystems
are up → startup races and requests served against half-initialized state.
**Fix:** Set ready after worker init; gate `/health/ready` on actual subsystem
readiness.

### 4. `context.Background()` in hot paths breaks cancellation
`MetadataVirtualDirectory.Readdir` (`internal/nzbfilesystem/...:665`) and
`RepairCoalescer.flush` (`internal/nzbfilesystem/repair_coalescer.go:170`) ignore
caller/shutdown context. Large directory scans can't be cancelled; refreshes
outlive shutdown.
**Fix:** Thread real context through both.

### 5. Updater has no signature verification
`internal/updater/binary.go` verifies only a SHA-512 checksum, and the checksum
file itself is unauthenticated (trust = HTTPS only).
**Fix:** Sign releases (minisign/cosign), verify signature before applying.
Lower priority for Docker (self-update disabled in containers) but matters for
CLI installs.

---

## P1 — Reliability & Performance (streaming core)

### 6. Download-manager / reader stall risk
`internal/usenet/usenet_reader.go`: if the prefetch window fills
(`maxPrefetch` ≈ 60) and the consumer stops advancing, the manager blocks on
`cond.Wait()` (~line 479); Close relies on 30s-timeout polling (lines 170-186)
and ctx cancellation that may never fire on an external hang → goroutine leak.
**Fix:** Hard deadline + ctx-derived cancellation in the download loop; ensure
Close always drains. Add a leaked-goroutine test (extend the storm/race tests).

### 7. Segment `Release()` vs pending `GetReaderContext()` race
`internal/usenet/segment.go:147` — a released-then-re-requested segment can block
forever on `dataReady` under retry/cache-miss interleaving.
**Fix:** Guard release with a state check; never null out data while a reader is
still waiting.

### 8. Refactor `metadata_remote_file.go` (2092 lines)
`internal/nzbfilesystem/metadata_remote_file.go` mixes reader lifecycle,
encryption wrapping, nested-archive handling, repair, and caching. Highest-traffic,
highest-risk, hardest-to-test file in the repo.
**Fix:** Split into `reader_lifecycle.go`, `encryption.go`, `nested_sources.go`,
`repair.go` — no behavior change, enables targeted tests for #6/#7 and the repair
work below.

### 9. WebDAV doesn't surface corrupted-file errors
`internal/webdav/adapter.go`: `CorruptedFileError` returns a generic 500, so
clients/ARRs can't distinguish corruption from a transient error; MIME is pre-set
to `application/octet-stream` which breaks some clients.
**Fix:** Map corruption to a distinct status; refine content-type by extension.

### 10. Per-byte stream-tracker updates
`internal/nzbfilesystem` updates the tracker on every Read → contention under many
concurrent streams.
**Fix:** Batch/throttle updates (time- or byte-interval based).

### 11. Worker claim serialized under one mutex
`internal/importer/queue/manager.go` (`claimMu`) serializes all claims to dodge
SQLite lock contention, capping import throughput as workers scale.
**Fix:** Single atomic claim (`UPDATE ... WHERE id IN (SELECT ... LIMIT n)
RETURNING`) instead of a global mutex; benchmark with `manager_test`.

---

## P2 — Testing & Maintainability

12. **Zero frontend tests.** Add Vitest + React Testing Library. Start with
    `api/client.ts` (1060 lines — response unwrapping + 401 handling) and the auth
    context.
13. **Untested backend integration points:** `webdav`, `fuse`, `sabnzbd/client`,
    `prowlarr/client`, `stremio/cleanup`, `rclone/mount_service`, `encryption`.
    Prioritize WebDAV+FUSE corrupted-file/MOVE behavior and the SABnzbd API
    contract (compatibility is the migration selling point).
14. **Swallowed errors.** Fix NZB restore-on-compression-failure
    (`internal/importer/service.go:951`) — leaves DB/file state inconsistent.
    Audit ARR health checks (`arrs/service.go:232-252`) and nzbdav metadata
    unmarshal (`scanner/nzbdav.go:279`).
15. **Oversized React components:** `ImportMethods.tsx` (1824),
    `MountConfigSection.tsx` (1352), `RCloneConfigSection.tsx` (1019),
    `QueuePage.tsx` (1071). Extract sub-components.
16. **Frontend resilience:** add an `ErrorBoundary` at layout level (one render
    throw currently crashes the app); add route-level `React.lazy`/code-splitting
    (everything eager-loaded today); drop unused `@tanstack/react-virtual`;
    lazy-load Recharts/cron libs.
17. **Two real backend TODOs:** `internal/api/file_handlers.go` — "implement actual
    availability check" / "available segment count" return stubs. Implement or
    document as unsupported.

---

## P3 — Features (from open issues)

- Stremio uses Sonarr/Radarr **quality profiles** for NZB selection
  ([#568](https://github.com/javi11/altmount/issues/568)) — most-requested.
- GET API to **look up a previously-imported NZB**
  ([#512](https://github.com/javi11/altmount/issues/512)).
- Native **MERGE of `local_dir` + `webdav_mount`** into one `usenet_mount`
  ([#510](https://github.com/javi11/altmount/issues/510)).
- **WebDAV request/response timestamp logging**
  ([#489](https://github.com/javi11/altmount/issues/489)) — cheap observability.
- **Sonarr v4 / Radarr v6 API** support
  ([#241](https://github.com/javi11/altmount/issues/241)).
- **Anti-duplicate uploads** ([#310](https://github.com/javi11/altmount/issues/310))
  and **indexer cart RSS ingestion**
  ([#76](https://github.com/javi11/altmount/issues/76)).
- Finish **Windows SYMLINK import strategy**
  ([#603](https://github.com/javi11/altmount/issues/603),
  [#587](https://github.com/javi11/altmount/issues/587)) — actively broken.

---

## Suggested sequencing

1. **Sprint 1 (stability + trust):** #1, #2, #3, #6.
2. **Sprint 2 (core hardening):** #8 refactor → unlocks #7, #4, #9; add WebDAV/FUSE
   tests (#13). Lay groundwork for repair (Appendix A, Phase A).
3. **Sprint 3 (scale + UX):** #11; frontend tests + ErrorBoundary + code-splitting
   (#12, #16); component breakup (#15).
4. **Sprint 4 (self-healing):** Appendix A, Phases B–C.
5. **Ongoing:** P3 features by demand.

---

# Appendix A — On-the-fly repair of corrupt NZBs while streaming

### Goal
Today, when a segment is unreadable mid-stream, AltMount gives up on the file and
relies on an **ARR re-download of the entire release**. Goal: recover the missing
data *without* re-downloading the whole file, ideally fast enough that the stream
recovers (or self-heals shortly after).

### What already exists (verified in code)
- **Detection** — `DataCorruptionError` and `nntppool.ErrArticleNotFound` are
  raised during reads (`internal/usenet/usenet_reader.go:231,279,290,306,386`)
  and caught in `internal/nzbfilesystem/metadata_remote_file.go:991`.
  Distinguishes *article-not-found* (permanent, no retry) from *yEnc decode
  failure*.
- **Multi-provider tiers** — config has `IsBackupProvider`
  (`internal/config/manager.go:386`) passed to nntppool as `Backup`
  (`:864`). Per-segment provider selection/failover is **delegated entirely to
  the external `nntppool/v4` library**; AltMount has no explicit per-segment
  fallback code.
- **PAR2 is already parsed and stored but never used** — the metadata proto
  defines `Par2FileReference { filename; file_size; repeated SegmentData
  segment_data }` (`internal/metadata/proto/metadata.proto:29-34`, field
  `par2_files = 13`). PAR2 articles are identified at import
  (`internal/importer/multifile/processor.go:57-64`) and their Usenet segments
  recorded — **but nothing consults them during repair.**
- **Segment cache** — `internal/nzbfilesystem/segcache` is keyed by Usenet
  message-ID. This is the natural sink for reconstructed data.
- **Repair coalescer + health worker** — debounce repair triggers and currently
  just mark the file corrupted, move metadata to `corrupted_metadata/`, and
  trigger an ARR rescan (`metadata_remote_file.go:1906-1989`,
  `internal/health/worker.go:951-1034`).

### What's missing
- No segment-level reconstruction (no Reed-Solomon / PAR2 recovery).
- `SegmentData` stores a **single** message-ID — no alternate/redundant IDs and
  no per-segment checksum (`metadata.proto:21-27`).
- `NestedSources` is for archives-within-archives, **not** per-segment redundancy.

### Reality check on "on-the-fly"
PAR2 uses Reed-Solomon over fixed-size blocks. Recovering *k* missing blocks
requires *k* recovery blocks **plus all the surviving data blocks** of the set —
there is no shortcut to rebuild one block in isolation. For a 50 GB release that
means streaming the whole file's blocks through the solver once. So **literal
inline repair during a live seek is impractical for large files.** The realistic
design is layered: cheap inline failover first, PAR2 as a background self-heal.

---

### Phase A — Inline segment failover (cheap, true on-the-fly) — do first
Most "corruption" is a single provider missing an article (takedown/retention),
not actual bit-rot. Backup providers on a different backbone usually have it.

1. **Verify nntppool actually exhausts all providers (incl. backups) before
   returning `ErrArticleNotFound`.** If it short-circuits, add an explicit
   per-segment retry across the backup tier in `downloadSegmentWithRetry`
   (`usenet_reader.go:330`) before declaring corruption. This is the single
   highest-leverage change and needs no schema change.
2. **Alternate message-IDs per segment (schema change).** Add
   `repeated string alternate_ids` to `SegmentData`. Populate from re-posts /
   multiple indexer results (Prowlarr already searches indexers). On miss, try
   alternates before failing. Enables recovery from a *different posting* of the
   same content.

*Outcome:* the stream recovers transparently for the common missing-article case.

---

### Phase B status — engine core IMPLEMENTED & validated ✅
The reconstruction engine now exists as `internal/repair/par2` (MIT, no external
deps), validated against `par2cmdline`-generated fixtures:
- `field.go` — GF(2¹⁶) arithmetic (poly `0x1100B`, generator 2), incl. the
  vectorised `dst ^= c·src` inner loop.
- `packets.go` — PAR2 packet parser (Main / FileDesc / IFSC / RecoverySlice),
  per-packet MD5 verification, multi-volume de-duplication.
- `reconstruct.go` — input-block base constants (`2^logbase`, logbase coprime to
  65535, per the par2cmdline source), RS matrix, Gauss-Jordan solve over GF.
- `engine.go` — `RepairFileSegments`: maps NZB segments (byte ranges +
  message-IDs) ↔ PAR2 slices, fetches surviving segments, reconstructs missing
  ones, and emits decoded bytes to a `Sink`. Scoped to single-file recovery sets
  (a multi-file set needs other files' data, which one virtual file's metadata
  can't supply — returns `ErrMultiFileUnsupported`).

Tests prove: parser structure + per-slice MD5/CRC vs real data; exponent-0
recovery == XOR of inputs; reconstruction of scattered / first / last / max
(missing == recovery count) blocks; a 123-block / 40-recovery fixture; the
segment↔slice pipeline with unaligned segments; and clean failure on
insufficient recovery. All green under `-race`.

**Wiring — DONE ✅** (`internal/repair`): `Service.RepairFile` reads the file's
metadata, fetches + parses its `Par2FileReference` segments, classifies present
vs. missing segments, reconstructs the missing ones, and writes their decoded
bytes to a `Sink`. Adapters: `PoolFetcher` (NNTP pool, priority lane, classifies
`ErrArticleNotFound`) and `cacheSink` (the segment cache — the reader already
checks it before fetching, so recovered segments serve transparently with no
hot-path change). Connections are budgeted via `AcquireImportSlot`.

Trigger: `MetadataVirtualFile.updateFileHealthOnError` now, when a repair service
is wired, runs `selfHealOrFallback` in the background — attempting PAR2
reconstruction first and only marking the file corrupt / triggering an ARR
re-download if recovery is impossible (no PAR2, mismatch, insufficient blocks,
multi-file set). It is **opt-in** via `streaming.par2_repair: true` (requires
`segment_cache.enabled`), so default behaviour is unchanged.

Validated end-to-end against real `par2cmdline` fixtures: the service reads
metadata → fetches/parses PAR2 → reconstructs missing segments → byte-identical
output; `ErrNoPar2` / `ErrPar2Mismatch` / no-op paths covered; success-routing
test confirms the destructive ARR fallback is skipped on heal. (`FileSize`-vs-
PAR2-length guard limits this to single-file recovery sets where segment offsets
line up with PAR2 slices, i.e. non-archive posts; archive/multi-file sets fall
back to ARR.)

**Not yet exercised in a live mount:** the background trigger fires from the real
streaming corruption path, which needs a running provider + FUSE mount to
observe; the reconstruction core and the service orchestration are fully unit-
tested offline.

### Phase B — PAR2-backed background self-heal (the real feature)
Replace "give up → ARR re-download whole release" with "reconstruct only the
missing blocks from PAR2, write them into the segment cache, un-hide the file."

**Trigger:** on `DataCorruptionError`, instead of (only) marking corrupted, enqueue
a coalesced repair job (reuse `RepairCoalescer`). Keep serving the rest of the
file; the failed read still errors, but the file heals shortly after and the next
read/seek succeeds from cache.

**Repair job steps:**
1. Load `par2_files` for the file from metadata; if none, fall back to today's
   ARR-rescan path (PAR2 isn't always posted).
2. Fetch the PAR2 segments via the existing pool (they're just Usenet articles).
   Parse PAR2 packets: Main (block/slice size, file IDs), File Description
   (file MD5, MD5-16k), **Input File Slice Checksums** (MD5+CRC32 per block),
   **Recovery Slices** (parity).
3. Identify damaged blocks by mapping the failed byte range → PAR2 slice indices
   and/or verifying CRC32 per slice.
4. Run Reed-Solomon recovery: stream **all surviving data blocks + enough
   recovery blocks** through the GF(2¹⁶) PAR2 matrix once, accumulating into the
   missing-block outputs. Memory ≈ O(missing_blocks × blocksize); time ≈
   O(total_blocks × missing_blocks) — bounded, single pass, no whole-file buffer.
5. Map reconstructed PAR2 slices back onto the file's NZB segments and **write the
   recovered bytes into `segcache`** keyed by the original message-IDs. Subsequent
   reads hit the cache and succeed.
6. Mark the file healthy again and refresh the VFS (reuse existing coalescing).

**Library choice:** PAR2 needs its specific RS variant (GF(2¹⁶), par2 exponents) —
the fast SIMD `klauspost/reedsolomon` (different matrix) won't drop in. Options:
adapt `github.com/akalin/gopar` (Go par2 verify/repair, MIT) or implement the par2
RS directly. Prototype with gopar's parser to de-risk.

**Guardrails:**
- Skip/limit by size (e.g., only auto-heal files under N GB inline; larger ones
  heal lazily or fall back to ARR).
- Recovery only works if PAR2 overhead ≥ damaged blocks across *all* providers —
  surface "not enough recovery blocks" cleanly and fall back to ARR rescan.
- Cap concurrent repair jobs (reuse the import-admission gate in
  `internal/pool/admission.go`) so healing doesn't starve live streams of NNTP
  connections.

---

### Phase C — Proactive per-segment verification
PAR2 ships MD5+CRC32 per slice. Store a per-segment CRC32 in `SegmentData`
(populated at import from PAR2 slice checksums) so AltMount can detect *silently
corrupt* (decoded-but-wrong) blocks, not just missing ones, and trigger Phase B
before the user notices. Optional/nice-to-have.

---

### Integration map (where each piece lands)
| Concern | Location |
|---|---|
| Detect corruption | `internal/usenet/usenet_reader.go` (exists) |
| Inline backup-provider retry | `usenet_reader.go:downloadSegmentWithRetry` (Phase A) |
| Alt message-IDs / per-segment CRC | `internal/metadata/proto/metadata.proto` + regen (Phase A/C) |
| Repair trigger + coalescing | `internal/nzbfilesystem/repair_coalescer.go` (extend) |
| PAR2 parse + RS reconstruct | new `internal/repair/par2` package (Phase B) |
| Reconstructed-data sink | `internal/nzbfilesystem/segcache` (exists) |
| Connection budgeting | `internal/pool/admission.go` (reuse) |
| Fallback when unrecoverable | existing ARR-rescan path in `internal/health/worker.go` |

### Recommended order
Phase A.1 (provider failover) → A.2 (alt IDs) → B (PAR2 self-heal) → C
(proactive verify). A.1 alone likely fixes the majority of real-world corruption
reports at near-zero cost.

---

## Sources
- [javi11/altmount](https://github.com/javi11/altmount) ·
  [open issues](https://github.com/javi11/altmount/issues)
- [nzbdav](https://github.com/nzbdav-dev/nzbdav) ·
  [AIOStreams Usenet wiki](https://github.com/Viren070/AIOStreams/wiki/Usenet)
- PAR2 / Reed-Solomon:
  [Parchive (Wikipedia)](https://en.wikipedia.org/wiki/Parchive) ·
  [UsenetServer PAR2 KB](https://support.usenetserver.com/kb/article/222-downloading-par2-files/) ·
  [NewsDemon PAR2 repair](https://www.newsdemon.com/usenet-par2-repair)

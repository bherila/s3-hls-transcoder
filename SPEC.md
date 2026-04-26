# SPEC.md — Behavioral specification

This document describes **what the implemented system does** — the contract a user, operator, or integrator can rely on. It is a companion to [PLAN.md](./PLAN.md) (which covers design intent and rationale): SPEC.md tells you _what happens_, PLAN.md tells you _why we chose it_.

If something here disagrees with the code, the code wins and this doc is wrong — file it as a bug.

---

## 1. Inputs

### 1.1 Source bucket

The source bucket is **read-only**. The system performs `HEAD`, `GET`, and `LIST` operations against it; it never issues `PUT`, `DELETE`, or `COPY`.

A source object is considered a video if its key has one of the following extensions (case-insensitive): `.mp4`, `.mov`, `.mkv`, `.webm`, `.avi`, `.m4v`, `.mpg`, `.mpeg`, `.ts`, `.flv`, `.wmv`. Non-video keys are silently skipped during scanning.

If `SOURCE_PREFIX` (single-pair) or `source.prefix` (multi-pair) is set, only keys under that prefix are scanned.

### 1.2 Destination bucket

The destination bucket **must not overlap** with any source bucket. Overlap is defined as: same endpoint host (URL-normalized: lowercased scheme+host) AND same bucket name AND one prefix is a prefix of the other (the empty prefix is a prefix of every prefix). Overlap is detected at startup and refuses to run.

Source-vs-source and dest-vs-dest sharing _is_ allowed.

---

## 2. Destination bucket layout

```
<dest-bucket>/[<dest-prefix>/]
├── .transcoder.lock                                ← global single-runner lock
├── by-id/
│   └── sha256:<64-hex>/                            ← content-addressed output
│       ├── master.m3u8
│       ├── 360p/index.m3u8 + seg_*.m4s + init.mp4
│       ├── 480p/...
│       ├── 720p/...
│       ├── 1080p/...
│       ├── metadata.json
│       └── .processing                             ← per-video lease (transient)
├── mappings/
│   └── <source-key-verbatim>.json                  ← source → contentId pointer
└── fingerprints/
    ├── index.json                                  ← perceptual lookup index
    └── <contentId>.bin                             ← raw fingerprint bytes
```

Key invariants:

- **Content IDs are scheme-prefixed.** v1 emits only `sha256:<hex>`. The directory layout reserves room for future schemes (e.g., `psig:`) without migration.
- **Mapping keys preserve the source path verbatim**, with `mappings/` prefix and `.json` suffix. A source key `videos/2024/intro.mp4` maps to `mappings/videos/2024/intro.mp4.json`.
- The system **never deletes from the source bucket**, even when `CLEANUP_DELETED_SOURCES=true`.

---

## 3. Mapping resolution (client contract)

To play HLS for a source key, a client:

1. Fetches `<dest>/mappings/<source-key>.json`.
2. Reads `hlsRoot` from the JSON.
3. Fetches `<dest>/<hlsRoot>` (the master playlist) and plays it via any HLS player.

Mapping JSON:

```json
{
  "sourceKey": "videos/2024/intro.mp4",
  "sourceEtag": "<etag>",
  "sourceSize": 12345678,
  "sourceLastModified": "2024-01-15T10:30:00Z",
  "contentId": "sha256:f7c3bcc0…",
  "hlsRoot": "by-id/sha256:f7c3bcc0…/master.m3u8",
  "encodedAt": "2026-04-25T12:00:00Z",
  "encoderVersion": "0.1.0"
}
```

`hlsRoot` may point at a different content ID for two source keys whose bytes are identical (byte-hash dedup) or whose video content is perceptually similar (perceptual dedup).

---

## 4. Pipeline (per cron invocation)

Strict sequence. Steps numbered for cross-reference; side effects called out explicitly.

1. **Load + validate config.** Refuses to start on missing required vars or on source/destination overlap (§1.2).

2. **For each bucket pair** (sequentially):
   1. **Acquire global lock** at `<dest>/.transcoder.lock` via conditional PUT (`If-None-Match: *`).
      - Success → proceed.
      - Held by live worker (within `lockTtlSeconds`) → exit cleanly, return.
      - Held but stale (past TTL) → DELETE and re-PUT atomically; if a third worker beat us, return.
   2. **Scan source bucket** with pagination. For each video key, in listing order:
      1. **Mapping cache check.** GET `<dest>/mappings/<source-key>.json`. If it exists with matching `sourceEtag` AND `sourceSize` → skip (counted as `cached`).
      2. **Stream-and-hash.** Single GET of source bytes; in parallel compute SHA-256 of bytes and write to a temp file.
      3. **Byte-hash dedup.** If `<dest>/by-id/sha256:<hash>/master.m3u8` exists → write mapping pointing at it (counted as `deduped`); skip transcode.
      4. **Acquire per-video lease** at `<dest>/by-id/sha256:<hash>/.processing` via conditional PUT.
         - Held → counted as `busy`, skip this video.
      5. **Probe.** Run `ffprobe` to extract resolution, duration, audio presence, bitrate.
      6. **Compute effective ladder.** Filter the configured ladder to rungs at or below source resolution. No upscaling.
      7. **Fingerprint** the source via dHash on keyframes sampled at `fps=1/2`, 9×8 grayscale.
      8. **Perceptual match search.** Read `<dest>/fingerprints/index.json` and compare against entries with comparable frame count (within 0.7 ratio prefilter). Similarity = 1 − meanHammingDist / 64.
         - Best similarity ≥ `PERCEPTUAL_THRESHOLD` (default 0.95):
           - Incoming resolution ≤ matched stored resolution → write mapping pointing at matched contentId; skip transcode (counted as `deduped`).
           - Incoming resolution > matched stored resolution → mark `pendingRepointFrom = matchedContentId`, continue to transcode the higher-quality version.
         - No match → continue.
      9. **Transcode** to HLS using ffmpeg with the effective ladder. Output: per-rung `index.m3u8` + `seg_*.m4s` + `init.mp4`, plus `master.m3u8` referencing all rungs.
      10. **Upload HLS output.** All segments + playlists uploaded under `by-id/<contentId>/`.
      11. **Upload fingerprint** (`fingerprints/<contentId>.bin`) and **upsert** into `fingerprints/index.json`.
      12. **Write metadata.json** (probe results + ladder used + encoder version) and **write mapping** for this source key.
      13. **Repoint, if applicable.** If step 8 set `pendingRepointFrom`:
          - List all mapping keys whose JSON references the old contentId (linear scan over `mappings/`).
          - Rewrite each to point at the new contentId.
          - Delete the old `by-id/<oldId>/` tree, the old `fingerprints/<oldId>.bin`, and remove the old index entry.
      14. **Release per-video lease** (DELETE `.processing`).
      15. Increment `processed` counter.
   3. **Cleanup pass** (only if `CLEANUP_DELETED_SOURCES=true`): see §6.
   4. **Release global lock** (DELETE `.transcoder.lock`).

3. After all pairs complete, return a summary `{ processed, cached, deduped, busy, failed, durationMs }`.

### 4.1 Budget exit

Before each iteration of step 2.2, the worker checks elapsed time against `BUDGET_MULTIPLIER × MAX_RUNTIME_SECONDS`. If exceeded, the loop ends, the lock is released, and the run exits. The next cron tick picks up where this one left off.

### 4.2 Failure handling

- A failure on any single source key is logged, increments `failed`, and the loop continues with the next key. The lease for the failed key is NOT released — it expires after `lockTtlSeconds` (so a crashed transcode is retried by a later run, but a deterministically-broken file isn't retried in a tight loop).
- A failure to acquire the global lock exits cleanly (return; not throw).
- A failure during config load or overlap validation throws and aborts the run.

---

## 5. Deduplication behavior

Two layers, applied in order:

| Layer          | Trigger                                                                   | Cost                            | Action                                                                                           |
| -------------- | ------------------------------------------------------------------------- | ------------------------------- | ------------------------------------------------------------------------------------------------ |
| **Byte-hash**  | SHA-256 of source bytes matches an existing `by-id/sha256:<hash>/` entry  | Always computed during download | Write mapping pointing at existing entry. No transcode.                                          |
| **Perceptual** | dHash similarity ≥ `PERCEPTUAL_THRESHOLD` against an existing fingerprint | One ffmpeg pass per new video   | Quality-aware: equal/lower → reuse; higher → re-encode the new version and repoint old mappings. |

Repoint-on-quality-upgrade (step 4.13) is what makes perceptual dedup safe: a higher-resolution duplicate _replaces_ the stored output and all mappings that previously pointed at the old version are atomically (per-mapping) rewritten before the old `by-id/` is GC'd.

`PERCEPTUAL_DRY_RUN=true` causes step 8 to log would-be matches without acting.

---

## 6. Cleanup pass (CLEANUP_DELETED_SOURCES)

Off by default. When enabled, runs once per bucket pair after all source keys have been processed. Algorithm:

1. **Enumerate live source keys** by re-scanning the source bucket (no extension filter — every key counts as live).
2. **Enumerate mapping keys** under `<dest>/mappings/`.
3. **Compute orphans:** mappings whose decoded source key is NOT in the live set.
4. **Group orphans by `contentId`.**
5. **For each contentId in the orphan set:**
   - Run `findMappingsForContentId(contentId)` to count how many _live_ mappings still point at it.
   - If `liveCount == 0` → delete `by-id/<contentId>/` (recursive), delete `fingerprints/<contentId>.bin`, and remove the index entry.
   - If `liveCount > 0` → leave the transcoded output and fingerprint in place (other live source paths still use it).
6. **Always delete the orphan mapping objects themselves**, regardless of refcount.

`CLEANUP_DRY_RUN=true` causes the cleanup pass to log every action it would take without performing any deletes.

The cleanup pass respects `source.prefix` — it only treats mappings _under_ a pair's source prefix as candidates. This protects shared dest buckets with multiple source pairs.

---

## 7. Concurrency contract

- **At most one runner per (endpoint, dest bucket) pair** is permitted at a time, enforced by the global lock at `.transcoder.lock`.
- **Per-video leases** at `by-id/<contentId>/.processing` are redundant under the v1 single-runner global lock but are written and respected anyway. This keeps the data layout forward-compatible with a future `MAX_CONCURRENCY > 1`.
- **Cron tick collisions are safe.** A late tick that overlaps an in-progress run sees the global lock and exits cleanly without writing anything.
- **Crash recovery** is bounded by `lockTtlSeconds` (default `MAX_RUNTIME × 1.5`). After that window, the next tick reclaims the stale lock.

---

## 8. Time budgets

Three values, all derived from `MAX_RUNTIME_SECONDS`:

| Value                 | Formula                             | Default (Lambda / CF / local) | Purpose                                               |
| --------------------- | ----------------------------------- | ----------------------------- | ----------------------------------------------------- |
| `MAX_RUNTIME_SECONDS` | platform-specific                   | 900 / 3600 / 3600             | Hard ceiling on a single invocation.                  |
| Self-imposed budget   | `MAX_RUNTIME × BUDGET_MULTIPLIER`   | 675 / 2700 / 2700 (×0.75)     | Soft cutoff that ends the per-video loop cleanly.     |
| Lock TTL              | `MAX_RUNTIME × LOCK_TTL_MULTIPLIER` | 1350 / 5400 / 5400 (×1.5)     | Stale-lock cutoff for crash recovery by the next run. |

Budget < runtime < lock-TTL is invariant. Setting multipliers that violate this ordering will cause the lock to be re-claimed before a healthy worker has finished.

---

## 9. ABR ladder rules

- Default ladder: 360p / 480p / 720p / 1080p, H.264 Main + AAC.
- **No upscaling.** Rungs above source resolution are dropped from the effective ladder.
- If the source has no audio, audio rungs are omitted from the ffmpeg variant map (the master playlist still lists the variants without `AUDIO`).
- Codec is fixed at v1: H.264 + AAC. HEVC/AV1 are out of scope (see [FUTURE.md](./FUTURE.md)).
- Override the ladder via the `HLS_LADDER` env var (JSON array of `{name, width, height, videoBitrateKbps, audioBitrateKbps}`).

---

## 10. Configuration contract

### 10.1 Priority

Bucket configuration is loaded in this priority order (first match wins):

1. `BUCKETS_CONFIG_FILE` — path to a JSON file containing an array of pair objects.
2. `BUCKETS_CONFIG` — JSON literal containing an array of pair objects.
3. `SOURCE_*` / `DEST_*` env vars — single-pair fallback.

### 10.2 Credential cascade (per bucket within a pair)

1. Bucket-level `accessKeyId` / `secretAccessKey` / `region` (set on `pair.source` or `pair.dest`).
2. Pair-level `accessKeyId` / `secretAccessKey` / `region` (set at the pair root).
3. Env-level — `SOURCE_ACCESS_KEY_ID` / `SOURCE_SECRET_ACCESS_KEY` / `SOURCE_REGION` for any pair's source bucket; `DEST_*` analogously for the dest side.

A bucket with no resolved credentials at any level fails startup.

### 10.3 Overlap rejection

§1.2 rules. Reject-at-startup, no warning-mode override.

---

## 11. Determinism + idempotency

- **Content ID is deterministic** in source bytes: identical bytes → identical `sha256:<hex>` → same `by-id/` path.
- **Re-running over an unchanged source bucket is a no-op** beyond cache-check GETs (every key hits the mapping cache, step 4.2.2.1).
- **Rerunning a partially-complete prior run** is safe: in-flight per-video leases are respected; stale leases expire and are reclaimed; uploaded segments past a crash are overwritten on retry.
- **Mapping rewrites during repoint are not transactional across mappings** — a crash mid-repoint can leave a mix of old and new pointers. The next run's perceptual-match step detects the inconsistency and re-runs the repoint.

---

## 12. Out of scope (v1)

See [FUTURE.md](./FUTURE.md). Notable explicit non-features:

- DASH manifests (CMAF segments are DASH-compatible if added later).
- HEVC / AV1 codecs.
- Source-bucket event-driven triggering.
- Per-job retry/resume across runs.
- Web UI / status dashboard.
- Auth on playback URLs.
- Subtitles / multi-audio-track passthrough.
- Per-video config overrides.

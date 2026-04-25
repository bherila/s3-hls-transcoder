# PLAN.md

## Goal

Self-hosted video transcoder. Scans an S3-compatible source bucket on a cron schedule and produces HLS streaming output in a destination bucket. Runs on AWS Lambda, Cloudflare Containers, or any local/VPS host (AWS Lightsail recommended). Avoids per-minute pricing of hosted services like Cloudflare Stream or AWS MediaConvert.

> The repo directory is named `s3-hsts-transcoder`. The output format is **HLS** (HTTP Live Streaming) — the "HSTS" in the directory name is a leftover from initial scoping; HSTS is unrelated to streaming.

## Architecture

Shared TypeScript library + three thin platform entrypoints. All entrypoints run the same transcoding pipeline; they differ only in trigger mechanism, environment, runtime cap, and how ffmpeg is provided.

```
.
├── lib/                # Shared transcoding logic — all the real work
├── aws/                # AWS Lambda entrypoint (container image)
├── cloudflare/         # Cloudflare Containers entrypoint
├── local/              # Local / VPS / Lightsail cron entrypoint
├── package.json        # Workspace root, convenience scripts
├── pnpm-workspace.yaml
├── PLAN.md
├── FUTURE.md
└── README.md
```

Entrypoints depend on `lib` via `workspace:*` (pnpm-resolved local link; everything is version-locked to the commit).

## Tech choices

- **Node.js + TypeScript** — universal across all three platforms.
- **pnpm workspaces** for monorepo management.
- **Native ffmpeg binary** (not ffmpeg.wasm — see Decision log).
  - Lambda: container image with ffmpeg apt-installed.
  - CF Containers: same Dockerfile pattern.
  - Local: relies on system ffmpeg on PATH.
- **AWS SDK v3 (`@aws-sdk/client-s3`)** — works uniformly against R2, S3, MinIO, etc.
- **HLS with fMP4/CMAF segments** — universal device support; DASH-compatible later if needed.
- **Vitest** for tests.

## Default ABR ladder

H.264 (Main profile) + AAC, four rungs. Skips rungs above source resolution (no upscaling).

| Rung  | Resolution | Video bitrate | Audio bitrate |
|-------|------------|---------------|---------------|
| 360p  | 640×360    | 800 kbps      | 96 kbps       |
| 480p  | 854×480    | 1400 kbps     | 128 kbps      |
| 720p  | 1280×720   | 2800 kbps     | 128 kbps      |
| 1080p | 1920×1080  | 5000 kbps     | 192 kbps      |

Configurable via `HLS_LADDER` env var (JSON array).

## Bucket layout

Source bucket (read-only — we never modify or delete):

```
videos_source/
  ...arbitrary tree of source video files...
```

Destination bucket:

```
videos_hsts/
  .transcoder.lock                  ← global single-runner lock
  by-id/
    sha256:<hash>/                  ← v1 byte-hash content IDs
      master.m3u8
      360p/index.m3u8, seg_00001.m4s, ...
      480p/...
      720p/...
      1080p/...
      metadata.json
      .processing                   ← per-video lease (deleted on success)
  mappings/
    <full-source-path>.json         ← preserves source dir structure
  fingerprints/
    <id>.bin                        ← MPEG-7 video signatures
    index.json                      ← lookup index for similarity search
```

The `by-id/` directory + `<scheme>:<id>` content IDs leave room for future identifier types (e.g., `psig:` for stronger perceptual matches) without schema migration.

## Mapping file format

```json
{
  "sourceKey": "marketing/intro-2024.mp4",
  "sourceEtag": "...",
  "sourceSize": 12345678,
  "sourceLastModified": "2024-01-15T10:30:00Z",
  "contentId": "sha256:f7c3bcc0...",
  "hlsRoot": "by-id/sha256:f7c3bcc0.../master.m3u8",
  "encodedAt": "2026-04-25T...",
  "encoderVersion": "0.1.0"
}
```

Client lookup: `GET <bucket>/mappings/<source-path>.json` → read `hlsRoot` → fetch `master.m3u8` → play.

## Transcoding pipeline

For each cron invocation:

1. **Acquire global lock** at `videos_hsts/.transcoder.lock` (atomic conditional PUT). If held by a live worker, exit. If stale, claim it.
2. List `videos_source/` (paginated).
3. For each source key, in order, while runtime budget remains:
   1. **HEAD source** → ETag, size, last-modified.
   2. **Mapping cache check**: if `mappings/<source-path>.json` exists with matching ETag+size, **skip** (already processed, fast path).
   3. **Stream source once**, computing in parallel: SHA-256 of bytes, MPEG-7 video signature.
   4. **Byte-hash dedup**: if `by-id/sha256:<hash>/master.m3u8` exists, write mapping pointing at it. Done.
   5. **Perceptual dedup**: load fingerprint index, compare against existing signatures within `PERCEPTUAL_THRESHOLD`:
      - No match → step 6.
      - Match found → compare quality (resolution, then bitrate) of incoming vs. stored:
        - Incoming ≤ stored → write mapping pointing at existing entry, no transcode.
        - Incoming > stored → re-transcode from incoming, repoint all mappings referencing the matched entry, GC old transcoded output.
   6. **Acquire per-video lease** at `by-id/<id>/.processing`.
   7. **Transcode** to ABR ladder via ffmpeg. Output fMP4 segments + per-resolution playlists + master playlist.
   8. **Write** `metadata.json` + fingerprint file + mapping file. **Release per-video lease.**
4. **Release global lock**, exit.

## Locking

Two layers.

### Global single-runner lock

Object key: `videos_hsts/.transcoder.lock`.

Acquisition uses S3 conditional PUT with `If-None-Match: *` (atomic; supported by R2 and modern S3 since Nov 2024).

Lock contents:

```json
{
  "workerId": "<uuid>",
  "platform": "aws-lambda" | "cloudflare-container" | "local",
  "hostname": "...",
  "startedAt": "2026-04-25T10:00:00Z",
  "expectedEndBy": "2026-04-25T10:15:00Z",
  "lockTtlSeconds": 1350
}
```

Three time values, distinct on purpose:

| Value                   | Default                  | Purpose                                                                                  |
|-------------------------|--------------------------|------------------------------------------------------------------------------------------|
| `MAX_RUNTIME_SECONDS`   | platform-specific        | Hard ceiling on this invocation. Lambda: 900. CF/local: 3600.                            |
| Self-imposed budget     | `MAX_RUNTIME × 0.75`     | Checked before each video. If exceeded, finish in-flight, release lock, exit.            |
| Lock TTL                | `MAX_RUNTIME × 1.5`      | Stale-lock cutoff for crash recovery. Future runs respect lock until TTL passes.         |

**Startup flow**:
1. Conditional PUT new lock. Success → start work.
2. Failure (lock exists) → GET lock, check `(now - startedAt)` against its `lockTtlSeconds`.
3. Within TTL → exit immediately (no-op cron run).
4. Past TTL → DELETE then conditional PUT (atomic; if a third worker beat us, our PUT fails, we exit).

**Shutdown flow**:
- Graceful (budget elapsed): DELETE lock, exit.
- Crash: lock left to expire after TTL.

### Per-video lease

Object key: `by-id/<id>/.processing`. Same conditional-PUT pattern.

Redundant under the v1 single-runner global lock, but kept because it lets us safely raise `MAX_CONCURRENCY` later without changing data structures.

## Platform runtime limits

| Platform                  | Max runtime per invocation                                                                                        | Lock design impact                                          |
|---------------------------|-------------------------------------------------------------------------------------------------------------------|-------------------------------------------------------------|
| **AWS Lambda**            | 900s (15 min) hard cap.                                                                                           | `MAX_RUNTIME=900`, budget 675s, lock TTL 1350s              |
| **Cloudflare Containers** | No documented hard cap. 15-min `SIGTERM`→`SIGKILL` grace on shutdown. Configurable idle timeout via `sleepAfter`. | `MAX_RUNTIME=3600` default (configurable)                   |
| **Local / Lightsail**     | None (we control it).                                                                                             | `MAX_RUNTIME=3600` default (configurable)                   |

Sources: [AWS Lambda quotas](https://docs.aws.amazon.com/lambda/latest/dg/gettingstarted-limits.html), [Cloudflare Containers limits](https://developers.cloudflare.com/containers/platform-details/limits/).

## Configuration (env vars)

Bucket pairs are configured one of three ways, in priority order:

1. `BUCKETS_CONFIG_FILE` — path to a JSON file with an array of pairs.
2. `BUCKETS_CONFIG` — JSON literal with an array of pairs.
3. `SOURCE_*` / `DEST_*` env vars — convenient single-pair fallback.

Each pair JSON object:

```jsonc
{
  "source": {
    "bucket": "videos_source",
    "endpoint": "https://<account>.r2.cloudflarestorage.com",
    "prefix": "uploads/",                // optional
    "accessKeyId":     "...",            // bucket-level credentials (optional)
    "secretAccessKey": "...",
    "region":          "auto"            // optional
  },
  "dest":   { "bucket": "videos_hsts",   "endpoint": "..." },
  "accessKeyId":     "...",              // pair-level credentials (optional)
  "secretAccessKey": "...",
  "region":          "auto"
}
```

**Credential cascade per bucket:** `bucket.accessKeyId` → `pair.accessKeyId` → env var (`SOURCE_ACCESS_KEY_ID` for source side, `DEST_ACCESS_KEY_ID` for dest side). Same for `secretAccessKey` and `region`.

**Overlap validation:** startup refuses to run if any source bucket overlaps with any destination bucket. Two buckets overlap when they share endpoint + bucket name AND one's prefix is a prefix of the other (including the empty prefix). Different endpoints with the same bucket name are *not* overlap. Source-vs-source and dest-vs-dest are intentionally allowed.

| Var                        | Required | Default          | Description                                                              |
|----------------------------|:--------:|------------------|--------------------------------------------------------------------------|
| `BUCKETS_CONFIG_FILE`      | no       | —                | Path to JSON file containing pair array                                  |
| `BUCKETS_CONFIG`           | no       | —                | JSON literal containing pair array                                       |
| `SOURCE_BUCKET`            | †        | —                | Source bucket name (single-pair fallback)                                |
| `SOURCE_ENDPOINT`          | †        | —                | S3 endpoint URL (R2: `https://<account>.r2.cloudflarestorage.com`)       |
| `SOURCE_ACCESS_KEY_ID`     | †        | —                | Also acts as env-level fallback in BUCKETS_CONFIG                        |
| `SOURCE_SECRET_ACCESS_KEY` | †        | —                | Also env-level fallback                                                  |
| `SOURCE_REGION`            | no       | `auto`           |                                                                          |
| `SOURCE_PREFIX`            | no       | ``               | Limit scan to this prefix (single-pair fallback only)                    |
| `DEST_BUCKET`              | †        | —                |                                                                          |
| `DEST_ENDPOINT`            | †        | —                |                                                                          |
| `DEST_ACCESS_KEY_ID`       | †        | —                | Env-level fallback                                                       |
| `DEST_SECRET_ACCESS_KEY`   | †        | —                | Env-level fallback                                                       |
| `DEST_REGION`              | no       | `auto`           |                                                                          |
| `HLS_LADDER`               | no       | (built-in)       | JSON array overriding ABR ladder                                         |
| `MAX_RUNTIME_SECONDS`      | no       | platform default | Self-imposed runtime budget ceiling                                      |
| `LOCK_TTL_MULTIPLIER`      | no       | `1.5`            | Lock TTL = `MAX_RUNTIME × this`                                          |
| `BUDGET_MULTIPLIER`        | no       | `0.75`           | Budget = `MAX_RUNTIME × this`                                            |
| `PERCEPTUAL_THRESHOLD`     | no       | `0.95`           | Similarity score required for dedup match                                |
| `PERCEPTUAL_DRY_RUN`       | no       | `false`          | If `true`, log would-be merges instead of acting                         |
| `CLEANUP_DELETED_SOURCES`  | no       | `false`          | If `true`, run a refcount-aware orphan-mapping GC pass each invocation   |
| `CLEANUP_DRY_RUN`          | no       | `false`          | If `true`, cleanup pass logs without deleting                            |
| `MAX_CONCURRENCY`          | no       | `1`              | Source files processed in parallel within one run                        |
| `LOG_LEVEL`                | no       | `info`           | `debug` / `info` / `warn` / `error`                                      |

† Required when neither `BUCKETS_CONFIG_FILE` nor `BUCKETS_CONFIG` is set.

`.env.sample` (in `local/`) documents the full set with examples.

## Per-entrypoint specifics

### `local/`

- `pnpm start` runs one transcoding pass and exits.
- Cron entry example: `*/15 * * * * cd /opt/transcoder/local && pnpm start >> /var/log/transcoder.log 2>&1`
- ffmpeg via system PATH; startup checks `which ffmpeg`.
- Suitable for AWS Lightsail, Hetzner, DigitalOcean, raw VPS, or even a Mac running launchd/cron.
- See [`local/README.md`](./local/README.md) for AWS Lightsail instance-tier recommendations.

### `aws/`

- AWS Lambda container image (10 GB image limit; ample for ffmpeg).
- Triggered by EventBridge cron rule.
- Streams source from S3 (avoids `/tmp` limits even though it's 10 GB).
- Recommended memory: 3008–10240 MB (more memory ≈ proportionally more vCPU on Lambda).
- Timeout: 900s.
- Dockerfile: `aws/Dockerfile`.

### `cloudflare/`

- Cloudflare Container with real ffmpeg binary.
- Triggered via Worker Cron Trigger that wakes the container's Durable Object.
- R2 bindings if available; falls back to S3-API client for portability.
- Dockerfile: `cloudflare/Dockerfile`.
- The cron Worker stays well under the Worker CPU limit by simply waking the container and exiting; the container does the work.

## Testing

- **Unit tests** in `lib/` for: hash flow, fingerprint comparison, mapping I/O, ladder validation, lock acquisition logic, lease semantics. Vitest.
- **Integration tests** with a local MinIO container (S3-compatible) + a small fixture set of test videos.
- Each entrypoint has its own smoke test for platform-specific glue.
- Root `package.json` script `test` runs all package tests via `pnpm -r test`.

## Out of scope (see [FUTURE.md](./FUTURE.md))

- HEVC / AV1 codecs
- DASH manifest generation alongside HLS
- Source-bucket event-driven (rather than poll) triggering
- Per-job retry / resume across runs
- Web UI / status page
- Auth on playback URLs
- Subtitles / multi-audio-track passthrough
- Per-video config overrides

## Decision log

- **Native ffmpeg over ffmpeg.wasm.** Wasm builds exceed CF Worker 10 MB bundle limit even when minimized; single-threaded performance is unacceptable; 128 MB memory cap is unworkable for ABR transcoding. → CF Containers used instead of CF Workers.
- **Cloudflare Containers over a Worker-as-dispatcher pattern.** Unifies the pipeline across all three platforms (every entrypoint actually transcodes) at the cost of taking a dependency on a real container product.
- **HLS-only with CMAF segments in v1.** Universal support via Safari native + hls.js fallback. CMAF segments are DASH-compatible if dual-protocol becomes needed later.
- **H.264 + AAC only in v1.** HEVC/AV1 storage savings don't justify the encoding-CPU cost when egress is free on R2. FUTURE.md tracks for reconsideration.
- **Byte-hash + MPEG-7 video signature dedup in v1.** ETag is unreliable across multipart upload tools. Audio fingerprinting (Chromaprint) ruled out because many sources are silent.
- **`by-id/` directory + `<scheme>:<id>` content IDs.** Forward-compat for adding perceptual identifiers (`psig:...`) without schema migration.
- **Single-runner global lock + per-video lease.** Belt-and-suspenders. Global lock makes v1 simple; per-video lease is forward-compat for raising concurrency later.
- **Conditional PUT (`If-None-Match: *`) for atomic locking.** Supported by R2 and modern S3. Removes the read-then-write race.
- **Source bucket is read-only.** Never modified or deleted from. We only write to the destination bucket.

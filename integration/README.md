# Integration Tests

End-to-end tests for the HLS transcoder. Each suite spins up a real MinIO
container via [Testcontainers](https://testcontainers.com/guides/getting-started-with-testcontainers-for-nodejs/),
uploads fixture videos generated with `ffmpeg`, runs `runOnce()`, and asserts on
the destination bucket that comes out the other side. Nothing is mocked: this is
the executable form of the [SPEC.md](../SPEC.md) client contract.

## Prerequisites

| Requirement                                                | Notes                                                          |
| ---------------------------------------------------------- | -------------------------------------------------------------- |
| Docker (or compatible runtime)                             | Testcontainers pulls and starts `minio/minio:latest`           |
| `ffmpeg` with `libx264`                                    | Must be on `PATH`, or set `FFMPEG_PATH`                        |
| `ffprobe`                                                  | Usually ships alongside `ffmpeg`; set `FFPROBE_PATH` if needed |
| `pnpm install` run at repo root                            | Fetches `testcontainers` and workspace deps                    |
| `lib` built (`pnpm --filter @s3-hls-transcoder/lib build`) | Integration package imports from `lib/dist`                    |

## Running

```sh
# From the repo root:
INTEGRATION=1 pnpm --filter @s3-hls-transcoder/integration test

# A single suite:
INTEGRATION=1 pnpm --filter @s3-hls-transcoder/integration test cleanup
```

Without `INTEGRATION=1` the suite is skipped and exits cleanly, so the recursive
`pnpm test` at the repo root is unaffected on machines without Docker. CI runs
it in its own `integration` job (see [`../.github/workflows/ci.yml`](../.github/workflows/ci.yml)).

## Fixtures

`src/globalSetup.ts` generates three MP4s once per run and passes their paths to
the tests through env vars:

| Fixture              | Env var           | Shape                           | Why                                                         |
| -------------------- | ----------------- | ------------------------------- | ----------------------------------------------------------- |
| `source.mp4`         | `FIXTURE_SOURCE`  | 640×360 moving test pattern, 4s | Baseline; keeps both rungs of the test ladder               |
| `source-remuxed.mp4` | `FIXTURE_REMUXED` | Stream copy with new metadata   | Identical frames, different bytes → perceptual, not byte    |
| `small.mp4`          | `FIXTURE_SMALL`   | 320×240 colour bars, 4s         | Unrelated content; no rung fits it, so filtering is visible |

The remux is deliberately a stream copy rather than a re-encode: its decoded
frames are bit-identical to the baseline's, so "byte-hash dedup misses,
perceptual match hits" is deterministic instead of depending on encoder noise.

## Suites

### `runOnce.integration.test.ts` — the pipeline and its output layout

1. `runOnce()` processes both fixtures and reports `processed: 2, failed: 0`.
2. `by-id/sha256:<hash>/master.m3u8` exists and is a valid master playlist.
3. `mappings/videos/test.mp4.json` names the content ID (with `sha256:` prefix),
   the owning source bucket, and an `hlsRoot` that resolves to a real object.
4. `.transcoder.lock` is absent after the run — the lock was released.
5. Per-variant playlists and fMP4 segments exist for every rung kept, and rungs
   above the source resolution are skipped (the 320×240 fixture gets `240p`
   only).
6. A fingerprint blob and its `fingerprints/index.json` entry are written.
7. `by-id/<contentId>/metadata.json` records the probe result and the effective
   ladder.
8. A second run reports `cached: 2` — the mapping cache short-circuits it.

### `dedup.integration.test.ts` — both dedup layers

1. The same bytes uploaded at two keys produce one content ID and two mappings
   (`deduped: 1`, no second transcode).
2. A byte-different file with identical frames gets **its own** `by-id/` tree.
3. The perceptual match is logged with `actedUpon: false` — advisory only, per
   SPEC.md §5. It never reuses, repoints, or deletes content.
4. `fingerprints/index.json` holds one entry per content ID, not per source key.
5. A second run re-transcodes nothing.

### `cleanup.integration.test.ts` — refcounted GC of deleted sources

Runs in order, deleting one source at a time:

1. With every source present, cleanup changes nothing.
2. `CLEANUP_DRY_RUN` reports without deleting.
3. Deleting one of two sources that share a content ID deletes only the orphan
   mapping; the output and fingerprint are retained for the survivor.
4. Deleting the last source referencing it GCs `by-id/<contentId>/`, the
   fingerprint blob, and the index entry.
5. An unrelated video is untouched throughout.

## Configuration

| Env var        | Default       | Purpose                          |
| -------------- | ------------- | -------------------------------- |
| `INTEGRATION`  | —             | Set to `1` to enable the suite   |
| `FFMPEG_PATH`  | (PATH lookup) | Override ffmpeg binary location  |
| `FFPROBE_PATH` | (PATH lookup) | Override ffprobe binary location |

MinIO is started on a random ephemeral port, so parallel checkouts don't collide.
Credentials inside tests are MinIO's defaults: `minioadmin` / `minioadmin`.

## Timeouts

Setup hooks allow 5 minutes (image pull on a cold cache, then transcoding the
fixtures) and each test the same, because transcode time dominates. Suites run
serially in a single fork — each brings up its own container, and parallel
containers would multiply Docker resource use for no benefit.

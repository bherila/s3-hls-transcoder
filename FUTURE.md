# FUTURE.md

Out of scope for v1, designed-for but not built.

## HEVC / AV1 codecs

H.264 ships in v1. Reasons to revisit:

- **HEVC**: 30–50% smaller files at the same quality. Encoding ~5–10× slower than H.264. Browser support: Safari yes, Chrome/Firefox patchy historically.
- **AV1**: even better compression. Encoding ~10–100× slower (impractical without hardware encoders). Hardware decode coverage still spotty on older mobile.

Trigger to add: storage cost on R2 becomes meaningful, or egress costs become real. Implementation: extend the ABR ladder schema to specify codec per rung. HLS supports codec-switching via `EXT-X-MEDIA` groups so a multi-codec ladder is feasible.

## DASH manifest generation

CMAF segments produced in v1 are DASH-compatible. Adding DASH = generate a `.mpd` alongside `master.m3u8`, pointing at the same segments. Cost: negligible. Storage cost: zero (no extra segments). Likely added when a non-Apple-ecosystem client surfaces a real need.

## Garbage collection of unreferenced by-id/ directories

The cleanup pass (`CLEANUP_DELETED_SOURCES=true`) handles the common case: when sources are deleted, refcount-aware GC removes `by-id/<id>/` once no live mappings reference it.

What's *still* deferred: a defensive sweep that enumerates `by-id/*/` and removes directories with **zero** mapping references — to recover from corrupted state (interrupted upload that wrote segments but no mapping; manually-deleted mappings; tooling bugs). Implementation:

1. Enumerate `mappings/*.json` → set of referenced content IDs.
2. Enumerate `by-id/*/` directories.
3. Delete directories with zero references (configurable grace period to handle race conditions with in-flight writes).

Useful as a separate `gc` command. Default: dry-run that emits a report to `gc-candidates.json` for human review before destructive action.

## Source-bucket event-driven triggering

Currently cron-poll. For larger or faster-changing source buckets:

- S3: bucket event notifications → SQS → Lambda
- R2: object-created events → CF Queue → Container

Same pipeline, just a different trigger. Worth doing when poll latency or list-bucket cost becomes a problem.

## Per-job retry / resume

A 14-min Lambda transcode that fails at minute 13 currently restarts from scratch on the next run. Resumability:

- Checkpoint per ladder rung (a finished rung doesn't need re-encoding).
- Persist progress to the per-video lease file.

## Authentication / signed playback URLs

v1 assumes the destination bucket is publicly readable (or readable to whoever has the URL). For private content:

- Pre-signed URLs from Lambda/Worker on demand.
- Token-based auth at a CDN edge.

## Web UI

Dashboard listing transcoded videos, queue depth, dedup matches, recent failures. Useful when manually managing larger libraries.

## Per-video config overrides

Currently the ladder is global. Future: a `_config.json` next to a source video to override (e.g., higher quality for marketing videos, lower for screencasts).

## Perceptual dedup tuning UX

v1 ships with a fixed default `PERCEPTUAL_THRESHOLD` and a `PERCEPTUAL_DRY_RUN` switch. Future:

- Periodic "review report" emitted to dest bucket listing recent matches with confidence scores, so the operator can spot-check.
- Per-pair allow/deny list (`fingerprints/overrides.json`) for known-distinct lookalikes (e.g., two takes of the same shot).

## Subtitle / captions support

If sources include WebVTT or embedded subtitles, propagate them into the HLS playlists (`EXT-X-MEDIA:TYPE=SUBTITLES`).

## Audio language tracks

Multi-audio-track sources are flattened to the first track in v1. Future: emit each language as an audio rendition.

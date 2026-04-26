# CLAUDE.md

Project: HLS video transcoder for S3-compatible buckets. Read **[PLAN.md](./PLAN.md)** for architecture and **[SPEC.md](./SPEC.md)** for the implemented behavioral contract; this file is the short version of conventions you'll need.

## Stack

- Node 20+, TypeScript (ESM), pnpm workspaces.
- ffmpeg as a native binary (not wasm). Spawn directly via `child_process`.
- AWS SDK v3 (`@aws-sdk/client-s3`) for both source and destination — works against R2, S3, MinIO.
- Vitest for tests.

## Layout

```
lib/         ← all real logic; exported from a single index
aws/         ← AWS Lambda container image entrypoint
cloudflare/  ← Cloudflare Containers entrypoint
local/       ← cron entrypoint (laptop / VPS / AWS Lightsail)
```

Entrypoints depend on `lib` via `workspace:*` and call into it; they only contain platform glue.

## Bucket conventions

- Configuration supports **multiple source/destination pairs** in a single run; orchestrator processes them sequentially. Each pair has independent credentials with cascade: bucket-level → pair-level → env-level (`SOURCE_*` / `DEST_*`).
- **Overlap is rejected at startup**: no source bucket may share endpoint + bucket-name with a dest bucket whose prefix overlaps. Source-vs-source and dest-vs-dest are allowed.
- Source bucket: **read-only**. Never modify, delete, or move source files.
- Destination layout (full tree in PLAN.md):
  - `by-id/<scheme>:<id>/` — content-addressed HLS output (v1 scheme is `sha256`)
  - `mappings/<source-path>.json` — source path → content ID lookup
  - `fingerprints/` — perceptual signatures + index
  - `.transcoder.lock` — single-runner lock at bucket root
- Always include the scheme prefix (`sha256:`) on content IDs — leaves room for `psig:` (perceptual) IDs later.

## Locking

- Global single-runner lock + per-video lease, both via S3 conditional PUT (`If-None-Match: *`).
- Three time values: `MAX_RUNTIME_SECONDS` (platform cap), self-imposed budget (`× 0.75`), lock TTL (`× 1.5`).
- Lambda binding constraint: 900s hard cap. CF Containers / local: configurable, default 3600s.

## Dedup

Two layers in v1: (1) byte-hash of source bytes (SHA-256), (2) MPEG-7 video signature (via `ffmpeg -vf signature`) for perceptual matching. Audio fingerprinting was rejected because many sources are silent. On a perceptual match with higher-resolution incoming, we re-transcode and repoint mappings; lower-or-equal incoming reuses existing.

## ABR ladder

H.264 Main + AAC. Default rungs: 360p / 480p / 720p / 1080p. Skip rungs above source resolution. HEVC/AV1 are out of scope (see [FUTURE.md](./FUTURE.md)).

## Conventions

- ESM throughout. `"type": "module"` in every package.json.
- `.env` for local dev only — never committed. The canonical `.env.sample` lives in `local/`; aws/cloudflare get config from platform-native sources but the same variable names.
- Errors fail loud — don't swallow exceptions in pipeline code. The lock+lease design makes retry safe.
- Don't add backwards-compat shims; this is a fresh project. If a design changes, change it.

## Where to look

- Architecture, env vars, decisions: [PLAN.md](./PLAN.md)
- Behavioral contract: [SPEC.md](./SPEC.md)
- Testing guide (how to run, mock strategy, what's covered): [TESTING.md](./TESTING.md)
- Deferred features: [FUTURE.md](./FUTURE.md)
- Per-platform setup: each entrypoint's `README.md`

# s3-hsts-transcoder

Self-hosted video transcoder. Watches an S3-compatible source bucket and produces HLS streaming output in a destination bucket. Designed to run on **AWS Lambda**, **Cloudflare Containers**, or any **local / VPS / AWS Lightsail** host on a cron schedule. No per-minute pricing.

> The directory name reflects an early naming mistake — the format produced is **HLS** (HTTP Live Streaming). HSTS is unrelated.

## Structure

- [`lib/`](./lib) — shared transcoding logic (the real work)
- [`aws/`](./aws) — AWS Lambda entrypoint
- [`cloudflare/`](./cloudflare) — Cloudflare Containers entrypoint
- [`local/`](./local) — local / VPS / AWS Lightsail cron entrypoint

Each package has its own README with platform-specific setup. Configuration is via environment variables; see [`local/.env.sample`](./local/.env.sample) for the canonical list.

## Prerequisites

- **Node.js 20+**
- **pnpm 10+** (`npm install -g pnpm`, or via Corepack)
- **ffmpeg** on `PATH` (only needed for the `local` entrypoint; the `aws` and `cloudflare` Dockerfiles vendor it)
- An **S3-compatible source bucket** (R2, S3, MinIO, …) with at least one video, and a **separate** destination bucket. Source and destination must not overlap (same endpoint + bucket name with overlapping prefixes is rejected at startup).

## First-time setup

```sh
git clone <repo> s3-hsts-transcoder
cd s3-hsts-transcoder
pnpm install
pnpm build
pnpm test          # 53 unit tests
```

## Quickstart — first transcode (local)

The fastest path from zero to a playable HLS manifest:

```sh
cp local/.env.sample local/.env
# edit local/.env: fill SOURCE_* and DEST_* (bucket, endpoint, keys)
pnpm --filter @s3-hsts-transcoder/local dev
```

This runs one transcoding pass against the buckets in `.env` and exits. After it finishes, find your output at:

```
<DEST_BUCKET>/mappings/<your-source-path>.json     ← contains hlsRoot
<DEST_BUCKET>/by-id/sha256:<hash>/master.m3u8      ← play this
```

Test with hls.js, Safari (native HLS), or `ffplay` against a presigned/public URL of the master playlist.

For ongoing operation, see the per-platform deploy guides below.

## Quick reference

| Want to                                         | Go to                                          |
| ----------------------------------------------- | ---------------------------------------------- |
| Understand the architecture                     | [PLAN.md](./PLAN.md)                           |
| Read the behavioral specification               | [SPEC.md](./SPEC.md)                           |
| See deferred features                           | [FUTURE.md](./FUTURE.md)                       |
| Run the transcoder on Lambda                    | [aws/README.md](./aws/README.md)               |
| Run the transcoder on Cloudflare                | [cloudflare/README.md](./cloudflare/README.md) |
| Run the transcoder locally / on VPS / Lightsail | [local/README.md](./local/README.md)           |
| Develop or test the shared lib                  | [lib/README.md](./lib/README.md)               |

# cloudflare — Cloudflare Containers entrypoint

Containerized Node entrypoint deployed via Cloudflare Containers. The container runs a one-shot transcoding pass and exits.

See **[../PLAN.md](../PLAN.md)** for architecture and **[../CLAUDE.md](../CLAUDE.md)** for conventions.

## Prerequisites

- A Cloudflare account with **Workers Paid** ($5/mo) — Containers requires a paid plan.
- The Cloudflare Containers product enabled on your account.
- Docker installed locally (only needed if you build the image yourself; `wrangler` can also build via the `image: ./Dockerfile` reference).
- An R2 bucket (or any S3-compatible source/destination).

> **Heads up:** Cloudflare Containers + the Worker/DO binding surface have been evolving rapidly. The walkthrough below is a sketch; cross-check every command against [developers.cloudflare.com/containers/](https://developers.cloudflare.com/containers/) before running it. If a command rejects an option or asks for a different field name, the docs are authoritative — these snippets are not.

## Install wrangler

```sh
pnpm --filter @s3-hsts-transcoder/cloudflare add -D wrangler
pnpm --filter @s3-hsts-transcoder/cloudflare exec wrangler login
```

## Configuration

Env vars are documented in [`local/.env.sample`](../local/.env.sample). Two ways to set them on Cloudflare:

```sh
# Non-secrets — declared in wrangler.jsonc under `vars`.
# (e.g., LOG_LEVEL, MAX_RUNTIME_SECONDS, SOURCE_ENDPOINT, SOURCE_BUCKET)

# Secrets — never commit these. Stored encrypted by Cloudflare.
cd cloudflare
pnpm exec wrangler secret put SOURCE_ACCESS_KEY_ID
pnpm exec wrangler secret put SOURCE_SECRET_ACCESS_KEY
pnpm exec wrangler secret put DEST_ACCESS_KEY_ID
pnpm exec wrangler secret put DEST_SECRET_ACCESS_KEY
```

`MAX_RUNTIME_SECONDS` defaults to **3600** on this entrypoint.

## Build (optional, for local testing)

```sh
docker build -f cloudflare/Dockerfile -t s3-hsts-transcoder-cf .
docker run --rm --env-file local/.env s3-hsts-transcoder-cf   # smoke test
```

## Deploy

```sh
cd cloudflare

# 1. Copy the wrangler config skeleton and fill in account_id + image registry.
cp wrangler.jsonc.example wrangler.jsonc
$EDITOR wrangler.jsonc

# 2. Review src/worker.ts. It's a @ts-nocheck skeleton — the DurableObject +
#    Container binding shape needs to match whatever the current CF docs
#    describe. Adjust before deploying.

# 3. Deploy. Depending on your wrangler version this either builds+pushes
#    the container image automatically (when `image: ./Dockerfile`) or
#    expects a pre-pushed image URL.
pnpm exec wrangler deploy
```

After deploy, the Worker's cron trigger (default `*/15 * * * *` in the example config) wakes the DurableObject, which starts the container, which runs `runOnce()` and exits.

## Container lifecycle

The Worker (cron-triggered) wakes the container's Durable Object, which starts the container. The container's entrypoint (`node dist/index.js`) runs `runOnce()`, releases the global lock, and exits. Cloudflare tears down the container after `sleepAfter` (configured in `wrangler.jsonc`).

## R2 bindings vs. S3 API

R2 bindings are faster and avoid the public network. v1 uses the S3-compatible API for portability across all three platforms; switching to native R2 bindings on this entrypoint is a future optimization.

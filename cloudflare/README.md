# cloudflare — Cloudflare Containers entrypoint

Containerized Node entrypoint deployed via Cloudflare Containers. Triggered by a Worker Cron Trigger that wakes the container's Durable Object.

See **[../PLAN.md](../PLAN.md)** for architecture and **[../CLAUDE.md](../CLAUDE.md)** for conventions.

## Configuration

Set env vars in `wrangler.jsonc` `vars` (non-secret) or via `wrangler secret put` (secrets). Variable names are listed in [`local/.env.sample`](../local/.env.sample). On this entrypoint, `MAX_RUNTIME_SECONDS` defaults to **3600**.

## Container lifecycle

The cron Worker wakes the container's Durable Object, which starts the container. The container runs the transcoding pass, releases the global lock, and exits. CF tears down the container after `sleepAfter` (configured in `wrangler.jsonc`).

## R2 bindings vs. S3 API

R2 bindings are faster and bypass the public network. v1 uses the S3-compatible API for portability; switching to bindings is a future optimization.

## Build & deploy

`Dockerfile` and `wrangler.jsonc` are forthcoming — populated alongside the real `lib` implementation.

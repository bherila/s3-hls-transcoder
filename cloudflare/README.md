# cloudflare — Cloudflare Containers entrypoint

Containerized Node entrypoint deployed via Cloudflare Containers. The container runs a one-shot transcoding pass and exits.

See **[../PLAN.md](../PLAN.md)** for architecture and **[../CLAUDE.md](../CLAUDE.md)** for conventions.

## Configuration

Env vars are documented in [`local/.env.sample`](../local/.env.sample). Set them in `wrangler.jsonc` `vars` (non-secrets) or via `wrangler secret put` (secrets like `*_SECRET_ACCESS_KEY`). On this entrypoint, `MAX_RUNTIME_SECONDS` defaults to **3600**.

## Build

From the repo root:

```sh
docker build -f cloudflare/Dockerfile -t s3-hsts-transcoder-cf .
```

## Deploy

1. Copy [`wrangler.jsonc.example`](./wrangler.jsonc.example) to `wrangler.jsonc` and fill in details.
2. Push the container image to a registry that Cloudflare can pull from (or rely on `wrangler` to build + push for you, depending on current CF Containers tooling).
3. Review [`src/worker.ts`](./src/worker.ts) — a skeleton Durable Object + `scheduled` cron handler. It carries `@ts-nocheck` and is excluded from `tsc` because the Cloudflare Containers binding surface is still moving; verify against [the current docs](https://developers.cloudflare.com/containers/) before deploying.
4. `pnpm wrangler deploy`.

> **Heads up:** the Containers + Durable Objects + Worker wiring is still evolving. Verify the binding syntax and DO/Container interaction against [developers.cloudflare.com/containers/](https://developers.cloudflare.com/containers/) before deploying.

## Container lifecycle

The Worker (cron-triggered) wakes the container's Durable Object, which starts the container. The container's entrypoint (`node dist/index.js`) runs `runOnce()`, releases the global lock, and exits. Cloudflare tears down the container after `sleepAfter` (configured in `wrangler.jsonc`).

## R2 bindings vs. S3 API

R2 bindings are faster and avoid the public network. v1 uses the S3-compatible API for portability across all three platforms; switching to native R2 bindings on this entrypoint is a future optimization.

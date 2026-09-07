# local — local / VPS / AWS Lightsail entrypoint

Single-process Node entrypoint suitable for cron on any host with ffmpeg installed: a laptop, a VPS, or an AWS Lightsail instance.

See **[../PLAN.md](../PLAN.md)** for architecture and **[../CLAUDE.md](../CLAUDE.md)** for conventions.

## Setup

1. Install Node 20+ and ffmpeg (`apt install ffmpeg` on Debian/Ubuntu, `brew install ffmpeg` on macOS).
2. Install pnpm (`npm install -g pnpm` or use Corepack).
3. From the repo root: `pnpm install` then `pnpm build`.
4. In `local/`: `cp .env.sample .env` and fill in credentials.
5. Run a one-shot pass: `pnpm start` (or `pnpm dev` to skip the build step).
6. Wire up cron (see below).

## Cron

```cron
*/15 * * * * cd /opt/transcoder/local && pnpm start >> /var/log/transcoder.log 2>&1
```

The global lock in the destination bucket prevents overlapping runs even if the cron interval is shorter than a transcoding job.

## AWS Lightsail recommendation

Lightsail is well-suited: fixed monthly price, generous bandwidth allowance, simple cron, and ffmpeg installs cleanly.

| Use case                                | Plan       | Specs                                                    |
| --------------------------------------- | ---------- | -------------------------------------------------------- |
| Occasional, short videos                | $12/mo     | 2 vCPU (burstable), 2 GB RAM, 60 GB SSD, 3 TB transfer   |
| **Daily/weekly cron, mixed lengths**    | **$24/mo** | 2 vCPU, 4 GB RAM, 80 GB SSD, 4 TB transfer ← **default** |
| Many videos / long videos / faster turn | $84/mo     | 4 vCPU, 16 GB RAM, 320 GB SSD, 6 TB transfer             |

Notes:

- **CPU is the bottleneck** for ffmpeg, not RAM. More vCPUs ≈ proportionally faster encoding.
- The smaller plans use **burstable CPU**. Sustained transcoding can exhaust burst credits and throttle. The $24/mo plan is the sweet spot for steady cron workloads.
- The transcoder **streams source bytes** and **uploads output segments as they're produced**, so disk space is rarely a constraint. The default 60–80 GB SSD is plenty.
- Place the Lightsail instance in the **same region** as the destination bucket to minimize transfer latency. Lightsail outbound transfer to R2/S3 counts against the bundle; R2 ingress is free.

## Event-driven wake-up (lower latency than cron)

A `*/15` cron means a freshly uploaded video can wait a quarter of an hour before HLS exists. The Go worker (`cmd/transcoder`) can instead run long-lived and start a pass as soon as something tells it to.

Wake requests are only a hint about timing. Every pass is still a full scan of the source bucket, so a lost, duplicated, or unrelated request costs at most one extra pass and can never leave an upload unprocessed. Requests that arrive during a pass coalesce into one follow-up pass, and `POLL_FALLBACK_SECONDS` keeps a periodic sweep running underneath as the safety net. Polling-only deployments are unaffected: with none of these variables set, the worker runs a single pass and exits exactly as before.

### HTTP endpoint

```sh
POLL_FALLBACK_SECONDS=900 \
WAKE_HTTP_ADDR=127.0.0.1:8787 \
WAKE_HTTP_TOKEN=$(openssl rand -hex 16) \
  ./transcoder
```

The worker serves:

| Route          | Purpose                                                                     |
| -------------- | --------------------------------------------------------------------------- |
| `POST /wake`   | Request a pass. Returns `202`. The request body is ignored.                 |
| `GET /healthz` | Liveness for a supervisor or load balancer. Returns `200`, no token needed. |

```sh
curl -X POST -H "X-Wake-Token: $WAKE_HTTP_TOKEN" http://127.0.0.1:8787/wake
# Authorization: Bearer $WAKE_HTTP_TOKEN works too.
```

Anything that can make an HTTP request can be the trigger:

- **R2**: enable object-created event notifications → Cloudflare Queue → a consumer Worker that `POST`s `/wake`.
- **S3**: bucket event notification → SNS or an EventBridge rule → a small Lambda that `POST`s `/wake`. (On Lambda itself, invoke the function directly instead — see [../aws/README.md](../aws/README.md).)
- **Your app**: `POST /wake` right after it finishes uploading, which gives the tightest loop of all.

If `WAKE_HTTP_TOKEN` is unset the endpoint is unauthenticated and the worker logs a warning at startup — bind to `127.0.0.1` and reverse-proxy it, or set a token.

### Redis queue

If the app already has Redis, it can `LPUSH` instead of making an HTTP call:

```sh
POLL_FALLBACK_SECONDS=900 REDIS_URL=redis://localhost:6379/0 ./transcoder
# app side, after an upload:
redis-cli LPUSH transcode:requests 1
```

Both wake sources can be enabled at once; `TRANSCODE_QUEUE` renames the list.

### Running it as a service

Long-lived mode replaces the cron entry and the oneshot timer below — use `Type=simple` and let systemd keep it running:

```ini
# /etc/systemd/system/transcoder-daemon.service
[Unit]
Description=s3-hls-transcoder (event-driven)
After=network-online.target

[Service]
Type=simple
ExecStart=/opt/transcoder/transcoder
EnvironmentFile=/opt/transcoder/.env
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

## systemd timer (alternative to cron)

```ini
# /etc/systemd/system/transcoder.service
[Unit]
Description=s3-hls-transcoder

[Service]
Type=oneshot
WorkingDirectory=/opt/transcoder/local
ExecStart=/usr/local/bin/pnpm start
EnvironmentFile=/opt/transcoder/local/.env
```

```ini
# /etc/systemd/system/transcoder.timer
[Unit]
Description=Run s3-hls-transcoder every 15 minutes

[Timer]
OnCalendar=*:0/15
Persistent=true

[Install]
WantedBy=timers.target
```

`systemctl enable --now transcoder.timer`.

## Configuration

See [`.env.sample`](./.env.sample) and the env var table in [PLAN.md](../PLAN.md#configuration-env-vars).

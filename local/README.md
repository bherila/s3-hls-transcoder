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

| Use case                                | Plan       | Specs                                                        |
|-----------------------------------------|------------|--------------------------------------------------------------|
| Occasional, short videos                | $12/mo     | 2 vCPU (burstable), 2 GB RAM, 60 GB SSD, 3 TB transfer       |
| **Daily/weekly cron, mixed lengths**    | **$24/mo** | 2 vCPU, 4 GB RAM, 80 GB SSD, 4 TB transfer ← **default**     |
| Many videos / long videos / faster turn | $84/mo     | 4 vCPU, 16 GB RAM, 320 GB SSD, 6 TB transfer                 |

Notes:

- **CPU is the bottleneck** for ffmpeg, not RAM. More vCPUs ≈ proportionally faster encoding.
- The smaller plans use **burstable CPU**. Sustained transcoding can exhaust burst credits and throttle. The $24/mo plan is the sweet spot for steady cron workloads.
- The transcoder **streams source bytes** and **uploads output segments as they're produced**, so disk space is rarely a constraint. The default 60–80 GB SSD is plenty.
- Place the Lightsail instance in the **same region** as the destination bucket to minimize transfer latency. Lightsail outbound transfer to R2/S3 counts against the bundle; R2 ingress is free.

## systemd timer (alternative to cron)

```ini
# /etc/systemd/system/transcoder.service
[Unit]
Description=s3-hsts-transcoder

[Service]
Type=oneshot
WorkingDirectory=/opt/transcoder/local
ExecStart=/usr/local/bin/pnpm start
EnvironmentFile=/opt/transcoder/local/.env
```

```ini
# /etc/systemd/system/transcoder.timer
[Unit]
Description=Run s3-hsts-transcoder every 15 minutes

[Timer]
OnCalendar=*:0/15
Persistent=true

[Install]
WantedBy=timers.target
```

`systemctl enable --now transcoder.timer`.

## Configuration

See [`.env.sample`](./.env.sample) and the env var table in [PLAN.md](../PLAN.md#configuration-env-vars).

# aws — AWS Lambda entrypoint

Lambda container image. Triggered by an EventBridge cron rule.

See **[../PLAN.md](../PLAN.md)** for architecture and **[../CLAUDE.md](../CLAUDE.md)** for conventions.

## Configuration

Set the env vars listed in [`local/.env.sample`](../local/.env.sample) on the Lambda function (`Configuration → Environment variables` in the AWS console). On this entrypoint, `MAX_RUNTIME_SECONDS` defaults to **900** (Lambda's hard cap).

## Memory / timeout

- Memory: **3008–10240 MB**. More memory ≈ proportionally more vCPU; for transcoding, set high.
- Timeout: **900s** (Lambda max). The transcoder self-imposes a 75% runtime budget (default 675s) and exits cleanly before the platform kill.

## Build & deploy

`Dockerfile` is forthcoming — populated alongside the real `lib` implementation.

Sketch: `public.ecr.aws/lambda/nodejs:20` base + `ffmpeg-static-amd64` + `pnpm deploy --filter @s3-hsts-transcoder/aws --prod` to assemble a self-contained `node_modules`.

## Sample IAM policy

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["s3:GetObject", "s3:ListBucket", "s3:HeadObject"],
      "Resource": ["arn:aws:s3:::SOURCE_BUCKET", "arn:aws:s3:::SOURCE_BUCKET/*"]
    },
    {
      "Effect": "Allow",
      "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:ListBucket", "s3:HeadObject"],
      "Resource": ["arn:aws:s3:::DEST_BUCKET", "arn:aws:s3:::DEST_BUCKET/*"]
    }
  ]
}
```

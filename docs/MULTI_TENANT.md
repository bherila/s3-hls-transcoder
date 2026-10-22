# One Go worker, multiple tenants

The Go transcoder image can serve independent applications from one container.
Each tenant keeps its own source bucket, output bucket and credentials. Existing
`SOURCE_*` / `DEST_*` single-pair configuration still works.

`BUCKETS_CONFIG_FILE` (preferred for secrets) or `BUCKETS_CONFIG` accepts an array
of bucket pairs. The Go worker now visits one source per pair per round, rather
than draining the first bucket before visiting the next. Source processing is
serial across the whole runner: only one download/hash/fingerprint/transcode
pipeline is active at a time. A tenant with a large backlog therefore cannot
start a second ffmpeg encode alongside another tenant's encode.

Fairness is per source, not per CPU second. One long video can delay another
tenant until it finishes or reaches the shared run deadline. In daemon mode the
next sweep starts with the tenant following the last attempted source, so an
interrupted long encode or stalled S3 request cannot always take the first turn. This cursor is kept
in memory, so it resets when the process restarts. Each sweep still lists from
the beginning and skips cached mappings; the cursor is not a durable object-key
checkpoint.

## Example: Vora and Kath

Save a real JSON array as `/etc/transcoder/buckets.json`. All credentials and
endpoints below are placeholders; substitute bucket-scoped tokens. The two
sides may use different endpoints or accounts.

```json
[
  {
    "name": "vora",
    "source": {
      "bucket": "vora-input",
      "endpoint": "https://ACCOUNT.r2.cloudflarestorage.com",
      "prefix": "media/reviewed/",
      "accessKeyId": "VORA_SOURCE_KEY",
      "secretAccessKey": "VORA_SOURCE_SECRET"
    },
    "dest": {
      "bucket": "vora-encoded",
      "endpoint": "https://ACCOUNT.r2.cloudflarestorage.com",
      "accessKeyId": "VORA_OUTPUT_KEY",
      "secretAccessKey": "VORA_OUTPUT_SECRET"
    }
  },
  {
    "name": "kath",
    "source": {
      "bucket": "kath-wedding",
      "endpoint": "https://ACCOUNT.r2.cloudflarestorage.com",
      "prefix": "videos/",
      "accessKeyId": "KATH_SOURCE_KEY",
      "secretAccessKey": "KATH_SOURCE_SECRET"
    },
    "dest": {
      "bucket": "kath-wedding-hls",
      "endpoint": "https://ACCOUNT.r2.cloudflarestorage.com",
      "accessKeyId": "KATH_OUTPUT_KEY",
      "secretAccessKey": "KATH_OUTPUT_SECRET"
    }
  }
]
```

Use Vora's actual reviewed-object prefix from its existing worker configuration;
the example prefix is illustrative. Source prefix filtering is literal. Do not
broaden it to include uploads that the app has not approved.

`name` is an optional unique label for structured logs. Unnamed pairs use
`pair-0`, `pair-1`, etc. It changes no object keys or app integration contracts.
Credential resolution stays bucket → pair → side-specific environment fallback.
Use explicit per-bucket credentials for independent tenants to avoid accidental
fallback to another tenant's token. Grant source reads only and the required
output reads/writes/deletes. Keep the JSON file out of git and mount it read-only.

For example, run the image built from this change with a shared CPU/memory cap:

```sh
docker run -d --name shared-hls --restart unless-stopped \
  --cpus=2 --memory=4g \
  --mount type=bind,src=/etc/transcoder/buckets.json,dst=/run/secrets/buckets.json,readonly \
  -e BUCKETS_CONFIG_FILE=/run/secrets/buckets.json \
  -e POLL_FALLBACK_SECONDS=300 \
  -e MAX_RUNTIME_SECONDS=3600 \
  -e CLEANUP_DELETED_SOURCES=true \
  ghcr.io/bherila/s3-hls-transcoder:YOUR_TESTED_TAG
```

Choose CPU, memory and temporary disk capacity for your largest video. Serial
source scheduling prevents concurrent encodes within this worker; Docker's
resource limits bound its total host usage. `MAX_CONCURRENCY` remains reserved
in the Go configuration and does not enable parallel jobs. This is not a
host-wide semaphore: separate worker containers, the imagehasher, or other
ffmpeg processes still consume resources independently.

## Isolation and triggering

- Every output bucket must be distinct, even when destination prefixes differ.
  Mappings, content hashes, reverse references, fingerprints, tombstones and
  locks all remain inside that bucket. There is no cross-tenant deduplication.
- Each pair holds its existing `.transcoder.lock` while its scan is suspended
  between turns. A busy lock, lock error or scan error skips only that pair.
- Cleanup runs for a pair after its full source scan completes. It uses that
  pair's credentials, source prefix and output bucket.
- The runtime budget is shared by every pair and includes in-flight work.
  Set `MAX_RUNTIME_SECONDS × BUDGET_MULTIPLIER` above the longest expected
  single encode; a video that cannot finish within that budget will be retried
  each sweep. This replaces the previous Go behavior that let an in-flight
  encode overrun the budget and potentially outlive its lock.
  Its deadline cancels downloads, S3 calls and subprocesses. Interrupted work is
  retried on a later sweep without a failure tombstone; lock release uses a
  separate bounded context so cancellation does not leave a live lock behind.
- Polling and a single Redis wake queue or HTTP `/wake` endpoint wake a full
  sweep of all pairs. Wake requests are coalesced signals, not tenant jobs.
  Tenant-specific queues, wake tokens and per-tenant encode policies are not
  introduced here. One shared HTTP token should be kept within trusted callers.
- Ladder, cleanup, re-encode and tombstone options apply to every configured pair.
  Use a separate worker when those policies must differ.

Multi-tenant fairness across sweeps requires daemon mode (poll or wake).
Repeated one-shot cron processes reset the cursor on every invocation.

## Moving from separate workers

1. Prepare the JSON file from the existing worker settings, retaining the exact
   source prefixes, endpoints and bucket-scoped credentials. Build/test the new
   image and retain the old image references and configurations for rollback.
2. Stop the old video workers and any cron jobs for those pairs. Wait for their
   processes to exit before starting the shared worker. Existing bucket locks
   protect storage if runs overlap, but overlap defeats the shared resource cap.
3. Start one shared worker. Watch the `tenant`, `sourceBucket` and `destBucket`
   log fields; confirm both tenants receive turns and existing objects are
   reported as cached. Upload one new video through each app and verify its HLS
   mapping and playback in its own output bucket.
4. Verify CPU/memory use and cleanup with a disposable upload before retiring
   the old configurations. To roll back, stop the shared worker first, then
   restore the old workers with their original settings.

No bucket moves, mapping migrations, app code changes or deployment operations
are required by the code change itself.

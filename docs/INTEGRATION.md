# Integrating an app with s3-hls-transcoder

The contract between an application that uploads videos and the transcoder that
turns them into HLS. If you are deploying the transcoder, start with
[PLAN.md](../PLAN.md) and the per-platform READMEs; this document is for the
application on the other side of the buckets.

[SPEC.md](../SPEC.md) is the normative version of everything here.

## The shape of the arrangement

```
        your app ──uploads──▶  source bucket  ──read-only──▶  transcoder
                                                                  │
        your app ◀──reads────  destination bucket  ◀──writes──────┘
                                       │
                              players ─┘ (public URL or CDN)
```

Three rules follow from that picture, and everything else is detail:

1. **The source bucket is yours.** The transcoder only lists and reads it. It
   never writes, moves, or deletes anything there — so upload, replace, and
   delete objects however you like.
2. **The destination bucket is the transcoder's.** Read it freely; do not write
   to it. Its layout is content-addressed and reference-counted, and hand-edits
   break the bookkeeping.
3. **The two must be disjoint.** Same endpoint + bucket name with overlapping
   prefixes is rejected at startup.

## 1. Uploading a source object

Put the file anywhere in the source bucket (or under the configured
`SOURCE_PREFIX`, if the deployment restricts the scan to a subtree). Directory
structure is up to you; it is preserved in the mapping path.

**The key must end in a recognized video extension**, matched case-insensitively
on the basename:

```
mp4  mov  mkv  webm  avi  m4v  mpg  mpeg  wmv  flv  ogv  3gp  ts  m2ts
```

Anything else is skipped silently — an extensionless key or a `.MP4.tmp`
part-file is never picked up. Upload atomically (or to a staging prefix outside
`SOURCE_PREFIX` and copy into place) so the transcoder never sees a partial
object.

A source key is used verbatim in the destination bucket's mapping path, so keys
must be unique across every source pair that shares a destination bucket. That
is the reason destination buckets have to be unique per run.

**Replacing a file:** upload over the same key. The transcoder detects the new
ETag and size and re-transcodes, then repoints the mapping. Until it does, the
mapping still resolves to the old output — see §3 for detecting that window.

## 2. Resolving a playable stream

One GET answers "is this video ready, and where do I play it?":

```
GET <destination>/mappings/<source-key>.json
```

```json
{
  "sourceKey": "uploads/2026/clip.mp4",
  "sourceBucket": "my-uploads",
  "sourceEndpoint": "https://<account>.r2.cloudflarestorage.com",
  "sourceEtag": "d41d8cd98f00b204e9800998ecf8427e",
  "sourceSize": 12345678,
  "sourceLastModified": "2026-06-01T10:30:00Z",
  "contentId": "sha256:f7c3bcc0...",
  "hlsRoot": "by-id/sha256:f7c3bcc0.../master.m3u8",
  "encodedAt": "2026-06-01T10:41:02.123Z",
  "encoderVersion": "0.1.0"
}
```

`hlsRoot` is a key in the destination bucket. Join it onto whatever public base
URL serves that bucket:

```js
const res = await fetch(`${DEST_PUBLIC_BASE}/mappings/${sourceKey}.json`);
if (res.status === 404) return { state: "processing" };
const mapping = await res.json();
return { state: "ready", playbackUrl: `${DEST_PUBLIC_BASE}/${mapping.hlsRoot}` };
```

Feed `playbackUrl` to hls.js, or to a `<video src>` directly on Safari and iOS,
which play HLS natively.

**Use `hlsRoot`. Do not build the path yourself.** `by-id/<contentId>/...` is an
internal layout that leaves room for future identifier schemes; `hlsRoot` is the
part that is promised. Everything else under the destination bucket —
`fingerprints/`, `.transcoder.lock`, `.processing` — is transcoder bookkeeping,
not API.

Two things to know about the output tree:

- The master playlist references its variants relatively (`360p/index.m3u8`), so
  the whole `by-id/<contentId>/` directory must be served from one origin.
- Content types are set on upload (`application/vnd.apple.mpegurl` for `.m3u8`,
  `video/iso.segment` for `.m4s`), so a bucket served as a static origin needs
  no extra configuration for players to accept them.

### Optional: `metadata.json`

`by-id/<contentId>/metadata.json` carries the probe result and the ladder
actually used — source width, height, duration, bitrate, and the rungs that
survived the "no upscaling" filter. Useful for a player poster aspect ratio or a
duration badge without touching the source file. Read it through `hlsRoot`'s
directory rather than assembling the key from `contentId`.

## 3. Processing, ready, and failed

The transcoder has no callback and no queue you can inspect. State is derived
from objects in the destination bucket:

| State          | How to detect it                                                                                                            |
| -------------- | --------------------------------------------------------------------------------------------------------------------------- |
| **processing** | `mappings/<key>.json` returns 404                                                                                           |
| **ready**      | The mapping exists and its `sourceEtag` + `sourceSize` match the source object                                              |
| **stale**      | The mapping exists but those fields differ — the file was replaced; the old output still plays until the next pass finishes |
| **failed**     | `errors/<key>.json` exists (see below)                                                                                      |

If you do not track the source ETag, treat mapping-exists as ready; you will
serve the previous rendition for one cycle after a replacement, which is
usually the desired behavior anyway.

### Failures

When the Go worker runs with error tombstones enabled (`ERROR_TOMBSTONES`,
default on), a source that fails to transcode gets:

```
GET <destination>/errors/<source-key>.json
```

```json
{
  "sourceKey": "uploads/2026/broken.mp4",
  "sourceEtag": "...",
  "sourceSize": 4096,
  "error": "ffmpeg exited with code 1: ...",
  "failedAt": "2026-06-01T10:41:02.123Z",
  "attempts": 3,
  "encoderVersion": "0.1.0"
}
```

After `TOMBSTONE_MAX_ATTEMPTS` (default 3) the file stops being retried, so a
deterministically broken upload cannot consume every run. `error` is an
operator-facing string — surface it in an admin view, not to end users, and do
not parse it. Re-uploading a fixed file under the same key changes the ETag,
which clears the block automatically.

### How long "processing" lasts

That is a deployment property, not a contract. A cron-driven deployment
processes new uploads on its next tick — commonly 15 minutes — plus transcode
time. Deployments that need lower latency can run the worker long-lived with a
wake source (`REDIS_URL`, with `POLL_FALLBACK_SECONDS` as a safety net), so a
pass starts within seconds of an upload; see the trigger modes in the
[repository README](../README.md#workers). Either way, plan the UI around a
"processing" state: an upload is never ready synchronously.

## 4. Deleting a video

**Delete the source object. That is the whole API.** Never delete anything in
the destination bucket by hand — outputs are shared between sources by
content-hash dedup, so deleting a `by-id/` tree can break an unrelated video
that happens to have identical bytes.

What happens next depends on the deployment:

- `CLEANUP_DELETED_SOURCES=false` (the default): nothing is removed. The mapping
  and its output stay, and continue to play if someone holds the URL.
- `CLEANUP_DELETED_SOURCES=true`: the next pass deletes the orphaned mapping, so
  your app sees a 404 and can treat the video as gone. The `by-id/` output is
  garbage-collected only when **no other mapping references it** — if two
  uploads had identical bytes, removing one leaves the output in place for the
  other.

So: after deleting the source, the mapping disappearing is the signal that
cleanup has happened, and it may lag by one cron cycle. If your app needs the
video to look gone immediately, stop serving it on your side; do not wait for
the bucket.

## 5. Serving the destination bucket

### CORS

Browser playback with hls.js (Chrome, Firefox, Edge) requires CORS on the
destination bucket or its CDN. Safari's native HLS does not, but the same
configuration is harmless. Both fetch byte ranges, so `Range` must be allowed
and the range response headers exposed:

```json
[
  {
    "AllowedOrigins": ["https://app.example.com"],
    "AllowedMethods": ["GET", "HEAD"],
    "AllowedHeaders": ["Range", "If-None-Match", "If-Modified-Since"],
    "ExposeHeaders": ["Content-Length", "Content-Range", "Content-Type", "ETag", "Accept-Ranges"],
    "MaxAgeSeconds": 3600
  }
]
```

Apply with `aws s3api put-bucket-cors --bucket <dest> --cors-configuration file://cors.json`,
or in the R2 dashboard under the bucket's CORS policy. List every origin that
embeds the player, including preview deployments; `"*"` works for fully public
content.

### Caching

The two prefixes want opposite policies:

| Prefix      | Cache                                                                                     |
| ----------- | ----------------------------------------------------------------------------------------- |
| `by-id/`    | Long and immutable. Paths are content-addressed, so the bytes at a key never change.      |
| `mappings/` | Short, or revalidated. A mapping is rewritten when a source is replaced or re-transcoded. |

A CDN in front of `by-id/` is worth having: segment requests dominate, and they
are perfectly cacheable.

### Access

Nothing in the output is access-controlled — v1 assumes a destination bucket
that is public, or private and fronted by something of yours that issues signed
URLs. If you sign them, sign the whole `by-id/<contentId>/` prefix, not just the
master playlist, or the variant playlists and segments will 403.

## 6. Images (`cmd/imagehasher`)

The sibling worker follows the same shape for images: point it at an image
bucket and it writes `image-mappings/<source-key>.json` with a `pdqHash` field
for near-duplicate detection. It computes hashes only — every duplicate decision
stays in your app. See the [repository README](../README.md#workers).

## Checklist

- [ ] Source keys end in a supported video extension, and uploads are atomic.
- [ ] Source keys are unique across all pairs sharing a destination bucket.
- [ ] The app reads `mappings/<source-key>.json` and plays `hlsRoot`; it never
      builds `by-id/` paths itself.
- [ ] The UI has a "processing" state, and does not expect a synchronous result.
- [ ] Replacement is detected via `sourceEtag`/`sourceSize` if serving the
      previous rendition for a cycle is not acceptable.
- [ ] Deletion is done by deleting the source object, never destination objects.
- [ ] `errors/<source-key>.json` is surfaced somewhere an operator will see it.
- [ ] The destination bucket has CORS for the player origins, and cache headers
      that distinguish `by-id/` from `mappings/`.

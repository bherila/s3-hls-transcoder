import { ListObjectsV2Command, type S3Client } from "@aws-sdk/client-s3";

export interface SourceObject {
  key: string;
  etag: string;
  size: number;
  lastModified: Date;
}

const VIDEO_EXTENSIONS: ReadonlySet<string> = new Set([
  "mp4", "mov", "mkv", "webm", "avi", "m4v",
  "mpg", "mpeg", "wmv", "flv", "ogv", "3gp", "ts", "m2ts",
]);

export function isVideoKey(key: string): boolean {
  const slash = key.lastIndexOf("/");
  const name = slash === -1 ? key : key.slice(slash + 1);
  const dot = name.lastIndexOf(".");
  if (dot === -1) return false;
  return VIDEO_EXTENSIONS.has(name.slice(dot + 1).toLowerCase());
}

export interface ScanOptions {
  prefix?: string;
  filter?: (key: string) => boolean;
}

/**
 * Lists source bucket objects with pagination. Filters to recognized video
 * extensions by default; pass a custom `filter` to override.
 *
 * Yields objects in S3's listing order (lexicographic by key).
 */
export async function* scanSource(
  client: S3Client,
  bucket: string,
  options: ScanOptions = {},
): AsyncIterable<SourceObject> {
  const filter = options.filter ?? isVideoKey;
  let continuationToken: string | undefined;

  do {
    const res = await client.send(
      new ListObjectsV2Command({
        Bucket: bucket,
        Prefix: options.prefix,
        ContinuationToken: continuationToken,
      }),
    );

    for (const obj of res.Contents ?? []) {
      if (!obj.Key || !obj.ETag || obj.Size === undefined || !obj.LastModified) continue;
      if (obj.Key.endsWith("/") && obj.Size === 0) continue;
      if (!filter(obj.Key)) continue;

      yield {
        key: obj.Key,
        etag: obj.ETag.replace(/^"|"$/g, ""),
        size: obj.Size,
        lastModified: obj.LastModified,
      };
    }

    continuationToken = res.NextContinuationToken;
  } while (continuationToken);
}

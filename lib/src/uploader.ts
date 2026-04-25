import type { S3Client } from "@aws-sdk/client-s3";
import { Upload } from "@aws-sdk/lib-storage";
import { createReadStream } from "node:fs";
import { readdir } from "node:fs/promises";
import path from "node:path";

export interface UploadDirectoryOptions {
  client: S3Client;
  bucket: string;
  /** Object key prefix in the destination bucket. Should end with "/". */
  keyPrefix: string;
  localDir: string;
  /** Concurrent uploads. Default: 8. */
  concurrency?: number;
}

/**
 * Uploads every file under `localDir` to `bucket` under `keyPrefix`,
 * preserving relative paths. Sets HLS-aware Content-Type headers.
 * Uses lib-storage Upload so large segments use multipart automatically.
 */
export async function uploadDirectory(opts: UploadDirectoryOptions): Promise<string[]> {
  const { client, bucket, keyPrefix, localDir } = opts;
  const concurrency = opts.concurrency ?? 8;

  const files: string[] = [];
  for await (const file of walkDir(localDir)) {
    files.push(file);
  }

  const keys: string[] = new Array(files.length);
  await pmap(files, concurrency, async (filePath, i) => {
    const rel = path.relative(localDir, filePath);
    const key = `${keyPrefix}${rel.split(path.sep).join("/")}`;

    const upload = new Upload({
      client,
      params: {
        Bucket: bucket,
        Key: key,
        Body: createReadStream(filePath),
        ContentType: contentTypeFor(rel),
      },
    });
    await upload.done();
    keys[i] = key;
  });

  return keys;
}

async function* walkDir(dir: string): AsyncIterable<string> {
  for (const entry of await readdir(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      yield* walkDir(full);
    } else if (entry.isFile()) {
      yield full;
    }
  }
}

export function contentTypeFor(filename: string): string {
  if (filename.endsWith(".m3u8")) return "application/vnd.apple.mpegurl";
  if (filename.endsWith(".m4s")) return "video/iso.segment";
  if (filename.endsWith(".mp4")) return "video/mp4";
  if (filename.endsWith(".ts")) return "video/mp2t";
  if (filename.endsWith(".vtt")) return "text/vtt";
  if (filename.endsWith(".json")) return "application/json";
  return "application/octet-stream";
}

async function pmap<T, R>(
  items: readonly T[],
  concurrency: number,
  fn: (item: T, index: number) => Promise<R>,
): Promise<R[]> {
  const results: R[] = new Array(items.length);
  let next = 0;
  async function worker(): Promise<void> {
    for (;;) {
      const i = next++;
      if (i >= items.length) return;
      results[i] = await fn(items[i]!, i);
    }
  }
  const workers = Array.from({ length: Math.min(concurrency, items.length) }, () => worker());
  await Promise.all(workers);
  return results;
}

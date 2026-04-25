import { GetObjectCommand, type S3Client } from "@aws-sdk/client-s3";
import { createHash } from "node:crypto";
import { createWriteStream } from "node:fs";
import { Readable, Transform } from "node:stream";
import { pipeline } from "node:stream/promises";

export interface DownloadResult {
  sha256: string;
  bytes: number;
}

/**
 * Streams an S3 object to disk, computing SHA-256 along the way. Saves a
 * second full read of the source for the byte-hash dedup check.
 */
export async function downloadAndHash(
  client: S3Client,
  bucket: string,
  key: string,
  localPath: string,
): Promise<DownloadResult> {
  const res = await client.send(new GetObjectCommand({ Bucket: bucket, Key: key }));
  if (!res.Body) throw new Error(`Empty body for s3://${bucket}/${key}`);

  const stream = res.Body as Readable;
  const hash = createHash("sha256");
  let bytes = 0;

  const teeAndHash = new Transform({
    transform(chunk, _enc, cb) {
      const buf: Buffer = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk);
      hash.update(buf);
      bytes += buf.length;
      cb(null, buf);
    },
  });

  await pipeline(stream, teeAndHash, createWriteStream(localPath));

  return { sha256: hash.digest("hex"), bytes };
}

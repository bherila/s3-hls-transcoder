import { GetObjectCommand, type S3Client } from "@aws-sdk/client-s3";
import { createHash } from "node:crypto";
import { Readable } from "node:stream";

export interface HashedSource {
  /** Lowercase hex SHA-256 of the source bytes. */
  sha256: string;
  /** Bytes streamed. Caller may sanity-check against ListObjects size. */
  bytes: number;
}

/**
 * Streams an S3 object once, computing SHA-256. The full body is consumed;
 * use this only when you intend to transcode-or-skip based on the hash.
 */
export async function hashSource(
  client: S3Client,
  bucket: string,
  key: string,
): Promise<HashedSource> {
  const res = await client.send(new GetObjectCommand({ Bucket: bucket, Key: key }));
  if (!res.Body) throw new Error(`Empty body for s3://${bucket}/${key}`);

  // In Node, the SDK's StreamingBlobPayloadOutputTypes resolves to Readable.
  const stream = res.Body as Readable;
  const hash = createHash("sha256");
  let bytes = 0;

  for await (const chunk of stream) {
    const buf: Buffer = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk);
    hash.update(buf);
    bytes += buf.length;
  }

  return {
    sha256: hash.digest("hex"),
    bytes,
  };
}

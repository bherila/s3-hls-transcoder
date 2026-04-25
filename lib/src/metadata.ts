import { GetObjectCommand, PutObjectCommand, type S3Client } from "@aws-sdk/client-s3";
import type { LadderRung } from "./config.js";
import { metadataKey } from "./contentId.js";
import { isNotFound } from "./s3.js";

export interface OutputMetadata {
  contentId: string;
  encoderVersion: string;
  encodedAt: string;
  source: {
    width: number;
    height: number;
    durationSeconds: number;
    bitrateKbps?: number;
  };
  ladder: LadderRung[];
}

export async function readMetadata(
  client: S3Client,
  bucket: string,
  contentId: string,
): Promise<OutputMetadata | null> {
  try {
    const res = await client.send(
      new GetObjectCommand({ Bucket: bucket, Key: metadataKey(contentId) }),
    );
    if (!res.Body) return null;
    return JSON.parse(await res.Body.transformToString()) as OutputMetadata;
  } catch (err) {
    if (isNotFound(err)) return null;
    throw err;
  }
}

export async function writeMetadata(
  client: S3Client,
  bucket: string,
  metadata: OutputMetadata,
): Promise<void> {
  await client.send(
    new PutObjectCommand({
      Bucket: bucket,
      Key: metadataKey(metadata.contentId),
      Body: JSON.stringify(metadata, null, 2),
      ContentType: "application/json",
    }),
  );
}

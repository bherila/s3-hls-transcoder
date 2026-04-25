import { GetObjectCommand, PutObjectCommand, type S3Client } from "@aws-sdk/client-s3";
import {
  deserializeFingerprint,
  fingerprintSimilarity,
  type VideoFingerprint,
} from "./fingerprint.js";
import { isNotFound } from "./s3.js";

const INDEX_KEY = "fingerprints/index.json";

export interface FingerprintIndexEntry {
  contentId: string;
  intervalSeconds: number;
  hashCount: number;
  width: number;
  height: number;
  videoBitrateKbps?: number;
  encodedAt: string;
}

export interface FingerprintIndex {
  version: 1;
  entries: FingerprintIndexEntry[];
}

export function fingerprintKey(contentId: string): string {
  return `fingerprints/${contentId}.bin`;
}

export async function readIndex(client: S3Client, bucket: string): Promise<FingerprintIndex> {
  try {
    const res = await client.send(new GetObjectCommand({ Bucket: bucket, Key: INDEX_KEY }));
    if (!res.Body) return emptyIndex();
    const parsed = JSON.parse(await res.Body.transformToString()) as FingerprintIndex;
    if (parsed.version !== 1) {
      throw new Error(`Unsupported fingerprint index version: ${parsed.version}`);
    }
    return parsed;
  } catch (err) {
    if (isNotFound(err)) return emptyIndex();
    throw err;
  }
}

function emptyIndex(): FingerprintIndex {
  return { version: 1, entries: [] };
}

export async function writeIndex(
  client: S3Client,
  bucket: string,
  index: FingerprintIndex,
): Promise<void> {
  await client.send(
    new PutObjectCommand({
      Bucket: bucket,
      Key: INDEX_KEY,
      Body: JSON.stringify(index, null, 2),
      ContentType: "application/json",
    }),
  );
}

/** Inserts or replaces an index entry by contentId, then writes back. */
export async function upsertIndexEntry(
  client: S3Client,
  bucket: string,
  entry: FingerprintIndexEntry,
): Promise<void> {
  const index = await readIndex(client, bucket);
  const filtered = index.entries.filter((e) => e.contentId !== entry.contentId);
  filtered.push(entry);
  await writeIndex(client, bucket, { version: 1, entries: filtered });
}

export async function uploadFingerprint(
  client: S3Client,
  bucket: string,
  contentId: string,
  blob: Buffer,
): Promise<void> {
  await client.send(
    new PutObjectCommand({
      Bucket: bucket,
      Key: fingerprintKey(contentId),
      Body: blob,
      ContentType: "application/octet-stream",
    }),
  );
}

export async function readFingerprint(
  client: S3Client,
  bucket: string,
  contentId: string,
): Promise<VideoFingerprint | null> {
  try {
    const res = await client.send(
      new GetObjectCommand({ Bucket: bucket, Key: fingerprintKey(contentId) }),
    );
    if (!res.Body) return null;
    const arr = await res.Body.transformToByteArray();
    return deserializeFingerprint(Buffer.from(arr));
  } catch (err) {
    if (isNotFound(err)) return null;
    throw err;
  }
}

export interface PerceptualMatch {
  contentId: string;
  similarity: number;
  entry: FingerprintIndexEntry;
}

/**
 * Linear scan of the fingerprint index. Pre-filters by frame-count ratio
 * (cheap) before downloading + comparing each blob (expensive). Returns the
 * best match at or above threshold, or null.
 *
 * O(N) for now; sufficient for hundreds of videos. For larger libraries,
 * a vector index (e.g., LSH) would be the natural extension.
 */
export async function findPerceptualMatch(
  client: S3Client,
  bucket: string,
  incoming: VideoFingerprint,
  threshold: number,
): Promise<PerceptualMatch | null> {
  const index = await readIndex(client, bucket);
  let best: PerceptualMatch | null = null;

  for (const entry of index.entries) {
    const ratio =
      Math.min(entry.hashCount, incoming.hashes.length) /
      Math.max(entry.hashCount, incoming.hashes.length);
    if (ratio < 0.7) continue;

    const stored = await readFingerprint(client, bucket, entry.contentId);
    if (!stored) continue;

    const similarity = fingerprintSimilarity(incoming, stored);
    if (similarity >= threshold && (!best || similarity > best.similarity)) {
      best = { contentId: entry.contentId, similarity, entry };
    }
  }

  return best;
}

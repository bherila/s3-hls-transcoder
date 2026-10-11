import {
  DeleteObjectCommand,
  GetObjectCommand,
  PutObjectCommand,
  type S3Client,
} from "@aws-sdk/client-s3";
import { findMappingsForContentId } from "./mapping.js";
import { isNotFound } from "./s3.js";

const REFS_PREFIX = "refs/";
const MAX_INDEXED_REFS = 1000;

/**
 * Reverse index for one content ID: the source keys whose mapping currently
 * points at it. Stored at `refs/<contentId>.json`, alongside the other
 * bookkeeping prefixes and deliberately outside `by-id/`: that prefix is served
 * to players, and the index names source keys — which are private, and shared
 * across every source that deduped onto the same content. Keeping it out also
 * leaves `by-id/` genuinely immutable for caching.
 *
 * Up to 1,000 entries make refcount decisions cost one GET per content ID. More
 * highly shared content is marked as overflow so cleanup can safely scan
 * mappings without allowing this object to grow without bound.
 */
export interface ContentRefs {
  version: number;
  contentId: string;
  sourceKeys: string[];
  /** True when this bounded index is incomplete and cleanup must scan mappings. */
  overflow?: boolean;
  updatedAt: string;
}

/** Dest-bucket key of a content ID's reverse index. */
export function refsKey(contentId: string): string {
  return `${REFS_PREFIX}${contentId}.json`;
}

/**
 * Removes a content ID's reverse index. No-op if absent. Called by cleanup when
 * the content is GC'd; the index no longer lives inside the `by-id/` tree, so
 * it is not removed with it.
 */
export async function deleteRefs(
  client: S3Client,
  bucket: string,
  contentId: string,
): Promise<void> {
  try {
    await client.send(new DeleteObjectCommand({ Bucket: bucket, Key: refsKey(contentId) }));
  } catch (err) {
    if (!isNotFound(err)) throw err;
  }
}

/** Returns a content ID's reverse index, or null if it has none. */
export async function readRefs(
  client: S3Client,
  bucket: string,
  contentId: string,
): Promise<ContentRefs | null> {
  let refs: ContentRefs;
  try {
    const res = await client.send(
      new GetObjectCommand({ Bucket: bucket, Key: refsKey(contentId) }),
    );
    if (!res.Body) throw new Error(`Empty body for ${refsKey(contentId)}`);
    refs = JSON.parse(await res.Body.transformToString()) as ContentRefs;
  } catch (err) {
    if (isNotFound(err)) return null;
    throw err;
  }
  if (refs.version !== 1) {
    throw new Error(`Unsupported refs version ${refs.version} for ${contentId}`);
  }
  return refs;
}

/** Writes a bounded, sorted, deduped index; overflow falls back to mappings. */
export async function writeRefs(
  client: S3Client,
  bucket: string,
  contentId: string,
  sourceKeys: readonly string[],
): Promise<void> {
  const uniqueKeys: string[] = [];
  const seen = new Set<string>();
  let overflow = false;
  for (const key of sourceKeys) {
    if (seen.has(key)) continue;
    seen.add(key);
    if (uniqueKeys.length === MAX_INDEXED_REFS) {
      overflow = true;
      break;
    }
    uniqueKeys.push(key);
  }
  uniqueKeys.sort();
  const refs: ContentRefs = {
    version: 1,
    contentId,
    sourceKeys: uniqueKeys,
    ...(overflow ? { overflow: true } : {}),
    updatedAt: new Date().toISOString(),
  };
  await client.send(
    new PutObjectCommand({
      Bucket: bucket,
      Key: refsKey(contentId),
      Body: JSON.stringify(refs, null, 2),
      ContentType: "application/json",
    }),
  );
}

/**
 * Records that `sourceKey`'s mapping points at `contentId`.
 *
 * Call it *before* writing the mapping: the index must never be missing a live
 * reference (cleanup would GC content that is still in use), while an extra
 * reference is harmless because cleanup re-checks each one against the mapping
 * it names and prunes the ones that no longer hold.
 */
export async function addRef(
  client: S3Client,
  bucket: string,
  contentId: string,
  sourceKey: string,
  seedFromScan: boolean,
): Promise<void> {
  const refs = await readRefs(client, bucket, contentId);
  // Once the bounded index overflows, cleanup deliberately falls back to a
  // mappings scan. Avoid rewriting an ever-growing attacker-controlled list
  // on every subsequent dedup hit.
  if (refs?.overflow) return;
  if (refs?.sourceKeys.includes(sourceKey)) return;
  // `seedFromScan` says whether the content pre-dates this write (a dedup hit).
  // If it does, recover its existing references first: seeding the index with
  // only the incoming key would hide mappings written before the index existed
  // and cleanup would then GC content they still point at. For content
  // transcoded in this pass no mapping can reference it yet, and scanning would
  // put an O(N) whole-bucket read on the ingest path — the cost this index
  // exists to avoid.
  const existing =
    refs?.sourceKeys ??
    (seedFromScan ? await findMappingsForContentId(client, bucket, contentId) : []);
  if (existing.length > MAX_INDEXED_REFS) {
    await writeRefs(client, bucket, contentId, existing);
  } else {
    await writeRefs(client, bucket, contentId, [...existing, sourceKey]);
  }
}

/**
 * Drops `sourceKey` from a content ID's reverse index. No-op when the index is
 * absent or does not name the key.
 */
export async function removeRef(
  client: S3Client,
  bucket: string,
  contentId: string,
  sourceKey: string,
): Promise<void> {
  const refs = await readRefs(client, bucket, contentId);
  if (!refs) return;
  // The stored keys are only a sample. Removing from it could incorrectly make
  // the index look complete; cleanup will rebuild it from mappings.
  if (refs.overflow) return;
  const remaining = refs.sourceKeys.filter((k) => k !== sourceKey);
  if (remaining.length === refs.sourceKeys.length) return;
  await writeRefs(client, bucket, contentId, remaining);
}

/**
 * Returns the source keys that reference `contentId`. When no index exists —
 * content transcoded before the reverse index was introduced, or an index
 * deleted out of band — it falls back to the O(N) `mappings/` scan and, when
 * `persist` is true, backfills the index so later passes are O(1).
 */
export async function listRefs(
  client: S3Client,
  bucket: string,
  contentId: string,
  persist: boolean,
): Promise<string[]> {
  const refs = await readRefs(client, bucket, contentId);
  if (refs && !refs.overflow) return refs.sourceKeys;

  const sourceKeys = await findMappingsForContentId(client, bucket, contentId);
  if (persist) await writeRefs(client, bucket, contentId, sourceKeys);
  return sourceKeys;
}

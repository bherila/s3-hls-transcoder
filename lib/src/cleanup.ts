import { DeleteObjectCommand, ListObjectsV2Command, type S3Client } from "@aws-sdk/client-s3";
import { deleteByIdDirectory } from "./dest.js";
import { deleteFingerprint, removeIndexEntry } from "./fingerprintIndex.js";
import type { Logger } from "./logger.js";
import { mappingKey, readMapping } from "./mapping.js";
import { listRefs, writeRefs } from "./refs.js";
import { scanSource } from "./scanner.js";

const MAPPING_PREFIX = "mappings/";
const MAPPING_SUFFIX = ".json";

export interface CleanupOptions {
  sourceClient: S3Client;
  destClient: S3Client;
  sourceBucket: string;
  sourceEndpoint: string;
  destBucket: string;
  /** Restrict cleanup to mappings whose source key starts with this prefix. */
  sourcePrefix?: string;
  logger: Logger;
  /** If true, log proposed actions but make no destructive S3 calls. */
  dryRun?: boolean;
}

export interface CleanupResult {
  orphanMappingsFound: number;
  orphanMappingsDeleted: number;
  contentIdsGcd: number;
  contentIdsRetained: number;
  objectsDeleted: number;
}

/**
 * Removes transcoded output for sources that have been deleted from the
 * source bucket. Refcount-aware: a content ID's by-id/ directory is only
 * GC'd when *all* mappings pointing at it are orphans (no live source still
 * references it). The orphan mappings themselves are always deleted.
 *
 * Run inside the per-pair lock, after the transcoding pass, so any newly
 * written mappings from this run already exist when we count refs.
 *
 * Restricts to mappings tagged with this pair's source bucket and endpoint
 * (plus `sourcePrefix`, if set) so that one pair's cleanup cannot stomp on
 * another pair's mappings if multiple pairs share a dest bucket.
 */
export async function runCleanupPass(opts: CleanupOptions): Promise<CleanupResult> {
  const {
    sourceClient,
    destClient,
    sourceBucket,
    sourceEndpoint,
    destBucket,
    sourcePrefix,
    logger,
    dryRun,
  } = opts;

  logger.info("cleanup: enumerating live source keys", { sourceBucket });
  const liveSources = new Set<string>();
  for await (const obj of scanSource(sourceClient, sourceBucket, {
    ...(sourcePrefix ? { prefix: sourcePrefix } : {}),
    filter: () => true, // everything, not just video extensions
  })) {
    liveSources.add(obj.key);
  }
  logger.info("cleanup: live source count", { count: liveSources.size });

  const orphans = await findOrphanMappings({
    destClient,
    destBucket,
    sourceBucket,
    sourceEndpoint,
    sourcePrefix,
    liveSources,
  });

  if (orphans.length === 0) {
    logger.info("cleanup: no orphan mappings");
    return {
      orphanMappingsFound: 0,
      orphanMappingsDeleted: 0,
      contentIdsGcd: 0,
      contentIdsRetained: 0,
      objectsDeleted: 0,
    };
  }

  logger.info("cleanup: found orphan mappings", { count: orphans.length, dryRun });

  const byContentId = new Map<string, string[]>();
  for (const o of orphans) {
    const list = byContentId.get(o.contentId) ?? [];
    list.push(o.sourceKey);
    byContentId.set(o.contentId, list);
  }

  let contentIdsGcd = 0;
  let contentIdsRetained = 0;
  let objectsDeleted = 0;
  let orphanMappingsDeleted = 0;

  for (const [contentId, orphanSourceKeys] of byContentId) {
    // The reverse index answers "who still points at this content?" in one GET;
    // only content written before the index existed falls back to the full
    // mappings/ scan (and is backfilled by listRefs).
    const refs = await listRefs(destClient, destBucket, contentId, !dryRun);
    const orphanSet = new Set(orphanSourceKeys);
    const liveKeys = await verifyLiveRefs({
      destClient,
      destBucket,
      contentId,
      refs,
      orphanSet,
      logger,
    });
    const liveCount = liveKeys.length;

    if (liveCount === 0) {
      logger.info("cleanup: contentId fully orphaned; gc-ing transcoded output", {
        contentId,
        orphanMappings: orphanSourceKeys.length,
        dryRun,
      });
      contentIdsGcd++;
      if (!dryRun) {
        objectsDeleted += await deleteByIdDirectory(destClient, destBucket, contentId);
        await deleteFingerprint(destClient, destBucket, contentId);
        await removeIndexEntry(destClient, destBucket, contentId);
      }
    } else {
      logger.info("cleanup: contentId still has live references; retaining", {
        contentId,
        liveMappings: liveCount,
        orphanMappings: orphanSourceKeys.length,
        dryRun,
      });
      contentIdsRetained++;
    }

    if (dryRun) {
      orphanMappingsDeleted += orphanSourceKeys.length;
      continue;
    }
    for (const sourceKey of orphanSourceKeys) {
      await destClient.send(
        new DeleteObjectCommand({ Bucket: destBucket, Key: mappingKey(sourceKey) }),
      );
      orphanMappingsDeleted++;
    }
    // Retained content keeps its index; rewrite it without the references just
    // deleted (and any that verification found stale). GC'd content took its
    // index down with the rest of its by-id/ directory.
    if (liveCount > 0 && liveKeys.length !== refs.length) {
      await writeRefs(destClient, destBucket, contentId, liveKeys);
    }
  }

  const result: CleanupResult = {
    orphanMappingsFound: orphans.length,
    orphanMappingsDeleted,
    contentIdsGcd,
    contentIdsRetained,
    objectsDeleted,
  };
  logger.info("cleanup pass complete", { ...result, dryRun });
  return result;
}

async function findOrphanMappings(args: {
  destClient: S3Client;
  destBucket: string;
  sourceBucket: string;
  sourceEndpoint: string;
  sourcePrefix?: string;
  liveSources: Set<string>;
}): Promise<{ sourceKey: string; contentId: string }[]> {
  const { destClient, destBucket, sourceBucket, sourceEndpoint, sourcePrefix, liveSources } = args;
  const orphans: { sourceKey: string; contentId: string }[] = [];
  let token: string | undefined;

  do {
    const res = await destClient.send(
      new ListObjectsV2Command({
        Bucket: destBucket,
        Prefix: MAPPING_PREFIX,
        ContinuationToken: token,
      }),
    );

    for (const obj of res.Contents ?? []) {
      if (!obj.Key || !obj.Key.endsWith(MAPPING_SUFFIX)) continue;
      const sourceKey = obj.Key.slice(
        MAPPING_PREFIX.length,
        obj.Key.length - MAPPING_SUFFIX.length,
      );
      if (sourcePrefix && !sourceKey.startsWith(sourcePrefix)) continue;
      if (liveSources.has(sourceKey)) continue;

      const mapping = await readMapping(destClient, destBucket, sourceKey);
      if (!mapping) continue;
      if (mapping.sourceBucket !== sourceBucket || mapping.sourceEndpoint !== sourceEndpoint) {
        continue;
      }
      orphans.push({ sourceKey, contentId: mapping.contentId });
    }

    token = res.NextContinuationToken;
  } while (token);

  return orphans;
}

/**
 * Returns the reverse-index entries that still hold: a source key not in
 * `orphanSet` whose mapping object exists and still points at `contentId`.
 *
 * The index is deliberately allowed to over-report (a reference is added before
 * its mapping is written and removed after it is deleted), so every retention
 * decision is confirmed against the named mapping — one GET per candidate,
 * against the whole-bucket scan this replaces.
 */
async function verifyLiveRefs(args: {
  destClient: S3Client;
  destBucket: string;
  contentId: string;
  refs: readonly string[];
  orphanSet: ReadonlySet<string>;
  logger: Logger;
}): Promise<string[]> {
  const { destClient, destBucket, contentId, refs, orphanSet, logger } = args;
  const live: string[] = [];
  for (const sourceKey of refs) {
    if (orphanSet.has(sourceKey)) continue;
    const mapping = await readMapping(destClient, destBucket, sourceKey);
    if (mapping?.contentId !== contentId) {
      logger.debug("cleanup: pruning stale reverse-index entry", { contentId, sourceKey });
      continue;
    }
    live.push(sourceKey);
  }
  return live;
}

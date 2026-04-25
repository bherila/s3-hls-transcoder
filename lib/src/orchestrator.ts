import type { S3Client } from "@aws-sdk/client-s3";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";

import type { BucketPair, Config, LadderRung } from "./config.js";
import { runCleanupPass } from "./cleanup.js";
import { byIdPrefix, formatContentId, masterPlaylistKey } from "./contentId.js";
import { deleteByIdDirectory, transcodedOutputExists } from "./dest.js";
import { downloadAndHash } from "./download.js";
import type { ProbeResult } from "./ffmpeg/probe.js";
import { probeSource } from "./ffmpeg/probe.js";
import { transcodeToHls } from "./ffmpeg/transcode.js";
import { fingerprintVideo, serializeFingerprint } from "./fingerprint.js";
import {
  deleteFingerprint,
  findPerceptualMatch,
  removeIndexEntry,
  uploadFingerprint,
  upsertIndexEntry,
  type FingerprintIndexEntry,
} from "./fingerprintIndex.js";
import { acquireLease } from "./lease.js";
import { acquireLock, computeBudgetSeconds, computeLockTtlSeconds } from "./lock.js";
import type { Logger } from "./logger.js";
import {
  findMappingsForContentId,
  isCachedMapping,
  readMapping,
  writeMapping,
  type SourceMapping,
} from "./mapping.js";
import { writeMetadata, type OutputMetadata } from "./metadata.js";
import { createS3Client } from "./s3.js";
import { scanSource, type SourceObject, type ScanOptions } from "./scanner.js";
import { uploadDirectory } from "./uploader.js";
import { VERSION } from "./version.js";

export interface OrchestratorOptions {
  config: Config;
  logger: Logger;
}

export interface RunSummary {
  processed: number;
  cached: number;
  deduped: number;
  busy: number;
  failed: number;
  durationMs: number;
  pairsProcessed: number;
  totalPairs: number;
}

/**
 * Runs all configured bucket pairs sequentially, sharing the runtime budget.
 * Each pair acquires its own global lock on its destination bucket; lock
 * collisions skip that pair's run, not subsequent pairs.
 */
export async function runOnce(opts: OrchestratorOptions): Promise<RunSummary> {
  const { config, logger } = opts;
  const startedAt = Date.now();

  const lockTtl = computeLockTtlSeconds(config.maxRuntimeSeconds, config.lockTtlMultiplier);
  const budget = computeBudgetSeconds(config.maxRuntimeSeconds, config.budgetMultiplier);
  const budgetEndsAt = startedAt + budget * 1000;

  let processed = 0;
  let cached = 0;
  let deduped = 0;
  let busy = 0;
  let failed = 0;
  let pairsProcessed = 0;

  for (let i = 0; i < config.pairs.length; i++) {
    const pair = config.pairs[i]!;
    if (Date.now() > budgetEndsAt) {
      logger.info("budget exhausted before processing remaining pairs", {
        completedPairs: i,
        totalPairs: config.pairs.length,
      });
      break;
    }

    const sourceClient = createS3Client(pair.source);
    const destClient = createS3Client(pair.dest);

    try {
      const result = await runPair({
        pair, config, logger,
        sourceClient, destClient,
        lockTtlSeconds: lockTtl,
        budgetEndsAt,
        pairIndex: i,
      });
      processed += result.processed;
      cached += result.cached;
      deduped += result.deduped;
      busy += result.busy;
      failed += result.failed;
      if (result.acquiredLock) pairsProcessed++;
    } catch (err) {
      failed++;
      logger.error("pair processing failed", {
        pairIndex: i,
        sourceBucket: pair.source.bucket,
        destBucket: pair.dest.bucket,
        error: err instanceof Error ? err.message : String(err),
      });
    } finally {
      sourceClient.destroy();
      destClient.destroy();
    }
  }

  const summary: RunSummary = {
    processed, cached, deduped, busy, failed,
    durationMs: Date.now() - startedAt,
    pairsProcessed,
    totalPairs: config.pairs.length,
  };
  logger.info("run complete", { ...summary });
  return summary;
}

interface PairResult {
  processed: number;
  cached: number;
  deduped: number;
  busy: number;
  failed: number;
  acquiredLock: boolean;
}

async function runPair(args: {
  pair: BucketPair;
  config: Config;
  logger: Logger;
  sourceClient: S3Client;
  destClient: S3Client;
  lockTtlSeconds: number;
  budgetEndsAt: number;
  pairIndex: number;
}): Promise<PairResult> {
  const {
    pair, config, logger, sourceClient, destClient,
    lockTtlSeconds, budgetEndsAt, pairIndex,
  } = args;

  const lock = await acquireLock({
    client: destClient,
    bucket: pair.dest.bucket,
    platform: config.platform,
    maxRuntimeSeconds: config.maxRuntimeSeconds,
    lockTtlSeconds,
    logger,
  });
  if (!lock) {
    return { processed: 0, cached: 0, deduped: 0, busy: 0, failed: 0, acquiredLock: false };
  }

  let processed = 0;
  let cached = 0;
  let deduped = 0;
  let busy = 0;
  let failed = 0;

  try {
    logger.info("starting pair", {
      pairIndex,
      sourceBucket: pair.source.bucket,
      destBucket: pair.dest.bucket,
      ...(pair.source.prefix ? { sourcePrefix: pair.source.prefix } : {}),
    });

    const scanOpts: ScanOptions = {};
    if (pair.source.prefix) scanOpts.prefix = pair.source.prefix;

    for await (const source of scanSource(sourceClient, pair.source.bucket, scanOpts)) {
      if (Date.now() > budgetEndsAt) {
        logger.info("budget exhausted in pair", {
          pairIndex, processed, cached, deduped, busy, failed,
        });
        break;
      }
      try {
        const result = await processSource({
          source, pair, config, sourceClient, destClient, logger,
        });
        if (result === "transcoded") processed++;
        else if (result === "deduped") deduped++;
        else if (result === "cached") cached++;
        else if (result === "lease-busy") busy++;
      } catch (err) {
        failed++;
        logger.error("source processing failed", {
          pairIndex,
          sourceKey: source.key,
          error: err instanceof Error ? err.message : String(err),
          stack: err instanceof Error ? err.stack : undefined,
        });
      }
    }

    // Cleanup pass (refcount-aware orphan GC) — runs after transcoding so
    // any new mappings just written are already counted in refs. Best-effort:
    // failures are logged but don't fail the pair.
    if (config.cleanupDeletedSources && Date.now() < budgetEndsAt) {
      try {
        await runCleanupPass({
          sourceClient,
          destClient,
          sourceBucket: pair.source.bucket,
          destBucket: pair.dest.bucket,
          ...(pair.source.prefix ? { sourcePrefix: pair.source.prefix } : {}),
          logger,
          dryRun: config.cleanupDryRun,
        });
      } catch (err) {
        logger.error("cleanup pass failed", {
          pairIndex,
          error: err instanceof Error ? err.message : String(err),
        });
      }
    }
  } finally {
    await lock.release();
  }

  return { processed, cached, deduped, busy, failed, acquiredLock: true };
}

type ProcessResult = "transcoded" | "deduped" | "cached" | "lease-busy";

async function processSource(args: {
  source: SourceObject;
  pair: BucketPair;
  config: Config;
  sourceClient: S3Client;
  destClient: S3Client;
  logger: Logger;
}): Promise<ProcessResult> {
  const { source, pair, config, sourceClient, destClient, logger } = args;

  // 1. Mapping cache check.
  const existing = await readMapping(destClient, pair.dest.bucket, source.key);
  if (isCachedMapping(existing, { etag: source.etag, size: source.size })) {
    logger.debug("mapping cache hit", { sourceKey: source.key });
    return "cached";
  }

  const tempDir = await mkdtemp(path.join(tmpdir(), "transcoder-"));
  try {
    const localSource = path.join(tempDir, "source");

    // 2. Download + hash.
    logger.info("downloading", { sourceKey: source.key, sizeBytes: source.size });
    const { sha256, bytes } = await downloadAndHash(
      sourceClient, pair.source.bucket, source.key, localSource,
    );
    if (bytes !== source.size) {
      logger.warn("downloaded size differs from listing", {
        sourceKey: source.key, listed: source.size, downloaded: bytes,
      });
    }
    const contentId = formatContentId("sha256", sha256);

    // 3. Byte-hash dedup.
    if (await transcodedOutputExists(destClient, pair.dest.bucket, contentId)) {
      logger.info("byte-hash dedup hit", { sourceKey: source.key, contentId });
      await writeMapping(destClient, pair.dest.bucket, buildMapping(source, contentId));
      return "deduped";
    }

    // 4. Per-video lease.
    const lease = await acquireLease({
      client: destClient,
      bucket: pair.dest.bucket,
      contentId,
      platform: config.platform,
      maxRuntimeSeconds: config.maxRuntimeSeconds,
      lockTtlSeconds: computeLockTtlSeconds(config.maxRuntimeSeconds, config.lockTtlMultiplier),
      logger,
    });
    if (!lease) {
      logger.info("per-video lease busy; skipping", { sourceKey: source.key, contentId });
      return "lease-busy";
    }

    try {
      // 5. Probe.
      const probe = await probeSource(localSource);
      logger.info("probed source", {
        sourceKey: source.key,
        width: probe.width, height: probe.height,
        durationSeconds: Math.round(probe.durationSeconds),
        hasAudio: probe.hasAudio,
      });

      // 6. Effective ladder.
      const effectiveLadder = computeEffectiveLadder(config.ladder, probe.width, probe.height);
      logger.info("effective ladder", { rungs: effectiveLadder.map((r) => r.name) });

      // 7. Perceptual fingerprint.
      const fingerprint = await fingerprintVideo(localSource);

      // 8. Perceptual match.
      const match = await findPerceptualMatch(
        destClient, pair.dest.bucket, fingerprint, config.perceptualThreshold,
      );
      let pendingRepointFrom: string | null = null;
      if (match) {
        const incomingHigher = isHigherQuality(probe, match.entry);
        logger.info("perceptual match", {
          sourceKey: source.key,
          matchedContentId: match.contentId,
          similarity: Number(match.similarity.toFixed(3)),
          stored: { width: match.entry.width, height: match.entry.height,
                    videoBitrateKbps: match.entry.videoBitrateKbps },
          incoming: { width: probe.width, height: probe.height,
                      bitrateKbps: probe.bitrateKbps },
          incomingHigherQuality: incomingHigher,
          dryRun: config.perceptualDryRun,
        });
        if (!config.perceptualDryRun) {
          if (!incomingHigher) {
            await writeMapping(
              destClient, pair.dest.bucket,
              buildMapping(source, match.contentId),
            );
            return "deduped";
          }
          pendingRepointFrom = match.contentId;
        }
      }

      // 9. Transcode.
      const outputDir = path.join(tempDir, "hls");
      logger.info("transcoding", { sourceKey: source.key });
      await transcodeToHls({
        input: localSource, outputDir, ladder: effectiveLadder, hasAudio: probe.hasAudio,
      });

      // 10. Upload HLS tree.
      logger.info("uploading HLS tree", { contentId });
      await uploadDirectory({
        client: destClient,
        bucket: pair.dest.bucket,
        keyPrefix: byIdPrefix(contentId),
        localDir: outputDir,
      });

      // 11. Fingerprint + index.
      await uploadFingerprint(
        destClient, pair.dest.bucket, contentId,
        serializeFingerprint(fingerprint),
      );
      const indexEntry: FingerprintIndexEntry = {
        contentId,
        intervalSeconds: fingerprint.intervalSeconds,
        hashCount: fingerprint.hashes.length,
        width: probe.width,
        height: probe.height,
        encodedAt: new Date().toISOString(),
      };
      if (probe.bitrateKbps !== undefined) indexEntry.videoBitrateKbps = probe.bitrateKbps;
      await upsertIndexEntry(destClient, pair.dest.bucket, indexEntry);

      // 12. Metadata + mapping.
      const metadata: OutputMetadata = {
        contentId,
        encoderVersion: VERSION,
        encodedAt: new Date().toISOString(),
        source: {
          width: probe.width,
          height: probe.height,
          durationSeconds: probe.durationSeconds,
        },
        ladder: effectiveLadder,
      };
      if (probe.bitrateKbps !== undefined) metadata.source.bitrateKbps = probe.bitrateKbps;
      await writeMetadata(destClient, pair.dest.bucket, metadata);
      await writeMapping(destClient, pair.dest.bucket, buildMapping(source, contentId));

      logger.info("transcode complete", { sourceKey: source.key, contentId });

      // 13. Repoint + GC if higher-quality replacement.
      if (pendingRepointFrom) {
        await repointAndGc({
          destClient,
          bucket: pair.dest.bucket,
          oldContentId: pendingRepointFrom,
          newContentId: contentId,
          logger,
        });
      }

      return "transcoded";
    } finally {
      await lease.release();
    }
  } finally {
    await rm(tempDir, { recursive: true, force: true });
  }
}

function buildMapping(source: SourceObject, contentId: string): SourceMapping {
  return {
    sourceKey: source.key,
    sourceEtag: source.etag,
    sourceSize: source.size,
    sourceLastModified: source.lastModified.toISOString(),
    contentId,
    hlsRoot: masterPlaylistKey(contentId),
    encodedAt: new Date().toISOString(),
    encoderVersion: VERSION,
  };
}

function computeEffectiveLadder(
  full: readonly LadderRung[],
  sourceWidth: number,
  sourceHeight: number,
): LadderRung[] {
  const filtered = full.filter((r) => r.width <= sourceWidth && r.height <= sourceHeight);
  if (filtered.length > 0) return filtered;
  return [full[0]!];
}

function isHigherQuality(probe: ProbeResult, stored: FingerprintIndexEntry): boolean {
  const incomingPixels = probe.width * probe.height;
  const storedPixels = stored.width * stored.height;
  if (incomingPixels !== storedPixels) return incomingPixels > storedPixels;
  if (probe.bitrateKbps !== undefined && stored.videoBitrateKbps !== undefined) {
    return probe.bitrateKbps > stored.videoBitrateKbps;
  }
  return false;
}

async function repointAndGc(args: {
  destClient: S3Client;
  bucket: string;
  oldContentId: string;
  newContentId: string;
  logger: Logger;
}): Promise<void> {
  const { destClient, bucket, oldContentId, newContentId, logger } = args;
  const sourceKeys = await findMappingsForContentId(destClient, bucket, oldContentId);
  logger.info("repointing mappings to new transcoded output", {
    oldContentId, newContentId, mappingCount: sourceKeys.length,
  });

  for (const sourceKey of sourceKeys) {
    const old = await readMapping(destClient, bucket, sourceKey);
    if (!old) continue;
    const updated: SourceMapping = {
      ...old,
      contentId: newContentId,
      hlsRoot: masterPlaylistKey(newContentId),
      encoderVersion: VERSION,
    };
    await writeMapping(destClient, bucket, updated);
  }

  logger.info("garbage-collecting superseded transcoded output", { oldContentId });
  const deletedCount = await deleteByIdDirectory(destClient, bucket, oldContentId);
  await deleteFingerprint(destClient, bucket, oldContentId);
  await removeIndexEntry(destClient, bucket, oldContentId);
  logger.info("perceptual upgrade complete", {
    oldContentId, newContentId, deletedObjects: deletedCount,
  });
}

import type { S3Client } from "@aws-sdk/client-s3";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";

import type { Config, LadderRung } from "./config.js";
import { byIdPrefix, formatContentId, masterPlaylistKey } from "./contentId.js";
import { transcodedOutputExists } from "./dest.js";
import { downloadAndHash } from "./download.js";
import { probeSource } from "./ffmpeg/probe.js";
import { transcodeToHls } from "./ffmpeg/transcode.js";
import { fingerprintVideo, serializeFingerprint } from "./fingerprint.js";
import {
  findPerceptualMatch,
  uploadFingerprint,
  upsertIndexEntry,
  type FingerprintIndexEntry,
} from "./fingerprintIndex.js";
import { acquireLease } from "./lease.js";
import { acquireLock, computeBudgetSeconds, computeLockTtlSeconds } from "./lock.js";
import type { Logger } from "./logger.js";
import { isCachedMapping, readMapping, writeMapping, type SourceMapping } from "./mapping.js";
import { writeMetadata, type OutputMetadata } from "./metadata.js";
import { scanSource, type SourceObject, type ScanOptions } from "./scanner.js";
import { uploadDirectory } from "./uploader.js";
import { VERSION } from "./version.js";

export interface OrchestratorOptions {
  config: Config;
  sourceClient: S3Client;
  destClient: S3Client;
  logger: Logger;
}

export interface RunSummary {
  processed: number;
  cached: number;
  deduped: number;
  busy: number;
  failed: number;
  durationMs: number;
  acquiredLock: boolean;
}

/**
 * Single transcoding pass: acquire global lock, scan source bucket, process
 * each unprocessed source until budget runs out, release lock.
 */
export async function runOnce(opts: OrchestratorOptions): Promise<RunSummary> {
  const { config, sourceClient, destClient, logger } = opts;
  const startedAt = Date.now();

  const lockTtl = computeLockTtlSeconds(config.maxRuntimeSeconds, config.lockTtlMultiplier);
  const budget = computeBudgetSeconds(config.maxRuntimeSeconds, config.budgetMultiplier);
  const budgetExpiresAt = startedAt + budget * 1000;

  const lock = await acquireLock({
    client: destClient,
    bucket: config.dest.bucket,
    platform: config.platform,
    maxRuntimeSeconds: config.maxRuntimeSeconds,
    lockTtlSeconds: lockTtl,
    logger,
  });
  if (!lock) {
    return summarize(startedAt, false, 0, 0, 0, 0, 0);
  }

  let processed = 0;
  let cached = 0;
  let deduped = 0;
  let busy = 0;
  let failed = 0;

  try {
    const scanOpts: ScanOptions = {};
    if (config.source.prefix) scanOpts.prefix = config.source.prefix;

    for await (const source of scanSource(sourceClient, config.source.bucket, scanOpts)) {
      if (Date.now() > budgetExpiresAt) {
        logger.info("budget exhausted; exiting current run", {
          processed, cached, deduped, busy, failed,
        });
        break;
      }

      try {
        const result = await processSource({ source, config, sourceClient, destClient, logger });
        if (result === "transcoded") processed++;
        else if (result === "deduped") deduped++;
        else if (result === "cached") cached++;
        else if (result === "lease-busy") busy++;
      } catch (err) {
        failed++;
        logger.error("source processing failed", {
          sourceKey: source.key,
          error: err instanceof Error ? err.message : String(err),
          stack: err instanceof Error ? err.stack : undefined,
        });
      }
    }
  } finally {
    await lock.release();
  }

  const summary = summarize(startedAt, true, processed, cached, deduped, busy, failed);
  logger.info("run complete", { ...summary });
  return summary;
}

function summarize(
  startedAt: number,
  acquiredLock: boolean,
  processed: number,
  cached: number,
  deduped: number,
  busy: number,
  failed: number,
): RunSummary {
  return {
    processed, cached, deduped, busy, failed,
    durationMs: Date.now() - startedAt,
    acquiredLock,
  };
}

type ProcessResult = "transcoded" | "deduped" | "cached" | "lease-busy";

async function processSource(args: {
  source: SourceObject;
  config: Config;
  sourceClient: S3Client;
  destClient: S3Client;
  logger: Logger;
}): Promise<ProcessResult> {
  const { source, config, sourceClient, destClient, logger } = args;

  // 1. Mapping cache check.
  const existing = await readMapping(destClient, config.dest.bucket, source.key);
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
      sourceClient, config.source.bucket, source.key, localSource,
    );
    if (bytes !== source.size) {
      logger.warn("downloaded size differs from listing", {
        sourceKey: source.key, listed: source.size, downloaded: bytes,
      });
    }
    const contentId = formatContentId("sha256", sha256);

    // 3. Byte-hash dedup.
    if (await transcodedOutputExists(destClient, config.dest.bucket, contentId)) {
      logger.info("byte-hash dedup hit", { sourceKey: source.key, contentId });
      await writeMapping(destClient, config.dest.bucket, buildMapping(source, contentId));
      return "deduped";
    }

    // 4. Per-video lease.
    const lease = await acquireLease({
      client: destClient,
      bucket: config.dest.bucket,
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

      // 6. Effective ladder: skip rungs above source resolution.
      const effectiveLadder = computeEffectiveLadder(config.ladder, probe.width, probe.height);
      logger.info("effective ladder", { rungs: effectiveLadder.map((r) => r.name) });

      // 7. Perceptual fingerprint.
      const fingerprint = await fingerprintVideo(localSource);

      // 8. Perceptual match check.
      // v1 logs the match for observability. Quality-comparison + repoint of
      // existing mappings (when incoming is higher resolution) lands in a
      // follow-up commit. Until then, every miss-on-byte-hash transcodes.
      const match = await findPerceptualMatch(
        destClient, config.dest.bucket, fingerprint, config.perceptualThreshold,
      );
      if (match) {
        logger.info("perceptual match (informational; no action in v1)", {
          sourceKey: source.key,
          matchedContentId: match.contentId,
          similarity: Number(match.similarity.toFixed(3)),
          dryRun: config.perceptualDryRun,
        });
      }

      // 9. Transcode.
      const outputDir = path.join(tempDir, "hls");
      logger.info("transcoding", { sourceKey: source.key });
      await transcodeToHls({
        input: localSource,
        outputDir,
        ladder: effectiveLadder,
        hasAudio: probe.hasAudio,
      });

      // 10. Upload HLS tree.
      logger.info("uploading HLS tree", { contentId });
      await uploadDirectory({
        client: destClient,
        bucket: config.dest.bucket,
        keyPrefix: byIdPrefix(contentId),
        localDir: outputDir,
      });

      // 11. Upload fingerprint + index entry.
      await uploadFingerprint(
        destClient, config.dest.bucket, contentId,
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
      await upsertIndexEntry(destClient, config.dest.bucket, indexEntry);

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
      await writeMetadata(destClient, config.dest.bucket, metadata);
      await writeMapping(destClient, config.dest.bucket, buildMapping(source, contentId));

      logger.info("transcode complete", { sourceKey: source.key, contentId });
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
  // Source is smaller than the smallest rung. Encode at the smallest rung
  // anyway; ffmpeg's force_original_aspect_ratio=decrease + pad keeps it sane.
  return [full[0]!];
}

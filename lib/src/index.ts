// Version
export { VERSION } from "./version.js";

// Config
export type { Config, BucketConfig, LadderRung, Platform } from "./config.js";
export { loadConfig, DEFAULT_LADDER } from "./config.js";

// Logging
export type { Logger, LogLevel } from "./logger.js";
export { createLogger } from "./logger.js";

// S3 client
export { createS3Client, isPreconditionFailed, isNotFound } from "./s3.js";

// Mapping I/O
export type { SourceMapping } from "./mapping.js";
export { readMapping, writeMapping, mappingKey, isCachedMapping } from "./mapping.js";

// Lock + lease
export type { LockBody, LockHandle, AcquireOptions } from "./lock.js";
export {
  acquireLock,
  GLOBAL_LOCK_KEY,
  computeLockTtlSeconds,
  computeBudgetSeconds,
} from "./lock.js";
export { acquireLease, leaseKey } from "./lease.js";

// Content addressing
export type { ContentIdScheme } from "./contentId.js";
export {
  formatContentId,
  parseContentId,
  byIdPrefix,
  masterPlaylistKey,
  metadataKey,
} from "./contentId.js";

// Source bucket scanning
export type { SourceObject, ScanOptions } from "./scanner.js";
export { scanSource, isVideoKey } from "./scanner.js";

// Source byte hashing
export type { HashedSource } from "./hasher.js";
export { hashSource } from "./hasher.js";

// Destination existence check
export { transcodedOutputExists } from "./dest.js";

// Output metadata
export type { OutputMetadata } from "./metadata.js";
export { readMetadata, writeMetadata } from "./metadata.js";

// Process spawn helper
export { runProcess } from "./process.js";

// ffmpeg
export { findFfmpeg, findFfprobe } from "./ffmpeg/binary.js";
export type { ProbeResult } from "./ffmpeg/probe.js";
export { probeSource } from "./ffmpeg/probe.js";
export { extractSignature } from "./ffmpeg/signature.js";
export type { TranscodeOptions } from "./ffmpeg/transcode.js";
export { transcodeToHls } from "./ffmpeg/transcode.js";

// Download (stream-and-hash)
export type { DownloadResult } from "./download.js";
export { downloadAndHash } from "./download.js";

// Upload
export type { UploadDirectoryOptions } from "./uploader.js";
export { uploadDirectory, contentTypeFor } from "./uploader.js";

// Perceptual fingerprint
export type { VideoFingerprint } from "./fingerprint.js";
export {
  fingerprintVideo,
  fingerprintSimilarity,
  serializeFingerprint,
  deserializeFingerprint,
  popcount64,
} from "./fingerprint.js";

// Fingerprint index
export type {
  FingerprintIndex,
  FingerprintIndexEntry,
  PerceptualMatch,
} from "./fingerprintIndex.js";
export {
  readIndex,
  writeIndex,
  upsertIndexEntry,
  uploadFingerprint,
  readFingerprint,
  findPerceptualMatch,
  fingerprintKey,
} from "./fingerprintIndex.js";

// Orchestrator
export type { OrchestratorOptions, RunSummary } from "./orchestrator.js";
export { runOnce } from "./orchestrator.js";

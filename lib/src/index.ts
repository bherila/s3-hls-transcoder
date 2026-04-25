export const VERSION = "0.1.0";

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

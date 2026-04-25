import { readFileSync } from "node:fs";

export type Platform = "aws-lambda" | "cloudflare-container" | "local";

export interface BucketConfig {
  bucket: string;
  endpoint: string;
  accessKeyId: string;
  secretAccessKey: string;
  region: string;
  prefix?: string;
}

export interface BucketPair {
  source: BucketConfig;
  dest: BucketConfig;
}

export interface LadderRung {
  name: string;
  width: number;
  height: number;
  videoBitrateKbps: number;
  audioBitrateKbps: number;
}

export interface Config {
  pairs: BucketPair[];
  ladder: LadderRung[];
  maxRuntimeSeconds: number;
  lockTtlMultiplier: number;
  budgetMultiplier: number;
  perceptualThreshold: number;
  perceptualDryRun: boolean;
  cleanupDeletedSources: boolean;
  cleanupDryRun: boolean;
  maxConcurrency: number;
  logLevel: "debug" | "info" | "warn" | "error";
  platform: Platform;
}

export const DEFAULT_LADDER: readonly LadderRung[] = [
  { name: "360p",  width: 640,  height: 360,  videoBitrateKbps: 800,  audioBitrateKbps: 96  },
  { name: "480p",  width: 854,  height: 480,  videoBitrateKbps: 1400, audioBitrateKbps: 128 },
  { name: "720p",  width: 1280, height: 720,  videoBitrateKbps: 2800, audioBitrateKbps: 128 },
  { name: "1080p", width: 1920, height: 1080, videoBitrateKbps: 5000, audioBitrateKbps: 192 },
];

const PLATFORM_DEFAULT_MAX_RUNTIME = {
  "aws-lambda": 900,
  "cloudflare-container": 3600,
  local: 3600,
} as const satisfies Record<Platform, number>;

// JSON shape for BUCKETS_CONFIG / BUCKETS_CONFIG_FILE inputs.
interface BucketInput {
  bucket: string;
  endpoint: string;
  accessKeyId?: string;
  secretAccessKey?: string;
  region?: string;
  prefix?: string;
}

interface PairInput {
  source: BucketInput;
  dest: BucketInput;
  // Pair-level credentials (apply when bucket-level credentials not set).
  accessKeyId?: string;
  secretAccessKey?: string;
  region?: string;
}

export function loadConfig(platform: Platform): Config {
  const platformDefaultRuntime = PLATFORM_DEFAULT_MAX_RUNTIME[platform];
  const pairs = loadPairs();
  validateNoOverlaps(pairs);

  return {
    pairs,
    ladder: parseLadder(process.env.HLS_LADDER),
    maxRuntimeSeconds: parseNumber("MAX_RUNTIME_SECONDS", process.env.MAX_RUNTIME_SECONDS, platformDefaultRuntime),
    lockTtlMultiplier: parseNumber("LOCK_TTL_MULTIPLIER", process.env.LOCK_TTL_MULTIPLIER, 1.5),
    budgetMultiplier: parseNumber("BUDGET_MULTIPLIER", process.env.BUDGET_MULTIPLIER, 0.75),
    perceptualThreshold: parseNumber("PERCEPTUAL_THRESHOLD", process.env.PERCEPTUAL_THRESHOLD, 0.95),
    perceptualDryRun: process.env.PERCEPTUAL_DRY_RUN === "true",
    cleanupDeletedSources: process.env.CLEANUP_DELETED_SOURCES === "true",
    cleanupDryRun: process.env.CLEANUP_DRY_RUN === "true",
    maxConcurrency: parseNumber("MAX_CONCURRENCY", process.env.MAX_CONCURRENCY, 1),
    logLevel: parseLogLevel(process.env.LOG_LEVEL),
    platform,
  };
}

/**
 * Resolves bucket pairs from env in priority order:
 *  1. BUCKETS_CONFIG_FILE — path to a JSON file containing an array of pairs.
 *  2. BUCKETS_CONFIG — JSON literal containing an array of pairs.
 *  3. Single pair built from SOURCE_BUCKET / DEST_BUCKET / etc env vars.
 *
 * In structured pair JSON, credentials cascade: bucket-level overrides
 * pair-level overrides env-level fallback (SOURCE_ACCESS_KEY_ID etc).
 */
function loadPairs(): BucketPair[] {
  if (process.env.BUCKETS_CONFIG_FILE) {
    const raw = readFileSync(process.env.BUCKETS_CONFIG_FILE, "utf-8");
    return parseBucketsConfigJson(raw, "BUCKETS_CONFIG_FILE");
  }
  if (process.env.BUCKETS_CONFIG) {
    return parseBucketsConfigJson(process.env.BUCKETS_CONFIG, "BUCKETS_CONFIG");
  }
  return [singlePairFromEnv()];
}

function parseBucketsConfigJson(raw: string, sourceName: string): BucketPair[] {
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch (err) {
    throw new Error(`${sourceName} is not valid JSON: ${err instanceof Error ? err.message : String(err)}`);
  }
  if (!Array.isArray(parsed) || parsed.length === 0) {
    throw new Error(`${sourceName} must be a non-empty JSON array of bucket pairs`);
  }
  return parsed.map((p, i) => resolvePair(p as PairInput, i, sourceName));
}

function resolvePair(input: PairInput, idx: number, sourceName: string): BucketPair {
  if (!input || typeof input !== "object") {
    throw new Error(`${sourceName}[${idx}] must be an object with source and dest`);
  }
  if (!input.source || !input.dest) {
    throw new Error(`${sourceName}[${idx}] must have both 'source' and 'dest' fields`);
  }
  return {
    source: resolveBucket(input.source, input, "source", idx, sourceName),
    dest: resolveBucket(input.dest, input, "dest", idx, sourceName),
  };
}

function resolveBucket(
  bucket: BucketInput,
  pair: PairInput,
  side: "source" | "dest",
  idx: number,
  sourceName: string,
): BucketConfig {
  if (!bucket.bucket) {
    throw new Error(`${sourceName}[${idx}].${side}.bucket is required`);
  }
  if (!bucket.endpoint) {
    throw new Error(`${sourceName}[${idx}].${side}.endpoint is required for bucket '${bucket.bucket}'`);
  }

  // Credential cascade: bucket-level → pair-level → env-level.
  const envPrefix = side === "source" ? "SOURCE" : "DEST";
  const accessKeyId =
    bucket.accessKeyId ?? pair.accessKeyId ?? process.env[`${envPrefix}_ACCESS_KEY_ID`];
  const secretAccessKey =
    bucket.secretAccessKey ?? pair.secretAccessKey ?? process.env[`${envPrefix}_SECRET_ACCESS_KEY`];
  const region =
    bucket.region ?? pair.region ?? process.env[`${envPrefix}_REGION`] ?? "auto";

  if (!accessKeyId || !secretAccessKey) {
    throw new Error(
      `${sourceName}[${idx}].${side} bucket '${bucket.bucket}' is missing credentials. ` +
        `Provide accessKeyId/secretAccessKey at bucket level, pair level, or via ` +
        `${envPrefix}_ACCESS_KEY_ID / ${envPrefix}_SECRET_ACCESS_KEY env vars.`,
    );
  }

  const result: BucketConfig = {
    bucket: bucket.bucket,
    endpoint: bucket.endpoint,
    accessKeyId,
    secretAccessKey,
    region,
  };
  if (bucket.prefix) result.prefix = bucket.prefix;
  return result;
}

function singlePairFromEnv(): BucketPair {
  const source: BucketConfig = {
    bucket: required("SOURCE_BUCKET"),
    endpoint: required("SOURCE_ENDPOINT"),
    accessKeyId: required("SOURCE_ACCESS_KEY_ID"),
    secretAccessKey: required("SOURCE_SECRET_ACCESS_KEY"),
    region: optional("SOURCE_REGION", "auto"),
  };
  if (process.env.SOURCE_PREFIX) source.prefix = process.env.SOURCE_PREFIX;

  const dest: BucketConfig = {
    bucket: required("DEST_BUCKET"),
    endpoint: required("DEST_ENDPOINT"),
    accessKeyId: required("DEST_ACCESS_KEY_ID"),
    secretAccessKey: required("DEST_SECRET_ACCESS_KEY"),
    region: optional("DEST_REGION", "auto"),
  };

  return { source, dest };
}

/**
 * Refuses to run if any source bucket overlaps with any destination bucket.
 * Two buckets "overlap" when they share endpoint + bucket name AND one's
 * prefix is a prefix of the other (including empty prefix). Source vs.
 * source and dest vs. dest are intentionally allowed (multiple pairs may
 * legitimately share either side).
 */
function validateNoOverlaps(pairs: BucketPair[]): void {
  if (pairs.length === 0) {
    throw new Error("At least one bucket pair must be configured");
  }
  for (let i = 0; i < pairs.length; i++) {
    for (let j = 0; j < pairs.length; j++) {
      const src = pairs[i]!.source;
      const dst = pairs[j]!.dest;
      if (bucketsOverlap(src, dst)) {
        throw new Error(
          `Configuration error: source pairs[${i}] '${src.bucket}' (prefix='${src.prefix ?? ""}') ` +
            `at ${src.endpoint} overlaps with destination pairs[${j}] '${dst.bucket}' ` +
            `(prefix='${dst.prefix ?? ""}') at ${dst.endpoint}. ` +
            `Source and destination must be disjoint to avoid corruption.`,
        );
      }
    }
  }
}

export function bucketsOverlap(a: BucketConfig, b: BucketConfig): boolean {
  if (normalizeEndpoint(a.endpoint) !== normalizeEndpoint(b.endpoint)) return false;
  if (a.bucket !== b.bucket) return false;
  const aPrefix = a.prefix ?? "";
  const bPrefix = b.prefix ?? "";
  return aPrefix.startsWith(bPrefix) || bPrefix.startsWith(aPrefix);
}

function normalizeEndpoint(s: string): string {
  try {
    const u = new URL(s);
    // Lowercase scheme + host + (port if present); ignore path.
    return `${u.protocol}//${u.host}`.toLowerCase();
  } catch {
    return s.toLowerCase();
  }
}

function required(name: string): string {
  const v = process.env[name];
  if (!v) throw new Error(`Missing required env var: ${name}`);
  return v;
}

function optional(name: string, fallback: string): string {
  return process.env[name] ?? fallback;
}

function parseLadder(raw: string | undefined): LadderRung[] {
  if (!raw) return [...DEFAULT_LADDER];
  const parsed = JSON.parse(raw);
  if (!Array.isArray(parsed) || parsed.length === 0) {
    throw new Error("HLS_LADDER must be a non-empty JSON array");
  }
  for (const rung of parsed) {
    if (
      typeof rung !== "object" || rung === null ||
      typeof rung.name !== "string" ||
      typeof rung.width !== "number" ||
      typeof rung.height !== "number" ||
      typeof rung.videoBitrateKbps !== "number" ||
      typeof rung.audioBitrateKbps !== "number"
    ) {
      throw new Error(`HLS_LADDER rung is malformed: ${JSON.stringify(rung)}`);
    }
  }
  return parsed as LadderRung[];
}

function parseLogLevel(raw: string | undefined): Config["logLevel"] {
  const v = (raw ?? "info").toLowerCase();
  if (v !== "debug" && v !== "info" && v !== "warn" && v !== "error") {
    throw new Error(`LOG_LEVEL must be debug|info|warn|error, got: ${raw}`);
  }
  return v;
}

function parseNumber(name: string, raw: string | undefined, fallback: number): number {
  if (raw === undefined || raw === "") return fallback;
  const n = Number(raw);
  if (!Number.isFinite(n)) throw new Error(`${name} must be numeric, got: ${raw}`);
  return n;
}

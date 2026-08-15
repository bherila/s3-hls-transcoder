/**
 * End-to-end integration test for runOnce().
 *
 * Requires:
 *   - INTEGRATION=1
 *   - Docker daemon running (for testcontainers)
 *   - ffmpeg on PATH (or FFMPEG_PATH set)
 *
 * Run with:
 *   INTEGRATION=1 pnpm --filter @s3-hls-transcoder/integration test
 */

import {
  CreateBucketCommand,
  GetObjectCommand,
  HeadObjectCommand,
  ListObjectsV2Command,
  PutObjectCommand,
  S3Client,
} from "@aws-sdk/client-s3";
import { readFile } from "node:fs/promises";
import path from "node:path";
import { GenericContainer, Wait } from "testcontainers";
import { afterAll, beforeAll, describe, expect, it } from "vitest";

import type { Config } from "@s3-hls-transcoder/lib";
import { GLOBAL_LOCK_KEY, mappingKey, masterPlaylistKey, runOnce } from "@s3-hls-transcoder/lib";

// ---------------------------------------------------------------------------
// Guard: skip the whole suite when INTEGRATION=1 is absent.
// (globalSetup also prints a notice; this is belt-and-suspenders inside the
//  worker process where the env var may not be forwarded in all Vitest modes.)
// ---------------------------------------------------------------------------
const INTEGRATION = process.env["INTEGRATION"] === "1";

// ---------------------------------------------------------------------------
// Shared state set up / torn down once per describe block.
// ---------------------------------------------------------------------------

const MINIO_IMAGE = "minio/minio:latest";
const MINIO_ACCESS_KEY = "minioadmin";
const MINIO_SECRET_KEY = "minioadmin";
const MINIO_PORT = 9000;
const SOURCE_BUCKET = "source";
const DEST_BUCKET = "dest";
const SOURCE_KEY = "videos/test.mp4";

describe.skipIf(!INTEGRATION)("runOnce() integration", () => {
  let endpoint: string;
  let s3: S3Client;
  let stopContainer: (() => Promise<unknown>) | undefined;

  beforeAll(async () => {
    // ------------------------------------------------------------------
    // 1. Start MinIO container.
    // ------------------------------------------------------------------
    const container = await new GenericContainer(MINIO_IMAGE)
      .withExposedPorts(MINIO_PORT)
      .withCommand(["server", "/data"])
      .withEnvironment({
        MINIO_ROOT_USER: MINIO_ACCESS_KEY,
        MINIO_ROOT_PASSWORD: MINIO_SECRET_KEY,
      })
      .withWaitStrategy(Wait.forHttp("/minio/health/live", MINIO_PORT))
      .start();

    const mappedPort = container.getMappedPort(MINIO_PORT);
    endpoint = `http://127.0.0.1:${mappedPort}`;
    stopContainer = () => container.stop();

    // ------------------------------------------------------------------
    // 2. Create source + destination buckets.
    // ------------------------------------------------------------------
    s3 = new S3Client({
      endpoint,
      region: "us-east-1",
      credentials: {
        accessKeyId: MINIO_ACCESS_KEY,
        secretAccessKey: MINIO_SECRET_KEY,
      },
      forcePathStyle: true,
    });

    await s3.send(new CreateBucketCommand({ Bucket: SOURCE_BUCKET }));
    await s3.send(new CreateBucketCommand({ Bucket: DEST_BUCKET }));

    // ------------------------------------------------------------------
    // 3. Upload the pre-generated fixture MP4 to the source bucket.
    // ------------------------------------------------------------------
    const fixturePath = process.env["FIXTURE_MP4"];
    if (!fixturePath) {
      throw new Error(
        "FIXTURE_MP4 env var is not set — globalSetup likely did not run. " +
          "Make sure INTEGRATION=1 is set and you are running via `vitest run`.",
      );
    }
    const fixtureBytes = await readFile(fixturePath);
    await s3.send(
      new PutObjectCommand({
        Bucket: SOURCE_BUCKET,
        Key: SOURCE_KEY,
        Body: fixtureBytes,
        ContentType: "video/mp4",
      }),
    );
  }, 120_000); // container startup can be slow on first pull

  afterAll(async () => {
    s3.destroy();
    await stopContainer?.();
  });

  // -----------------------------------------------------------------------
  // Helpers
  // -----------------------------------------------------------------------

  function makeConfig(): Config {
    const bucketCfg = {
      endpoint,
      accessKeyId: MINIO_ACCESS_KEY,
      secretAccessKey: MINIO_SECRET_KEY,
      region: "us-east-1",
    };
    return {
      pairs: [
        {
          source: { ...bucketCfg, bucket: SOURCE_BUCKET },
          dest: { ...bucketCfg, bucket: DEST_BUCKET },
        },
      ],
      ladder: [
        // Single small rung — fast enough for a test fixture.
        { name: "240p", width: 426, height: 240, videoBitrateKbps: 400, audioBitrateKbps: 64 },
      ],
      maxRuntimeSeconds: 240,
      lockTtlMultiplier: 1.5,
      budgetMultiplier: 0.9,
      perceptualThreshold: 0.95,
      perceptualDryRun: false,
      cleanupDeletedSources: false,
      cleanupDryRun: false,
      maxConcurrency: 1,
      maxSourceSizeBytes: 1024 * 1024 * 1024,
      maxVideoDurationSeconds: 60,
      logLevel: "info",
      platform: "local",
    };
  }

  async function objectExists(bucket: string, key: string): Promise<boolean> {
    try {
      await s3.send(new HeadObjectCommand({ Bucket: bucket, Key: key }));
      return true;
    } catch {
      return false;
    }
  }

  async function listObjects(bucket: string, prefix?: string): Promise<string[]> {
    const keys: string[] = [];
    let continuationToken: string | undefined;
    do {
      const res = await s3.send(
        new ListObjectsV2Command({
          Bucket: bucket,
          Prefix: prefix,
          ContinuationToken: continuationToken,
        }),
      );
      for (const obj of res.Contents ?? []) {
        if (obj.Key) keys.push(obj.Key);
      }
      continuationToken = res.NextContinuationToken;
    } while (continuationToken);
    return keys;
  }

  async function getJson(bucket: string, key: string): Promise<unknown> {
    const res = await s3.send(new GetObjectCommand({ Bucket: bucket, Key: key }));
    if (!res.Body) throw new Error(`Empty body for ${key}`);
    const text = await res.Body.transformToString();
    return JSON.parse(text) as unknown;
  }

  // -----------------------------------------------------------------------
  // Tests
  // -----------------------------------------------------------------------

  it("processes the source video and returns processed=1", async () => {
    const logger = {
      debug: () => {},
      info: (msg: string, fields?: Record<string, unknown>) =>
        console.log("[lib]", msg, fields ?? ""),
      warn: (msg: string, fields?: Record<string, unknown>) =>
        console.warn("[lib]", msg, fields ?? ""),
      error: (msg: string, fields?: Record<string, unknown>) =>
        console.error("[lib]", msg, fields ?? ""),
    };

    const summary = await runOnce({ config: makeConfig(), logger });

    expect(summary.processed).toBe(1);
    expect(summary.cached).toBe(0);
    expect(summary.deduped).toBe(0);
    expect(summary.failed).toBe(0);
    expect(summary.pairsProcessed).toBe(1);
  }, 240_000);

  it("creates master.m3u8 in by-id/<contentId>/", async () => {
    const keys = await listObjects(DEST_BUCKET, "by-id/");
    const masterKeys = keys.filter((k) => k.endsWith("master.m3u8"));
    expect(masterKeys).toHaveLength(1);

    // Verify the content is a valid M3U8 master playlist.
    const content = await getJson(DEST_BUCKET, masterKeys[0]!).catch(async () => {
      // master.m3u8 is not JSON — read raw text.
      const res = await s3.send(new GetObjectCommand({ Bucket: DEST_BUCKET, Key: masterKeys[0]! }));
      return res.Body!.transformToString();
    });
    const text = typeof content === "string" ? content : String(content);
    expect(text).toContain("#EXTM3U");
    expect(text).toContain("#EXT-X-STREAM-INF");
  });

  it("creates the mapping file at mappings/<sourceKey>.json", async () => {
    const key = mappingKey(SOURCE_KEY);
    const exists = await objectExists(DEST_BUCKET, key);
    expect(exists).toBe(true);

    const mapping = (await getJson(DEST_BUCKET, key)) as Record<string, unknown>;
    expect(mapping["sourceKey"]).toBe(SOURCE_KEY);
    expect(typeof mapping["contentId"]).toBe("string");
    expect((mapping["contentId"] as string).startsWith("sha256:")).toBe(true);
    expect(typeof mapping["hlsRoot"]).toBe("string");
    expect((mapping["hlsRoot"] as string).endsWith("master.m3u8")).toBe(true);
  });

  it("releases the global lock after completion (.transcoder.lock absent)", async () => {
    const lockExists = await objectExists(DEST_BUCKET, GLOBAL_LOCK_KEY);
    expect(lockExists).toBe(false);
  });

  it("returns cached=1 on a second run with the same source", async () => {
    const logger = {
      debug: () => {},
      info: () => {},
      warn: () => {},
      error: (msg: string, fields?: Record<string, unknown>) =>
        console.error("[lib]", msg, fields ?? ""),
    };

    const summary = await runOnce({ config: makeConfig(), logger });

    expect(summary.cached).toBe(1);
    expect(summary.processed).toBe(0);
    expect(summary.failed).toBe(0);
  }, 60_000);

  it("master.m3u8 path matches the hlsRoot field in the mapping", async () => {
    const mappingObj = (await getJson(DEST_BUCKET, mappingKey(SOURCE_KEY))) as Record<
      string,
      unknown
    >;
    const hlsRoot = mappingObj["hlsRoot"] as string;
    const contentId = mappingObj["contentId"] as string;

    // masterPlaylistKey(contentId) should equal hlsRoot.
    expect(hlsRoot).toBe(masterPlaylistKey(contentId));

    // The object should actually exist in the dest bucket.
    const exists = await objectExists(DEST_BUCKET, hlsRoot);
    expect(exists).toBe(true);
  });

  it("creates per-variant playlists and at least one segment", async () => {
    const keys = await listObjects(DEST_BUCKET, "by-id/");
    const variantPlaylists = keys.filter(
      (k) => k.endsWith("index.m3u8") && !k.includes("master.m3u8"),
    );
    expect(variantPlaylists.length).toBeGreaterThanOrEqual(1);

    const segments = keys.filter((k) => k.endsWith(".m4s") || k.endsWith(".ts"));
    expect(segments.length).toBeGreaterThanOrEqual(1);
  });

  it("uploads a fingerprint and fingerprint index for the content ID", async () => {
    const mappingObj = (await getJson(DEST_BUCKET, mappingKey(SOURCE_KEY))) as Record<
      string,
      unknown
    >;
    const contentId = mappingObj["contentId"] as string;

    // Fingerprint file: fingerprints/<contentId>.bin (or similar).
    const keys = await listObjects(DEST_BUCKET, "fingerprints/");
    const fingerprintKeys = keys.filter((k) => k.includes(contentId));
    expect(fingerprintKeys.length).toBeGreaterThanOrEqual(1);
  });

  it("writes a metadata.json alongside master.m3u8", async () => {
    const mappingObj = (await getJson(DEST_BUCKET, mappingKey(SOURCE_KEY))) as Record<
      string,
      unknown
    >;
    const contentId = mappingObj["contentId"] as string;
    const metaKey = `by-id/${contentId}/metadata.json`;
    const exists = await objectExists(DEST_BUCKET, metaKey);
    expect(exists).toBe(true);

    const meta = (await getJson(DEST_BUCKET, metaKey)) as Record<string, unknown>;
    expect(meta["contentId"]).toBe(contentId);
    expect(typeof meta["encoderVersion"]).toBe("string");
    expect(typeof (meta["source"] as Record<string, unknown>)["width"]).toBe("number");
  });
});

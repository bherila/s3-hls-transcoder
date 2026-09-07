/**
 * End-to-end integration test for runOnce(): one source video through the whole
 * pipeline, asserting the destination-bucket layout the client contract in
 * SPEC.md promises.
 *
 * Requires:
 *   - INTEGRATION=1
 *   - Docker daemon running (for testcontainers)
 *   - ffmpeg on PATH (or FFMPEG_PATH set)
 *
 * Run with:
 *   INTEGRATION=1 pnpm --filter @s3-hls-transcoder/integration test
 */

import { afterAll, beforeAll, describe, expect, it } from "vitest";

import type { SourceMapping } from "@s3-hls-transcoder/lib";
import { GLOBAL_LOCK_KEY, mappingKey, masterPlaylistKey, runOnce } from "@s3-hls-transcoder/lib";

import {
  DEST_BUCKET,
  SOURCE_BUCKET,
  collectingLogger,
  getJson,
  getText,
  listObjects,
  makeConfig,
  objectExists,
  startMinio,
  uploadFixture,
  type Harness,
} from "./harness.js";

const INTEGRATION = process.env["INTEGRATION"] === "1";

const SOURCE_KEY = "videos/test.mp4";
const SMALL_KEY = "videos/small.mp4";

describe.skipIf(!INTEGRATION)("runOnce() integration", () => {
  let harness: Harness;
  let contentId: string;

  beforeAll(async () => {
    harness = await startMinio();
    await uploadFixture(harness.s3, SOURCE_BUCKET, SOURCE_KEY, "SOURCE");
    await uploadFixture(harness.s3, SOURCE_BUCKET, SMALL_KEY, "SMALL");

    const { logger } = collectingLogger();
    const summary = await runOnce({ config: makeConfig(harness.endpoint), logger });

    expect(summary.processed).toBe(2);
    expect(summary.cached).toBe(0);
    expect(summary.deduped).toBe(0);
    expect(summary.failed).toBe(0);
    expect(summary.pairsProcessed).toBe(1);

    contentId = (await getJson<SourceMapping>(harness.s3, DEST_BUCKET, mappingKey(SOURCE_KEY)))
      .contentId;
  }, 300_000);

  afterAll(async () => {
    await harness?.stop();
  });

  it("writes a master playlist under by-id/<contentId>/", async () => {
    const master = await getText(harness.s3, DEST_BUCKET, masterPlaylistKey(contentId));
    expect(master).toContain("#EXTM3U");
    expect(master).toContain("#EXT-X-STREAM-INF");
  });

  it("writes a mapping naming the content ID and its playable root", async () => {
    const mapping = await getJson<SourceMapping>(harness.s3, DEST_BUCKET, mappingKey(SOURCE_KEY));
    expect(mapping.sourceKey).toBe(SOURCE_KEY);
    expect(mapping.contentId).toMatch(/^sha256:[0-9a-f]{64}$/);
    expect(mapping.hlsRoot).toBe(masterPlaylistKey(mapping.contentId));
    expect(mapping.sourceBucket).toBe(SOURCE_BUCKET);
    expect(await objectExists(harness.s3, DEST_BUCKET, mapping.hlsRoot)).toBe(true);
  });

  it("releases the global lock after completion", async () => {
    expect(await objectExists(harness.s3, DEST_BUCKET, GLOBAL_LOCK_KEY)).toBe(false);
  });

  it("writes per-variant playlists and segments for every rung it kept", async () => {
    const keys = await listObjects(harness.s3, DEST_BUCKET, `by-id/${contentId}/`);
    // The 640×360 source keeps both configured rungs; neither is an upscale.
    expect(keys).toContain(`by-id/${contentId}/240p/index.m3u8`);
    expect(keys).toContain(`by-id/${contentId}/360p/index.m3u8`);
    expect(keys.filter((k) => k.endsWith(".m4s") || k.endsWith(".ts")).length).toBeGreaterThan(0);
  });

  it("skips ladder rungs above the source resolution", async () => {
    const smallId = (await getJson<SourceMapping>(harness.s3, DEST_BUCKET, mappingKey(SMALL_KEY)))
      .contentId;
    const keys = await listObjects(harness.s3, DEST_BUCKET, `by-id/${smallId}/`);
    // No rung fits a 320×240 source, so it falls back to the smallest one only.
    expect(keys).toContain(`by-id/${smallId}/240p/index.m3u8`);
    expect(keys.some((k) => k.startsWith(`by-id/${smallId}/360p/`))).toBe(false);
  });

  it("writes a fingerprint and indexes it", async () => {
    expect(await objectExists(harness.s3, DEST_BUCKET, `fingerprints/${contentId}.bin`)).toBe(true);

    const index = await getJson<{ version: number; entries: { contentId: string }[] }>(
      harness.s3,
      DEST_BUCKET,
      "fingerprints/index.json",
    );
    expect(index.version).toBe(1);
    expect(index.entries.map((e) => e.contentId)).toContain(contentId);
  });

  it("writes metadata.json alongside the master playlist", async () => {
    const meta = await getJson<{
      contentId: string;
      encoderVersion: string;
      source: { width: number; height: number };
      ladder: { name: string }[];
    }>(harness.s3, DEST_BUCKET, `by-id/${contentId}/metadata.json`);

    expect(meta.contentId).toBe(contentId);
    expect(typeof meta.encoderVersion).toBe("string");
    expect(meta.source.width).toBe(640);
    expect(meta.source.height).toBe(360);
    expect(meta.ladder.map((r) => r.name)).toEqual(["240p", "360p"]);
  });

  it("treats an unchanged source as cached on the next run", async () => {
    const { logger } = collectingLogger();
    const summary = await runOnce({ config: makeConfig(harness.endpoint), logger });

    expect(summary.cached).toBe(2);
    expect(summary.processed).toBe(0);
    expect(summary.deduped).toBe(0);
    expect(summary.failed).toBe(0);
  }, 120_000);
});

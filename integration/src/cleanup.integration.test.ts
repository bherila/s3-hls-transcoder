/**
 * Refcount-aware cleanup of deleted sources against real bucket I/O
 * (SPEC.md §6). The tests in this file run in order: each step deletes another
 * source object and re-runs the pipeline, so the destination bucket walks
 * through retention, GC, and the dry-run escape hatch.
 *
 * Requires INTEGRATION=1, Docker, and ffmpeg. See ./harness.ts.
 */

import { afterAll, beforeAll, describe, expect, it } from "vitest";

import type { Config, SourceMapping } from "@s3-hls-transcoder/lib";
import { mappingKey, masterPlaylistKey, runOnce } from "@s3-hls-transcoder/lib";

import {
  DEST_BUCKET,
  SOURCE_BUCKET,
  collectingLogger,
  deleteObject,
  getJson,
  listObjects,
  makeConfig,
  objectExists,
  startMinio,
  uploadFixture,
  type Harness,
} from "./harness.js";

const INTEGRATION = process.env["INTEGRATION"] === "1";

// Two source keys with identical bytes (one content ID, two mappings) plus an
// unrelated video, so retention and GC are both observable.
const FIRST_KEY = "videos/first.mp4";
const SECOND_KEY = "videos/second-copy.mp4";
const OTHER_KEY = "videos/other.mp4";

describe.skipIf(!INTEGRATION)("cleanup pass integration", () => {
  let harness: Harness;
  let sharedContentId: string;
  let otherContentId: string;

  function config(overrides: Partial<Config> = {}): Config {
    return makeConfig(harness.endpoint, { cleanupDeletedSources: true, ...overrides });
  }

  async function run(overrides: Partial<Config> = {}) {
    const { logger } = collectingLogger();
    return runOnce({ config: config(overrides), logger });
  }

  beforeAll(async () => {
    harness = await startMinio();
    await uploadFixture(harness.s3, SOURCE_BUCKET, FIRST_KEY, "SOURCE");
    await uploadFixture(harness.s3, SOURCE_BUCKET, SECOND_KEY, "SOURCE");
    await uploadFixture(harness.s3, SOURCE_BUCKET, OTHER_KEY, "SMALL");

    const summary = await run({ cleanupDeletedSources: false });
    expect(summary.failed).toBe(0);

    sharedContentId = (await getJson<SourceMapping>(harness.s3, DEST_BUCKET, mappingKey(FIRST_KEY)))
      .contentId;
    otherContentId = (await getJson<SourceMapping>(harness.s3, DEST_BUCKET, mappingKey(OTHER_KEY)))
      .contentId;

    // The duplicate must share the first file's content ID for the refcount
    // assertions below to mean anything.
    const second = await getJson<SourceMapping>(harness.s3, DEST_BUCKET, mappingKey(SECOND_KEY));
    expect(second.contentId).toBe(sharedContentId);
    expect(otherContentId).not.toBe(sharedContentId);
  }, 300_000);

  afterAll(async () => {
    await harness?.stop();
  });

  it("leaves everything alone while every source is still present", async () => {
    const summary = await run();

    expect(summary.cached).toBe(3);
    expect(await listObjects(harness.s3, DEST_BUCKET, "mappings/")).toHaveLength(3);
    expect(await objectExists(harness.s3, DEST_BUCKET, masterPlaylistKey(sharedContentId))).toBe(
      true,
    );
  }, 120_000);

  it("reports but does not perform deletions in dry-run mode", async () => {
    await deleteObject(harness.s3, SOURCE_BUCKET, SECOND_KEY);

    await run({ cleanupDryRun: true });

    expect(await objectExists(harness.s3, DEST_BUCKET, mappingKey(SECOND_KEY))).toBe(true);
    expect(await objectExists(harness.s3, DEST_BUCKET, masterPlaylistKey(sharedContentId))).toBe(
      true,
    );
  }, 120_000);

  it("deletes the orphan mapping but retains content another source still uses", async () => {
    await run();

    expect(await objectExists(harness.s3, DEST_BUCKET, mappingKey(SECOND_KEY))).toBe(false);
    // FIRST_KEY still points at it, so the output and fingerprint stay.
    expect(await objectExists(harness.s3, DEST_BUCKET, mappingKey(FIRST_KEY))).toBe(true);
    expect(await objectExists(harness.s3, DEST_BUCKET, masterPlaylistKey(sharedContentId))).toBe(
      true,
    );
    expect(await objectExists(harness.s3, DEST_BUCKET, `fingerprints/${sharedContentId}.bin`)).toBe(
      true,
    );
  }, 120_000);

  it("garbage-collects the output once the last source referencing it is gone", async () => {
    await deleteObject(harness.s3, SOURCE_BUCKET, FIRST_KEY);

    await run();

    expect(await objectExists(harness.s3, DEST_BUCKET, mappingKey(FIRST_KEY))).toBe(false);
    expect(await listObjects(harness.s3, DEST_BUCKET, `by-id/${sharedContentId}/`)).toEqual([]);
    expect(await objectExists(harness.s3, DEST_BUCKET, `fingerprints/${sharedContentId}.bin`)).toBe(
      false,
    );

    const index = await getJson<{ entries: { contentId: string }[] }>(
      harness.s3,
      DEST_BUCKET,
      "fingerprints/index.json",
    );
    expect(index.entries.map((e) => e.contentId)).toEqual([otherContentId]);
  }, 120_000);

  it("leaves the unrelated video untouched throughout", async () => {
    expect(await objectExists(harness.s3, DEST_BUCKET, mappingKey(OTHER_KEY))).toBe(true);
    expect(await objectExists(harness.s3, DEST_BUCKET, masterPlaylistKey(otherContentId))).toBe(
      true,
    );
  });
});

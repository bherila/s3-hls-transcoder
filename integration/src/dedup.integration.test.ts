/**
 * Deduplication behavior against real bucket I/O (SPEC.md §5).
 *
 * Two layers, and they are deliberately not symmetric:
 *   - byte-hash: identical bytes reuse the existing output, no transcode.
 *   - perceptual: a match is advisory. It is logged with the quality comparison
 *     and never reuses, repoints, or deletes content, because a fingerprint
 *     does not prove the new source is authorized to affect the matched output.
 *
 * Requires INTEGRATION=1, Docker, and ffmpeg. See ./harness.ts.
 */

import { afterAll, beforeAll, describe, expect, it } from "vitest";

import type { SourceMapping } from "@s3-hls-transcoder/lib";
import { mappingKey, runOnce } from "@s3-hls-transcoder/lib";

import {
  DEST_BUCKET,
  SOURCE_BUCKET,
  collectingLogger,
  contentIdsWithOutput,
  getJson,
  listObjects,
  makeConfig,
  startMinio,
  uploadFixture,
  type LogEntry,
  type Harness,
} from "./harness.js";

const INTEGRATION = process.env["INTEGRATION"] === "1";

const ORIGINAL_KEY = "videos/original.mp4";
const IDENTICAL_KEY = "archive/identical-copy.mp4";
const REMUXED_KEY = "videos/remuxed.mp4";

describe.skipIf(!INTEGRATION)("dedup integration", () => {
  let harness: Harness;
  let entries: LogEntry[];

  beforeAll(async () => {
    harness = await startMinio();

    // Same bytes at two keys, plus a third file with identical frames but
    // different bytes (a stream copy with different container metadata).
    await uploadFixture(harness.s3, SOURCE_BUCKET, ORIGINAL_KEY, "SOURCE");
    await uploadFixture(harness.s3, SOURCE_BUCKET, IDENTICAL_KEY, "SOURCE");
    await uploadFixture(harness.s3, SOURCE_BUCKET, REMUXED_KEY, "REMUXED");

    const collected = collectingLogger();
    entries = collected.entries;
    // A threshold below 1.0 is not needed — the remux decodes to the same
    // frames — but leaving it at the default keeps the test honest about what
    // the shipped configuration does.
    const summary = await runOnce({
      config: makeConfig(harness.endpoint),
      logger: collected.logger,
    });

    // Two distinct byte-streams transcode; the duplicate is deduped.
    expect(summary.processed).toBe(2);
    expect(summary.deduped).toBe(1);
    expect(summary.failed).toBe(0);
  }, 300_000);

  afterAll(async () => {
    await harness?.stop();
  });

  it("points identical bytes at one content ID without transcoding twice", async () => {
    const original = await getJson<SourceMapping>(
      harness.s3,
      DEST_BUCKET,
      mappingKey(ORIGINAL_KEY),
    );
    const copy = await getJson<SourceMapping>(harness.s3, DEST_BUCKET, mappingKey(IDENTICAL_KEY));

    expect(copy.contentId).toBe(original.contentId);
    expect(copy.hlsRoot).toBe(original.hlsRoot);
    expect(copy.sourceKey).toBe(IDENTICAL_KEY);
  });

  it("gives every distinct byte-stream its own output tree", async () => {
    const remuxed = await getJson<SourceMapping>(harness.s3, DEST_BUCKET, mappingKey(REMUXED_KEY));
    const original = await getJson<SourceMapping>(
      harness.s3,
      DEST_BUCKET,
      mappingKey(ORIGINAL_KEY),
    );

    // Identical frames, different bytes: a perceptual match, not a byte match.
    expect(remuxed.contentId).not.toBe(original.contentId);
    expect(await contentIdsWithOutput(harness.s3, DEST_BUCKET)).toEqual(
      [original.contentId, remuxed.contentId].sort(),
    );
  });

  it("logs the perceptual match without acting on it", async () => {
    const matches = entries.filter((e) => e.msg === "perceptual match");
    // The remux decodes to the baseline's frames, so the match is not in doubt.
    expect(matches.length).toBeGreaterThanOrEqual(1);
    for (const match of matches) {
      expect(match.fields["actedUpon"]).toBe(false);
      expect(match.fields["similarity"]).toBeGreaterThanOrEqual(0.95);
    }
  });

  it("keeps a fingerprint index entry per content ID, not per source key", async () => {
    const index = await getJson<{ entries: { contentId: string }[] }>(
      harness.s3,
      DEST_BUCKET,
      "fingerprints/index.json",
    );
    const contentIds = await contentIdsWithOutput(harness.s3, DEST_BUCKET);

    expect(index.entries.map((e) => e.contentId).sort()).toEqual(contentIds);
    expect(await listObjects(harness.s3, DEST_BUCKET, "mappings/")).toHaveLength(3);
  });

  it("does not re-transcode any of them on a second run", async () => {
    const { logger } = collectingLogger();
    const summary = await runOnce({ config: makeConfig(harness.endpoint), logger });

    expect(summary.cached).toBe(3);
    expect(summary.processed).toBe(0);
    expect(summary.deduped).toBe(0);
  }, 120_000);
});

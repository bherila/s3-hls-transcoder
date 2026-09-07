/**
 * Vitest globalSetup — runs once before the worker pool is created.
 *
 * Responsibilities:
 *  1. Gate the entire suite on INTEGRATION=1.
 *  2. Generate the fixture MP4s with ffmpeg, once, so individual tests don't
 *     spend time on it. Paths reach the tests through FIXTURE_* env vars.
 *
 * The three fixtures exist to make the dedup layers observable:
 *
 *  | Fixture           | Env var          | Purpose                                                    |
 *  | ----------------- | ---------------- | ---------------------------------------------------------- |
 *  | source.mp4        | FIXTURE_SOURCE   | 640×360 baseline; keeps both ladder rungs                  |
 *  | source-remuxed.mp4| FIXTURE_REMUXED  | same frames, different bytes (stream copy + new metadata)   |
 *  | small.mp4         | FIXTURE_SMALL    | 320×240, visually unrelated; exercises rung filtering       |
 *
 * The remux is a stream copy, so its decoded frames — and therefore its
 * perceptual fingerprint — are identical to the baseline's, while its SHA-256
 * differs. That makes "byte-hash dedup misses, perceptual match hits" a
 * deterministic scenario rather than one that depends on encoder noise.
 */

import { execFile } from "node:child_process";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { promisify } from "node:util";

const execFileAsync = promisify(execFile);

// Module-level reference so teardown can remove the temp dir.
let fixtureTmpDir: string | undefined;

export async function setup(): Promise<void> {
  if (process.env["INTEGRATION"] !== "1") {
    // Skip loudly rather than silently so CI misconfiguration is obvious.
    console.log(
      "\n[integration] INTEGRATION env var is not set to 1 — skipping all integration tests.\n" +
        "  Run with: INTEGRATION=1 pnpm --filter @s3-hls-transcoder/integration test\n",
    );
    return;
  }

  // Locate ffmpeg (respect FFMPEG_PATH override, matching lib/src/ffmpeg/binary.ts).
  const ffmpeg = process.env["FFMPEG_PATH"] ?? "ffmpeg";

  fixtureTmpDir = await mkdtemp(path.join(tmpdir(), "hls-integration-fixture-"));
  const sourcePath = path.join(fixtureTmpDir, "source.mp4");
  const remuxedPath = path.join(fixtureTmpDir, "source-remuxed.mp4");
  const smallPath = path.join(fixtureTmpDir, "small.mp4");

  // Baseline: 4 seconds of a moving test pattern (motion matters — a solid
  // colour would give every frame the same perceptual hash) plus silent stereo
  // audio, so the ladder's audio path is exercised too.
  await generate(ffmpeg, sourcePath, "testsrc2=s=640x360:r=25");

  // Same video and audio bitstreams, different container metadata: identical
  // frames, different bytes.
  await execFileAsync(ffmpeg, [
    "-y",
    "-i",
    sourcePath,
    "-c",
    "copy",
    "-metadata",
    "title=remuxed copy",
    "-metadata",
    "comment=byte-identical frames, different file bytes",
    remuxedPath,
  ]);

  // Visually unrelated, and small enough that no configured rung fits it.
  await generate(ffmpeg, smallPath, "smptebars=s=320x240:r=25");

  process.env["FIXTURE_SOURCE"] = sourcePath;
  process.env["FIXTURE_REMUXED"] = remuxedPath;
  process.env["FIXTURE_SMALL"] = smallPath;
  console.log(`[integration] generated fixtures in ${fixtureTmpDir}`);
}

async function generate(ffmpeg: string, outPath: string, videoSource: string): Promise<void> {
  await execFileAsync(ffmpeg, [
    "-y",
    "-f",
    "lavfi",
    "-i",
    videoSource,
    "-f",
    "lavfi",
    "-i",
    "anullsrc=r=44100:cl=stereo",
    "-t",
    "4",
    "-c:v",
    "libx264",
    "-profile:v",
    "main",
    "-preset",
    "ultrafast",
    "-pix_fmt",
    "yuv420p",
    "-c:a",
    "aac",
    "-shortest",
    outPath,
  ]);
}

export async function teardown(): Promise<void> {
  if (fixtureTmpDir) {
    await rm(fixtureTmpDir, { recursive: true, force: true });
  }
}

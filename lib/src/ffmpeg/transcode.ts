import { mkdir } from "node:fs/promises";
import path from "node:path";
import type { LadderRung } from "../config.js";
import { runProcess } from "../process.js";
import { findFfmpeg } from "./binary.js";

export interface TranscodeOptions {
  input: string;
  outputDir: string;
  ladder: LadderRung[];
  hasAudio: boolean;
  /** HLS target segment duration in seconds. Default: 6. */
  segmentSeconds?: number;
  /** GOP size = segmentSeconds × fps. Default: 48 (assumes ~24fps × 2s; fine for 30fps). */
  gopSize?: number;
}

/**
 * Runs ffmpeg to produce an HLS ABR ladder with fMP4/CMAF segments.
 *
 * Output layout under `outputDir`:
 *   master.m3u8                  ← multi-variant master playlist
 *   <rungName>/index.m3u8        ← per-variant playlist
 *   <rungName>/init.mp4          ← fMP4 init segment
 *   <rungName>/seg_NNNNN.m4s     ← media segments
 */
export async function transcodeToHls(opts: TranscodeOptions): Promise<void> {
  if (opts.ladder.length === 0) {
    throw new Error("Ladder is empty");
  }

  await mkdir(opts.outputDir, { recursive: true });
  for (const rung of opts.ladder) {
    await mkdir(path.join(opts.outputDir, rung.name), { recursive: true });
  }

  const args = buildHlsArgs(opts);
  await runProcess(findFfmpeg(), args);
}

function buildHlsArgs(opts: TranscodeOptions): string[] {
  const { input, outputDir, ladder, hasAudio } = opts;
  const segmentSeconds = opts.segmentSeconds ?? 6;
  const gopSize = opts.gopSize ?? 48;

  // Filter graph: split video N ways, scale each.
  const splitOutputs = ladder.map((_, i) => `[v${i}]`).join("");
  const splitClause = `[0:v]split=${ladder.length}${splitOutputs}`;
  const scaleClauses = ladder
    .map(
      (rung, i) =>
        `[v${i}]scale=w=${rung.width}:h=${rung.height}:force_original_aspect_ratio=decrease,` +
        `pad=${rung.width}:${rung.height}:(ow-iw)/2:(oh-ih)/2[s${i}]`,
    )
    .join(";");
  const filterComplex = `${splitClause};${scaleClauses}`;

  const args = ["-y", "-i", input, "-filter_complex", filterComplex];

  // Per-rung video encoding.
  ladder.forEach((rung, i) => {
    args.push(
      "-map", `[s${i}]`,
      `-c:v:${i}`, "libx264",
      `-b:v:${i}`, `${rung.videoBitrateKbps}k`,
      `-maxrate:v:${i}`, `${Math.round(rung.videoBitrateKbps * 1.07)}k`,
      `-bufsize:v:${i}`, `${rung.videoBitrateKbps * 2}k`,
      `-profile:v:${i}`, "main",
      `-preset:v:${i}`, "fast",
      `-g:v:${i}`, String(gopSize),
      `-keyint_min:v:${i}`, String(gopSize),
      `-sc_threshold:v:${i}`, "0",
    );
  });

  // Per-rung audio encoding (only if source has audio).
  if (hasAudio) {
    ladder.forEach((rung, i) => {
      args.push(
        "-map", "0:a:0",
        `-c:a:${i}`, "aac",
        `-b:a:${i}`, `${rung.audioBitrateKbps}k`,
        `-ac:a:${i}`, "2",
      );
    });
  }

  // var_stream_map names each variant; %v in filenames resolves to these.
  const varStreamMap = ladder
    .map((rung, i) => (hasAudio ? `v:${i},a:${i},name:${rung.name}` : `v:${i},name:${rung.name}`))
    .join(" ");

  args.push(
    "-f", "hls",
    "-hls_time", String(segmentSeconds),
    "-hls_playlist_type", "vod",
    "-hls_segment_type", "fmp4",
    "-hls_flags", "independent_segments",
    "-hls_segment_filename", path.join(outputDir, "%v", "seg_%05d.m4s"),
    "-master_pl_name", "master.m3u8",
    "-var_stream_map", varStreamMap,
    path.join(outputDir, "%v", "index.m3u8"),
  );

  return args;
}

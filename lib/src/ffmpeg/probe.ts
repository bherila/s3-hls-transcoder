import { runProcess } from "../process.js";
import { findFfprobe } from "./binary.js";

export interface ProbeResult {
  /** Display size: the coded size with the stream's rotation applied. */
  width: number;
  height: number;
  rotation: number;
  durationSeconds: number;
  bitrateKbps?: number;
  videoCodec?: string;
  audioCodec?: string;
  hasAudio: boolean;
}

interface FfprobeOutput {
  streams: Array<{
    codec_type: string;
    codec_name?: string;
    width?: number;
    height?: number;
    /** Pixel shape, e.g. "32:27" (anamorphic); "1:1"/"0:1"/absent = square. */
    sample_aspect_ratio?: string;
    side_data_list?: Array<{ side_data_type?: string; rotation?: number }>;
    tags?: { rotate?: string };
  }>;
  format: {
    duration?: string;
    bit_rate?: string;
  };
}

export async function probeSource(input: string, timeoutMs?: number): Promise<ProbeResult> {
  const { stdout } = await runProcess(
    findFfprobe(),
    ["-v", "error", "-print_format", "json", "-show_streams", "-show_format", input],
    { timeoutMs },
  );

  return parseProbeOutput(stdout, input);
}

/** Turn ffprobe's JSON into a ProbeResult. */
export function parseProbeOutput(stdout: string, input: string): ProbeResult {
  const data = JSON.parse(stdout) as FfprobeOutput;

  const video = data.streams.find((s) => s.codec_type === "video");
  if (!video || video.width === undefined || video.height === undefined) {
    throw new Error(`No video stream found in ${input}`);
  }

  const audio = data.streams.find((s) => s.codec_type === "audio");

  const durationSeconds = data.format.duration ? Number(data.format.duration) : 0;
  const bitrateKbps = data.format.bit_rate
    ? Math.round(Number(data.format.bit_rate) / 1000)
    : undefined;

  const rotation = streamRotation(video);
  // Non-square pixels: display width = coded width × SAR (FFmpeg: DAR = iw/ih × sar).
  const sar = /^(\d+):(\d+)$/.exec(video.sample_aspect_ratio ?? "");
  const sarNum = sar ? Number(sar[1]) : 0;
  const sarDen = sar ? Number(sar[2]) : 0;
  const codedWidth =
    sarNum > 0 && sarDen > 0 && sarNum !== sarDen
      ? Math.round((video.width * sarNum) / sarDen)
      : video.width;
  const [width, height] =
    rotation % 180 === 0 ? [codedWidth, video.height] : [video.height, codedWidth];

  const result: ProbeResult = {
    width,
    height,
    rotation,
    durationSeconds,
    hasAudio: !!audio,
  };
  if (bitrateKbps !== undefined) result.bitrateKbps = bitrateKbps;
  if (video.codec_name) result.videoCodec = video.codec_name;
  if (audio?.codec_name) result.audioCodec = audio.codec_name;
  return result;
}

/**
 * Display rotation in degrees, normalized to 0/90/180/270. Newer ffprobe reports
 * it as Display Matrix side data, older versions as a "rotate" tag.
 */
function streamRotation(stream: FfprobeOutput["streams"][number]): number {
  const matrix = stream.side_data_list?.find((d) => d.side_data_type === "Display Matrix");
  const degrees = matrix?.rotation ?? (stream.tags?.rotate ? Number(stream.tags.rotate) : 0);
  const r = (((Math.round((Number.isFinite(degrees) ? degrees : 0) / 90) * 90) % 360) + 360) % 360;
  return r;
}

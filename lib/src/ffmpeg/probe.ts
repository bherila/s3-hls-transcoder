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
  const [width, height] =
    rotation % 180 === 0 ? [video.width, video.height] : [video.height, video.width];

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

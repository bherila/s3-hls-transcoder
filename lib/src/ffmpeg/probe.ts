import { runProcess } from "../process.js";
import { findFfprobe } from "./binary.js";

export interface ProbeResult {
  width: number;
  height: number;
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
  }>;
  format: {
    duration?: string;
    bit_rate?: string;
  };
}

export async function probeSource(input: string): Promise<ProbeResult> {
  const { stdout } = await runProcess(findFfprobe(), [
    "-v", "error",
    "-print_format", "json",
    "-show_streams",
    "-show_format",
    input,
  ]);

  const data = JSON.parse(stdout) as FfprobeOutput;

  const video = data.streams.find((s) => s.codec_type === "video");
  if (!video || video.width === undefined || video.height === undefined) {
    throw new Error(`No video stream found in ${input}`);
  }

  const audio = data.streams.find((s) => s.codec_type === "audio");

  const durationSeconds = data.format.duration ? Number(data.format.duration) : 0;
  const bitrateKbps = data.format.bit_rate ? Math.round(Number(data.format.bit_rate) / 1000) : undefined;

  const result: ProbeResult = {
    width: video.width,
    height: video.height,
    durationSeconds,
    hasAudio: !!audio,
  };
  if (bitrateKbps !== undefined) result.bitrateKbps = bitrateKbps;
  if (video.codec_name) result.videoCodec = video.codec_name;
  if (audio?.codec_name) result.audioCodec = audio.codec_name;
  return result;
}

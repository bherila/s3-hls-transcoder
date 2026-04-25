import { existsSync } from "node:fs";

let cachedFfmpeg: string | null = null;
let cachedFfprobe: string | null = null;

const FFMPEG_CANDIDATES = [
  "/usr/local/bin/ffmpeg",
  "/usr/bin/ffmpeg",
  "/opt/homebrew/bin/ffmpeg",
];

const FFPROBE_CANDIDATES = [
  "/usr/local/bin/ffprobe",
  "/usr/bin/ffprobe",
  "/opt/homebrew/bin/ffprobe",
];

function find(envVar: string, candidates: string[], nameOnPath: string): string {
  const fromEnv = process.env[envVar];
  if (fromEnv) {
    if (existsSync(fromEnv)) return fromEnv;
    throw new Error(`${envVar} is set but does not point at an existing file: ${fromEnv}`);
  }
  for (const candidate of candidates) {
    if (existsSync(candidate)) return candidate;
  }
  // Fall back to plain command name; spawn will use PATH and fail loudly if missing.
  return nameOnPath;
}

export function findFfmpeg(): string {
  if (cachedFfmpeg) return cachedFfmpeg;
  cachedFfmpeg = find("FFMPEG_PATH", FFMPEG_CANDIDATES, "ffmpeg");
  return cachedFfmpeg;
}

export function findFfprobe(): string {
  if (cachedFfprobe) return cachedFfprobe;
  cachedFfprobe = find("FFPROBE_PATH", FFPROBE_CANDIDATES, "ffprobe");
  return cachedFfprobe;
}

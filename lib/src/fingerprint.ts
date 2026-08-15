import { spawn } from "node:child_process";
import { findFfmpeg } from "./ffmpeg/binary.js";

const FRAME_W = 9;
const FRAME_H = 8;
const FRAME_BYTES = FRAME_W * FRAME_H;
const HASH_BITS = 64;

/**
 * A perceptual fingerprint of a video: a sequence of dHashes computed from
 * keyframes sampled at a fixed cadence. Robust to scaling and re-encoding,
 * which is what we need to detect "same content, different quality".
 *
 * Encoding/comparison is deliberately simple — dHash on 9×8 grayscale frames
 * gives a 64-bit hash per frame that's stable across reasonable transcodes.
 * Compare two fingerprints by averaging Hamming distance across aligned
 * frames (1 - avgDistance/64 → similarity in [0, 1]).
 */
export interface VideoFingerprint {
  hashes: bigint[];
  intervalSeconds: number;
}

export interface FingerprintOptions {
  intervalSeconds?: number;
  maxFrames?: number;
  timeoutMs?: number;
}

export async function fingerprintVideo(
  input: string,
  options: FingerprintOptions | number = {},
): Promise<VideoFingerprint> {
  const intervalSeconds = typeof options === "number" ? options : (options.intervalSeconds ?? 2);
  const maxFrames = typeof options === "number" ? undefined : options.maxFrames;
  const timeoutMs = typeof options === "number" ? undefined : options.timeoutMs;
  const ffmpeg = findFfmpeg();
  const args = [
    "-i",
    input,
    "-vf",
    `fps=1/${intervalSeconds},scale=${FRAME_W}:${FRAME_H}:flags=lanczos,format=gray`,
    "-f",
    "rawvideo",
    "-pix_fmt",
    "gray",
    "-",
  ];

  return new Promise((resolve, reject) => {
    const proc = spawn(ffmpeg, args, { stdio: ["ignore", "pipe", "pipe"] });
    const hashes: bigint[] = [];
    let pending: Buffer = Buffer.alloc(0);
    let stderr = "";
    let settled = false;
    let timedOut = false;
    let frameLimitExceeded = false;

    const finish = (err?: Error) => {
      if (settled) return;
      settled = true;
      if (timer) clearTimeout(timer);
      if (err) reject(err);
      else resolve({ hashes, intervalSeconds });
    };

    const killWith = (reason: "timeout" | "frame-limit") => {
      if (reason === "timeout") timedOut = true;
      else frameLimitExceeded = true;
      proc.kill("SIGKILL");
    };

    const timer = timeoutMs
      ? setTimeout(() => {
          killWith("timeout");
        }, timeoutMs)
      : undefined;

    proc.stdout.on("data", (d: Buffer) => {
      pending = pending.length === 0 ? d : Buffer.concat([pending, d]);
      while (pending.length >= FRAME_BYTES) {
        const frame = pending.subarray(0, FRAME_BYTES);
        hashes.push(dhash(frame));
        pending = pending.subarray(FRAME_BYTES);
        if (maxFrames !== undefined && hashes.length > maxFrames) {
          killWith("frame-limit");
          return;
        }
      }
    });
    proc.stderr.on("data", (d) => {
      stderr += d.toString();
      if (stderr.length > 4000) stderr = stderr.slice(-4000);
    });
    proc.on("error", finish);
    proc.on("close", (code) => {
      if (timedOut) {
        finish(new Error(`ffmpeg fingerprint timed out after ${timeoutMs}ms`));
        return;
      }
      if (frameLimitExceeded) {
        finish(new Error(`ffmpeg fingerprint exceeded frame limit (${maxFrames})`));
        return;
      }
      if (code !== 0) {
        const tail = stderr.length > 1000 ? `…${stderr.slice(-1000)}` : stderr;
        finish(new Error(`ffmpeg fingerprint exited ${code}\nstderr: ${tail}`));
        return;
      }
      finish();
    });
  });
}

function dhash(frame: Buffer): bigint {
  let hash = 0n;
  let bit = 0;
  for (let y = 0; y < FRAME_H; y++) {
    for (let x = 0; x < FRAME_W - 1; x++) {
      const left = frame[y * FRAME_W + x]!;
      const right = frame[y * FRAME_W + x + 1]!;
      if (left > right) hash |= 1n << BigInt(bit);
      bit++;
    }
  }
  return hash;
}

export function popcount64(n: bigint): number {
  let count = 0;
  let x = n & 0xffffffffffffffffn;
  while (x > 0n) {
    if (x & 1n) count++;
    x >>= 1n;
  }
  return count;
}

/**
 * Similarity score in [0, 1]. 1 = bit-identical hashes; 0 = maximally distant.
 * Aligned by frame index. Mismatched lengths use the shorter sequence.
 */
export function fingerprintSimilarity(a: VideoFingerprint, b: VideoFingerprint): number {
  const minFrames = Math.min(a.hashes.length, b.hashes.length);
  if (minFrames === 0) return 0;
  let totalDistance = 0;
  for (let i = 0; i < minFrames; i++) {
    totalDistance += popcount64(a.hashes[i]! ^ b.hashes[i]!);
  }
  const avgDistance = totalDistance / minFrames;
  return Math.max(0, Math.min(1, 1 - avgDistance / HASH_BITS));
}

const HEADER_BYTES = 8;

export function serializeFingerprint(fp: VideoFingerprint): Buffer {
  const buf = Buffer.alloc(HEADER_BYTES + fp.hashes.length * 8);
  buf.writeFloatLE(fp.intervalSeconds, 0);
  buf.writeUInt32LE(fp.hashes.length, 4);
  for (let i = 0; i < fp.hashes.length; i++) {
    buf.writeBigUInt64LE(fp.hashes[i]!, HEADER_BYTES + i * 8);
  }
  return buf;
}

export function deserializeFingerprint(buf: Buffer): VideoFingerprint {
  const intervalSeconds = buf.readFloatLE(0);
  const numHashes = buf.readUInt32LE(4);
  const hashes: bigint[] = [];
  for (let i = 0; i < numHashes; i++) {
    hashes.push(buf.readBigUInt64LE(HEADER_BYTES + i * 8));
  }
  return { hashes, intervalSeconds };
}

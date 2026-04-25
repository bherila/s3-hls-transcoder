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

export async function fingerprintVideo(
  input: string,
  intervalSeconds: number = 2,
): Promise<VideoFingerprint> {
  const ffmpeg = findFfmpeg();
  const args = [
    "-i", input,
    "-vf", `fps=1/${intervalSeconds},scale=${FRAME_W}:${FRAME_H}:flags=lanczos,format=gray`,
    "-f", "rawvideo",
    "-pix_fmt", "gray",
    "-",
  ];

  return new Promise((resolve, reject) => {
    const proc = spawn(ffmpeg, args, { stdio: ["ignore", "pipe", "pipe"] });
    const chunks: Buffer[] = [];
    let stderr = "";

    proc.stdout.on("data", (d: Buffer) => {
      chunks.push(d);
    });
    proc.stderr.on("data", (d) => {
      stderr += d.toString();
    });
    proc.on("error", reject);
    proc.on("close", (code) => {
      if (code !== 0) {
        const tail = stderr.length > 1000 ? `…${stderr.slice(-1000)}` : stderr;
        reject(new Error(`ffmpeg fingerprint exited ${code}\nstderr: ${tail}`));
        return;
      }
      const all = Buffer.concat(chunks);
      const numFrames = Math.floor(all.length / FRAME_BYTES);
      const hashes: bigint[] = [];
      for (let i = 0; i < numFrames; i++) {
        const frame = all.subarray(i * FRAME_BYTES, (i + 1) * FRAME_BYTES);
        hashes.push(dhash(frame));
      }
      resolve({ hashes, intervalSeconds });
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

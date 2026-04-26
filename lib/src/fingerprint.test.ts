import { describe, expect, it } from "vitest";
import {
  deserializeFingerprint,
  fingerprintSimilarity,
  popcount64,
  serializeFingerprint,
  type VideoFingerprint,
} from "./fingerprint.js";

describe("popcount64", () => {
  it("counts bits", () => {
    expect(popcount64(0n)).toBe(0);
    expect(popcount64(1n)).toBe(1);
    expect(popcount64(0xffn)).toBe(8);
    expect(popcount64(0xffffffffffffffffn)).toBe(64);
  });
});

describe("fingerprintSimilarity", () => {
  it("returns 1 for identical fingerprints", () => {
    const fp: VideoFingerprint = {
      hashes: [0x1234567890abcdefn, 0xfedcba0987654321n],
      intervalSeconds: 2,
    };
    expect(fingerprintSimilarity(fp, fp)).toBe(1);
  });

  it("returns 0 when one fingerprint is empty", () => {
    const empty: VideoFingerprint = { hashes: [], intervalSeconds: 2 };
    const one: VideoFingerprint = { hashes: [0n], intervalSeconds: 2 };
    expect(fingerprintSimilarity(empty, one)).toBe(0);
  });

  it("returns 0 for fully-flipped 64-bit difference", () => {
    const a: VideoFingerprint = { hashes: [0n], intervalSeconds: 2 };
    const b: VideoFingerprint = { hashes: [0xffffffffffffffffn], intervalSeconds: 2 };
    expect(fingerprintSimilarity(a, b)).toBe(0);
  });

  it("aligns by index when lengths differ (uses shorter sequence)", () => {
    const a: VideoFingerprint = { hashes: [0n, 0n, 0xffn], intervalSeconds: 2 };
    const b: VideoFingerprint = { hashes: [0n, 0n], intervalSeconds: 2 };
    // First two frames match perfectly; third frame ignored.
    expect(fingerprintSimilarity(a, b)).toBe(1);
  });

  it("computes intermediate similarity correctly", () => {
    // 8 bits flipped out of 64 = 87.5% similarity
    const a: VideoFingerprint = { hashes: [0n], intervalSeconds: 2 };
    const b: VideoFingerprint = { hashes: [0xffn], intervalSeconds: 2 };
    expect(fingerprintSimilarity(a, b)).toBeCloseTo(1 - 8 / 64, 5);
  });
});

describe("fingerprint serialization", () => {
  it("round-trips", () => {
    const fp: VideoFingerprint = {
      hashes: [0x1234567890abcdefn, 0xdeadbeefcafebaben, 0n, 0xffffffffffffffffn],
      intervalSeconds: 2.5,
    };
    const decoded = deserializeFingerprint(serializeFingerprint(fp));
    expect(decoded.intervalSeconds).toBeCloseTo(2.5);
    expect(decoded.hashes).toEqual(fp.hashes);
  });

  it("round-trips empty fingerprint", () => {
    const fp: VideoFingerprint = { hashes: [], intervalSeconds: 1 };
    const decoded = deserializeFingerprint(serializeFingerprint(fp));
    expect(decoded.intervalSeconds).toBeCloseTo(1);
    expect(decoded.hashes).toEqual([]);
  });
});

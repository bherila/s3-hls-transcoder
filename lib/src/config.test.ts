import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { bucketsOverlap, loadConfig, type BucketConfig } from "./config.js";

const ORIGINAL_ENV = process.env;

beforeEach(() => {
  process.env = { ...ORIGINAL_ENV };
});

afterEach(() => {
  process.env = ORIGINAL_ENV;
});

const base: BucketConfig = {
  bucket: "videos",
  endpoint: "https://r2.example.com",
  accessKeyId: "ak",
  secretAccessKey: "sk",
  region: "auto",
};

describe("bucketsOverlap", () => {
  it("different endpoint → not overlap", () => {
    expect(bucketsOverlap(base, { ...base, endpoint: "https://other.example" })).toBe(false);
  });

  it("different bucket name → not overlap", () => {
    expect(bucketsOverlap(base, { ...base, bucket: "other" })).toBe(false);
  });

  it("same bucket, no prefixes → overlap", () => {
    expect(bucketsOverlap(base, base)).toBe(true);
  });

  it("one prefix subsumes the other → overlap (both directions)", () => {
    const a: BucketConfig = { ...base, prefix: "uploads/" };
    const b: BucketConfig = { ...base, prefix: "uploads/transcoded/" };
    expect(bucketsOverlap(a, b)).toBe(true);
    expect(bucketsOverlap(b, a)).toBe(true);
  });

  it("disjoint prefixes → not overlap", () => {
    const a: BucketConfig = { ...base, prefix: "uploads/" };
    const b: BucketConfig = { ...base, prefix: "transcoded/" };
    expect(bucketsOverlap(a, b)).toBe(false);
  });

  it("empty prefix subsumes any other prefix → overlap", () => {
    const a: BucketConfig = { ...base }; // no prefix
    const b: BucketConfig = { ...base, prefix: "anything/" };
    expect(bucketsOverlap(a, b)).toBe(true);
  });

  it("normalizes endpoint case", () => {
    const a: BucketConfig = { ...base, endpoint: "https://R2.Example.Com" };
    const b: BucketConfig = { ...base, endpoint: "https://r2.example.com" };
    expect(bucketsOverlap(a, b)).toBe(true);
  });

  it("normalizes endpoint with trailing path", () => {
    const a: BucketConfig = { ...base, endpoint: "https://r2.example.com/" };
    const b: BucketConfig = { ...base, endpoint: "https://r2.example.com" };
    expect(bucketsOverlap(a, b)).toBe(true);
  });
});

describe("loadConfig destination validation", () => {
  it("rejects multiple pairs that share the same destination bucket namespace", () => {
    process.env.BUCKETS_CONFIG = JSON.stringify([
      {
        source: { bucket: "src-a", endpoint: "https://r2.example.com" },
        dest: { bucket: "shared-dest", endpoint: "https://R2.Example.com/path" },
        accessKeyId: "ak",
        secretAccessKey: "sk",
      },
      {
        source: { bucket: "src-b", endpoint: "https://r2.example.com" },
        dest: { bucket: "shared-dest", endpoint: "https://r2.example.com" },
        accessKeyId: "ak",
        secretAccessKey: "sk",
      },
    ]);

    expect(() => loadConfig("local")).toThrow(/Destination buckets must be unique/);
  });

  it("allows multiple pairs with different destination buckets", () => {
    process.env.BUCKETS_CONFIG = JSON.stringify([
      {
        source: { bucket: "src-a", endpoint: "https://r2.example.com" },
        dest: { bucket: "dest-a", endpoint: "https://r2.example.com" },
        accessKeyId: "ak",
        secretAccessKey: "sk",
      },
      {
        source: { bucket: "src-b", endpoint: "https://r2.example.com" },
        dest: { bucket: "dest-b", endpoint: "https://r2.example.com" },
        accessKeyId: "ak",
        secretAccessKey: "sk",
      },
    ]);

    expect(loadConfig("local").pairs).toHaveLength(2);
  });
});

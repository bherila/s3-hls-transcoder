import { describe, expect, it } from "vitest";
import { isCachedMapping, mappingKey, type SourceMapping } from "./mapping.js";

describe("mappingKey", () => {
  it("preserves source path under mappings/ prefix with .json suffix", () => {
    expect(mappingKey("video.mp4")).toBe("mappings/video.mp4.json");
    expect(mappingKey("a/b/c.mov")).toBe("mappings/a/b/c.mov.json");
    expect(mappingKey("deeply/nested/path/file.mkv")).toBe("mappings/deeply/nested/path/file.mkv.json");
  });
});

describe("isCachedMapping", () => {
  const base: SourceMapping = {
    sourceKey: "x.mp4",
    sourceEtag: "etag1",
    sourceSize: 100,
    sourceLastModified: "2026-04-25T00:00:00Z",
    contentId: "sha256:abc",
    hlsRoot: "by-id/sha256:abc/master.m3u8",
    encodedAt: "2026-04-25T00:00:00Z",
    encoderVersion: "0.1.0",
  };

  it("returns false for null mapping (never processed)", () => {
    expect(isCachedMapping(null, { etag: "etag1", size: 100 })).toBe(false);
  });

  it("returns true on etag + size match", () => {
    expect(isCachedMapping(base, { etag: "etag1", size: 100 })).toBe(true);
  });

  it("returns false on etag mismatch (re-uploaded)", () => {
    expect(isCachedMapping(base, { etag: "etag2", size: 100 })).toBe(false);
  });

  it("returns false on size mismatch (likely re-uploaded)", () => {
    expect(isCachedMapping(base, { etag: "etag1", size: 101 })).toBe(false);
  });
});

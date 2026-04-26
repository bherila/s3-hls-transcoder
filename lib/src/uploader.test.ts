import { describe, expect, it } from "vitest";
import { contentTypeFor } from "./uploader.js";

describe("contentTypeFor", () => {
  it("recognizes HLS playlists", () => {
    expect(contentTypeFor("master.m3u8")).toBe("application/vnd.apple.mpegurl");
    expect(contentTypeFor("360p/index.m3u8")).toBe("application/vnd.apple.mpegurl");
  });

  it("recognizes CMAF / segment formats", () => {
    expect(contentTypeFor("seg.m4s")).toBe("video/iso.segment");
    expect(contentTypeFor("seg.ts")).toBe("video/mp2t");
    expect(contentTypeFor("init.mp4")).toBe("video/mp4");
  });

  it("recognizes captions and metadata", () => {
    expect(contentTypeFor("captions.vtt")).toBe("text/vtt");
    expect(contentTypeFor("metadata.json")).toBe("application/json");
  });

  it("falls back to octet-stream for unknown extensions", () => {
    expect(contentTypeFor("unknown.xyz")).toBe("application/octet-stream");
    expect(contentTypeFor("nofile")).toBe("application/octet-stream");
  });
});

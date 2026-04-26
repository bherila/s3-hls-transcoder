import { describe, expect, it } from "vitest";
import {
  byIdPrefix,
  formatContentId,
  masterPlaylistKey,
  metadataKey,
  parseContentId,
} from "./contentId.js";

describe("formatContentId / parseContentId", () => {
  it("round-trips sha256", () => {
    const id = formatContentId("sha256", "abc123");
    expect(id).toBe("sha256:abc123");
    expect(parseContentId(id)).toEqual({ scheme: "sha256", id: "abc123" });
  });

  it("round-trips psig", () => {
    expect(parseContentId("psig:xyz")).toEqual({ scheme: "psig", id: "xyz" });
  });

  it("rejects missing scheme prefix", () => {
    expect(() => parseContentId("abc123")).toThrow(/missing scheme/i);
  });

  it("rejects unknown scheme", () => {
    expect(() => parseContentId("md5:abc")).toThrow(/Unknown content ID scheme/);
  });
});

describe("canonical paths", () => {
  const id = "sha256:abc";
  it("byIdPrefix", () => {
    expect(byIdPrefix(id)).toBe("by-id/sha256:abc/");
  });
  it("masterPlaylistKey", () => {
    expect(masterPlaylistKey(id)).toBe("by-id/sha256:abc/master.m3u8");
  });
  it("metadataKey", () => {
    expect(metadataKey(id)).toBe("by-id/sha256:abc/metadata.json");
  });
});

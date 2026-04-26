import { describe, expect, it } from "vitest";
import { isVideoKey } from "./scanner.js";

describe("isVideoKey", () => {
  it("matches common video extensions", () => {
    expect(isVideoKey("a.mp4")).toBe(true);
    expect(isVideoKey("clip.mov")).toBe(true);
    expect(isVideoKey("clip.mkv")).toBe(true);
    expect(isVideoKey("clip.webm")).toBe(true);
    expect(isVideoKey("clip.avi")).toBe(true);
    expect(isVideoKey("clip.m4v")).toBe(true);
    expect(isVideoKey("a/b/c.mov")).toBe(true);
  });

  it("is case-insensitive on extension", () => {
    expect(isVideoKey("video.MKV")).toBe(true);
    expect(isVideoKey("Video.Mp4")).toBe(true);
  });

  it("rejects non-video files", () => {
    expect(isVideoKey("notes.txt")).toBe(false);
    expect(isVideoKey("image.png")).toBe(false);
    expect(isVideoKey("audio.mp3")).toBe(false);
    expect(isVideoKey("doc.pdf")).toBe(false);
  });

  it("rejects extensionless files", () => {
    expect(isVideoKey("video")).toBe(false);
    expect(isVideoKey("a/b/file")).toBe(false);
  });

  it("uses the basename's extension, not directory dots", () => {
    expect(isVideoKey("v1.0/raw")).toBe(false);
    expect(isVideoKey("v1.0/raw.mp4")).toBe(true);
  });
});

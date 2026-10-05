import { describe, expect, it } from "vitest";
import { DEFAULT_LADDER } from "./config.js";
import { buildHlsArgs } from "./ffmpeg/transcode.js";
import { parseProbeOutput } from "./ffmpeg/probe.js";
import { computeEffectiveLadder } from "./ladder.js";

const dims = (w: number, h: number, kbps?: number) =>
  computeEffectiveLadder(DEFAULT_LADDER, w, h, kbps).map((r) => [
    r.name,
    r.width,
    r.height,
    r.videoBitrateKbps,
  ]);

describe("computeEffectiveLadder", () => {
  it("leaves 16:9 landscape unchanged", () => {
    expect(dims(1920, 1080)).toEqual([
      ["360p", 640, 360, 800],
      ["480p", 854, 480, 1400],
      ["720p", 1280, 720, 2800],
      ["1080p", 1920, 1080, 5000],
    ]);
  });

  it("keeps portrait upright with every rung", () => {
    expect(dims(1080, 1920)).toEqual([
      ["360p", 360, 640, 800],
      ["480p", 480, 854, 1400],
      ["720p", 720, 1280, 2800],
      ["1080p", 1080, 1920, 5000],
    ]);
  });

  it("caps a low-bitrate portrait source instead of padding it into 640×360", () => {
    expect(dims(432, 768, 344)).toEqual([["360p", 360, 640, 430]]);
  });

  it("never upscales below the lowest rung", () => {
    expect(dims(320, 240)).toEqual([["360p", 320, 240, 800]]);
  });
});

describe("buildHlsArgs", () => {
  it("scales to exact sizes without padding", () => {
    const args = buildHlsArgs({
      input: "in",
      outputDir: "out",
      hasAudio: true,
      ladder: computeEffectiveLadder(DEFAULT_LADDER, 1080, 1920),
    }).join(" ");
    expect(args).not.toMatch(/pad=|force_original_aspect_ratio/);
    expect(args).toContain("[v0]scale=w=360:h=640,setsar=1[s0]");
  });
});

describe("parseProbeOutput", () => {
  it("applies a Display Matrix rotation (iPhone .mov)", () => {
    const probe = parseProbeOutput(
      JSON.stringify({
        streams: [
          {
            codec_type: "video",
            width: 1920,
            height: 1080,
            side_data_list: [{ side_data_type: "Display Matrix", rotation: -90 }],
          },
        ],
        format: {},
      }),
      "test",
    );
    expect([probe.width, probe.height, probe.rotation]).toEqual([1080, 1920, 270]);
  });

  it("applies a legacy rotate tag", () => {
    const probe = parseProbeOutput(
      JSON.stringify({
        streams: [{ codec_type: "video", width: 1280, height: 720, tags: { rotate: "90" } }],
        format: {},
      }),
      "test",
    );
    expect([probe.width, probe.height, probe.rotation]).toEqual([720, 1280, 90]);
  });
});

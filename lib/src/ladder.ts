import type { LadderRung } from "./config.js";

// Ladder rungs are named for their short edge ("360p" = 360 pixels on the short
// side) and apply to any orientation: a portrait 1080×1920 source gets a 360p
// rendition of 360×640, not a 640×360 frame with the picture shrunk inside
// black bars. A rung's width/height are its landscape reference box; only the
// short edge and the bitrates are used. Mirrors core/ladder.go.

/** Floor when capping a rung to the source bitrate. */
const MIN_VIDEO_BITRATE_KBPS = 200;

/** Re-encoding at exactly a low source bitrate loses further detail. */
const SOURCE_BITRATE_HEADROOM = 1.25;

const shortEdge = (r: LadderRung): number => Math.min(r.width, r.height);

const roundEven = (v: number): number => Math.max(2, 2 * Math.round(v / 2));

/** The w×h size scaled so its short edge is `short`, both sides even. */
export function scaleToShortEdge(w: number, h: number, short: number): [number, number] {
  return w >= h
    ? [roundEven((w * short) / h), roundEven(short)]
    : [roundEven(short), roundEven((h * short) / w)];
}

/**
 * Pick the rungs for a source of display size w×h (after rotation), returned
 * with their exact output dimensions (even, aspect preserved, no padding).
 * Rungs whose short edge exceeds the source's are skipped; when none fit, the
 * lowest rung is used at the source's own short edge rather than upscaling.
 * When the source bitrate is known each rung's video bitrate is capped near it.
 */
export function computeEffectiveLadder(
  full: readonly LadderRung[],
  w: number,
  h: number,
  sourceBitrateKbps?: number,
): LadderRung[] {
  const sourceShort = Math.min(w, h);
  let picked = full
    .filter((r) => shortEdge(r) <= sourceShort)
    .map((r) => ({ rung: r, short: shortEdge(r) }));
  if (picked.length === 0) picked = [{ rung: full[0]!, short: sourceShort }];

  return picked.map(({ rung, short }) => {
    const [width, height] = scaleToShortEdge(w, h, short);
    let videoBitrateKbps = rung.videoBitrateKbps;
    if (sourceBitrateKbps !== undefined && sourceBitrateKbps > 0) {
      const cap = Math.max(
        MIN_VIDEO_BITRATE_KBPS,
        Math.round(sourceBitrateKbps * SOURCE_BITRATE_HEADROOM),
      );
      videoBitrateKbps = Math.min(videoBitrateKbps, cap);
    }
    return { ...rung, width, height, videoBitrateKbps };
  });
}

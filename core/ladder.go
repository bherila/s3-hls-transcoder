package core

import (
	"math"
	"strconv"
	"strings"
)

// Ladder rungs are named for their short edge ("360p" = 360 pixels on the short
// side) and apply to any orientation: a portrait 1080×1920 source gets a 360p
// rendition of 360×640, not a 640×360 frame with the picture shrunk inside
// black bars. A rung's Width/Height are its landscape reference box; only the
// short edge and the bitrates are used.

// minVideoBitrateKbps is the floor when capping a rung to the source bitrate.
const minVideoBitrateKbps = 200

// sourceBitrateHeadroom lets a rung exceed the source bitrate slightly, since
// re-encoding a low-bitrate source at exactly its bitrate loses further detail.
const sourceBitrateHeadroom = 1.25

// codecEfficiency is how many H.264 bits a source codec's bit is worth, for
// comparing a source bitrate against the H.264 ladder. Codecs absent from the
// map are not capped: without a known ratio, capping could starve the output.
var codecEfficiency = map[string]float64{
	"h264": 1, "mpeg4": 1, "mpeg2video": 1, "mpeg1video": 1, "h263": 1, "mjpeg": 1,
	"vp8": 1, "theora": 1, "msmpeg4v2": 1, "msmpeg4v3": 1, "wmv1": 1, "wmv2": 1, "wmv3": 1,
	"hevc": 2, "vp9": 2,
	"av1": 2.5,
}

// rungShortEdge is the short edge a rung targets.
func rungShortEdge(r LadderRung) int {
	return min(r.Width, r.Height)
}

// computeEffectiveLadder picks the rungs for a source of display size w×h (after
// rotation) and returns them with their exact output dimensions (even, aspect
// preserved, no padding). Rungs whose short edge exceeds the source's are
// skipped; when none fit, the lowest rung is used at the source's own short
// edge rather than upscaling. When the source bitrate and codec are known, each
// rung's video bitrate is capped near the source's H.264-equivalent bitrate:
// spending more than the source carries only encodes its artifacts.
func computeEffectiveLadder(full []LadderRung, w, h int, sourceBitrateKbps *int, sourceCodec string) []LadderRung {
	sourceShort := min(w, h)

	var picked []LadderRung
	for _, r := range full {
		if rungShortEdge(r) <= sourceShort {
			picked = append(picked, r)
		}
	}
	short := map[string]int{}
	if len(picked) == 0 {
		picked = []LadderRung{full[0]}
		short[full[0].Name] = sourceShort
	}

	out := make([]LadderRung, len(picked))
	for i, r := range picked {
		s, ok := short[r.Name]
		if !ok {
			s = rungShortEdge(r)
		}
		r.Width, r.Height = scaleToShortEdge(w, h, s)
		if efficiency, known := codecEfficiency[sourceCodec]; known && sourceBitrateKbps != nil && *sourceBitrateKbps > 0 {
			capKbps := max(minVideoBitrateKbps, int(math.Round(float64(*sourceBitrateKbps)*efficiency*sourceBitrateHeadroom)))
			r.VideoBitrateKbps = min(r.VideoBitrateKbps, capKbps)
		}
		out[i] = r
	}
	return out
}

// scaleToShortEdge returns the w×h size scaled so its short edge is `short`,
// with both sides even (required by libx264 4:2:0). Sides round to the nearest
// even number but never past the source's own (even-floored) size, so rounding
// can't upscale an odd-sized source by a pixel.
func scaleToShortEdge(w, h, short int) (int, int) {
	sw := min(roundEven(float64(w)*float64(short)/float64(min(w, h))), floorEven(w))
	sh := min(roundEven(float64(h)*float64(short)/float64(min(w, h))), floorEven(h))
	return sw, sh
}

func roundEven(v float64) int {
	return max(2, 2*int(math.Round(v/2)))
}

func floorEven(v int) int {
	return max(2, v-v%2)
}

// needsReframe reports whether an encoder before 0.2.0 produced a wrong layout
// for a w×h (display) source. Those encoders fit every source into each rung's
// box, so any stored rung whose aspect differs from the source's came out
// letterboxed or pillarboxed (portrait, square, 4:3, rotated phone video).
// `stored` is the ladder recorded in the old output's metadata — not today's
// config, which may have changed since. With no record, assume the worst.
func needsReframe(w, h int, stored []LadderRung) bool {
	if w <= 0 || h <= 0 || len(stored) == 0 {
		return true
	}
	src := float64(w) / float64(h)
	for _, r := range stored {
		if r.Width <= 0 || r.Height <= 0 || math.Abs(src-float64(r.Width)/float64(r.Height)) > 0.02 {
			return true
		}
	}
	return false
}

// legacyLayoutWrong reports whether an encoder before 0.2.0 laid this source
// out wrongly: its display aspect differs from a stored rung (see
// needsReframe), or its pixels aren't square — those encoders sized from the
// coded dimensions and kept the non-square SAR, so even a 16:9 anamorphic
// source came out as a narrower picture padded into the 16:9 box.
func legacyLayoutWrong(p *ProbeResult, stored []LadderRung) bool {
	return p.Anamorphic || needsReframe(p.Width, p.Height, stored)
}

// reencodeStillApplies reports whether a re-encode decided from a mapping
// still targets the bytes just downloaded: the source may have been replaced
// between the legacy check and the download.
func reencodeStillApplies(mapped SourceMapping, downloadedContentID string) bool {
	return mapped.ContentID == downloadedContentID
}

// versionBelow reports whether dotted version v sorts before threshold. An
// empty or malformed v counts as oldest.
func versionBelow(v, threshold string) bool {
	a, okA := parseVersion(v)
	b, _ := parseVersion(threshold)
	if !okA {
		return true
	}
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".")
	if len(parts) == 0 || len(parts) > 3 || parts[0] == "" {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

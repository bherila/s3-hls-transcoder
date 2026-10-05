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

// rungShortEdge is the short edge a rung targets.
func rungShortEdge(r LadderRung) int {
	return min(r.Width, r.Height)
}

// computeEffectiveLadder picks the rungs for a source of display size w×h (after
// rotation) and returns them with their exact output dimensions (even, aspect
// preserved, no padding). Rungs whose short edge exceeds the source's are
// skipped; when none fit, the lowest rung is used at the source's own short
// edge rather than upscaling. When the source bitrate is known, each rung's
// video bitrate is capped near it: spending more than the source carries only
// encodes its artifacts.
func computeEffectiveLadder(full []LadderRung, w, h int, sourceBitrateKbps *int) []LadderRung {
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
		if sourceBitrateKbps != nil && *sourceBitrateKbps > 0 {
			capKbps := max(minVideoBitrateKbps, int(math.Round(float64(*sourceBitrateKbps)*sourceBitrateHeadroom)))
			r.VideoBitrateKbps = min(r.VideoBitrateKbps, capKbps)
		}
		out[i] = r
	}
	return out
}

// scaleToShortEdge returns the w×h size scaled so its short edge is `short`,
// with both sides rounded to even numbers (required by libx264 4:2:0).
func scaleToShortEdge(w, h, short int) (int, int) {
	if w >= h {
		return roundEven(float64(w) * float64(short) / float64(h)), roundEven(float64(short))
	}
	return roundEven(float64(short)), roundEven(float64(h) * float64(short) / float64(w))
}

func roundEven(v float64) int {
	return max(2, 2*int(math.Round(v/2)))
}

// needsReframe reports whether an encoder before 0.2.0 produced a wrong layout
// for a w×h (display) source. Those encoders fit every source into the
// ladder's boxes (refW×refH, 16:9 by default), so only a source of a different
// aspect (portrait, square, 4:3, or rotated phone video) came out letterboxed
// or pillarboxed.
func needsReframe(w, h, refW, refH int) bool {
	if w <= 0 || h <= 0 || refW <= 0 || refH <= 0 {
		return false
	}
	return math.Abs(float64(w)/float64(h)-float64(refW)/float64(refH)) > 0.02
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

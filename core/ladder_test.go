package core

import (
	"slices"
	"strings"
	"testing"
)

type rendition struct {
	name string
	w, h int
	kbps int
}

func renditions(l []LadderRung) []rendition {
	out := make([]rendition, len(l))
	for i, r := range l {
		out[i] = rendition{r.Name, r.Width, r.Height, r.VideoBitrateKbps}
	}
	return out
}

func kbps(v int) *int { return &v }

func TestEffectiveLadderKeepsOrientationAndAspect(t *testing.T) {
	cases := []struct {
		name    string
		w, h    int
		bitrate *int
		codec   string
		want    []rendition
	}{
		{"landscape 1080p is unchanged", 1920, 1080, nil, "h264", []rendition{
			{"360p", 640, 360, 800}, {"480p", 854, 480, 1400}, {"720p", 1280, 720, 2800}, {"1080p", 1920, 1080, 5000},
		}},
		{"portrait phone video gets every rung, upright", 1080, 1920, nil, "h264", []rendition{
			{"360p", 360, 640, 800}, {"480p", 480, 854, 1400}, {"720p", 720, 1280, 2800}, {"1080p", 1080, 1920, 5000},
		}},
		// The wedding ceremony: a 432×768 portrait download at ~344 kbps. It used to
		// become a single 640×360 frame (picture ~203×360 inside black bars) at 800 kbps.
		{"low-res portrait is upright and bitrate-capped", 432, 768, kbps(344), "h264", []rendition{
			{"360p", 360, 640, 430},
		}},
		{"square", 1080, 1080, nil, "h264", []rendition{
			{"360p", 360, 360, 800}, {"480p", 480, 480, 1400}, {"720p", 720, 720, 2800}, {"1080p", 1080, 1080, 5000},
		}},
		{"4:3 landscape", 1440, 1080, nil, "h264", []rendition{
			{"360p", 480, 360, 800}, {"480p", 640, 480, 1400}, {"720p", 960, 720, 2800}, {"1080p", 1440, 1080, 5000},
		}},
		{"below the lowest rung: no upscale", 320, 240, nil, "h264", []rendition{
			{"360p", 320, 240, 800},
		}},
		{"odd sizes round to even", 1081, 1921, nil, "h264", []rendition{
			{"360p", 360, 640, 800}, {"480p", 480, 852, 1400}, {"720p", 720, 1280, 2800}, {"1080p", 1080, 1920, 5000},
		}},
		{"odd below-ladder source rounds down, never up", 319, 239, nil, "h264", []rendition{
			{"360p", 318, 238, 800},
		}},
		{"rounding can't widen past the source", 641, 360, nil, "h264", []rendition{
			{"360p", 640, 360, 800},
		}},
		// H.264 needs about twice HEVC's bits: a good 1.5 Mbps HEVC 1080p must not
		// cap the H.264 1080p rung at 1.875 Mbps.
		{"HEVC source gets an H.264-equivalent cap", 1920, 1080, kbps(1500), "hevc", []rendition{
			{"360p", 640, 360, 800}, {"480p", 854, 480, 1400}, {"720p", 1280, 720, 2800}, {"1080p", 1920, 1080, 3750},
		}},
		{"unknown codec is not capped", 1920, 1080, kbps(300), "", []rendition{
			{"360p", 640, 360, 800}, {"480p", 854, 480, 1400}, {"720p", 1280, 720, 2800}, {"1080p", 1920, 1080, 5000},
		}},
		{"bitrate cap never drops below the floor", 1920, 1080, kbps(50), "h264", []rendition{
			{"360p", 640, 360, 200}, {"480p", 854, 480, 200}, {"720p", 1280, 720, 200}, {"1080p", 1920, 1080, 200},
		}},
	}
	for _, c := range cases {
		got := renditions(computeEffectiveLadder(DefaultLadder, c.w, c.h, c.bitrate, c.codec))
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	if DefaultLadder[0].Width != 640 || DefaultLadder[0].VideoBitrateKbps != 800 {
		t.Fatal("computeEffectiveLadder mutated DefaultLadder")
	}
}

func TestHLSArgsScaleWithoutPadding(t *testing.T) {
	ladder := computeEffectiveLadder(DefaultLadder, 1080, 1920, nil, "h264")
	args := strings.Join(buildHLSArgs(TranscodeOptions{Input: "in", OutputDir: "out", Ladder: ladder, HasAudio: true}), " ")

	if strings.Contains(args, "pad=") || strings.Contains(args, "force_original_aspect_ratio") {
		t.Fatalf("filter still letterboxes into a fixed box: %s", args)
	}
	for _, want := range []string{"[v0]scale=w=360:h=640,setsar=1[s0]", "[v3]scale=w=1080:h=1920,setsar=1[s3]"} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q:\n%s", want, args)
		}
	}
}

func TestProbeAppliesRotation(t *testing.T) {
	cases := []struct {
		name         string
		json         string
		w, h, rotate int
	}{
		{"display matrix (iPhone .mov)", `{"streams":[{"codec_type":"video","codec_name":"hevc","width":1920,"height":1080,
			"side_data_list":[{"side_data_type":"Display Matrix","rotation":-90}]}],"format":{"duration":"3.0","bit_rate":"8000000"}}`, 1080, 1920, 270},
		{"legacy rotate tag", `{"streams":[{"codec_type":"video","codec_name":"h264","width":1280,"height":720,
			"tags":{"rotate":"90"}}],"format":{}}`, 720, 1280, 90},
		{"upside down keeps orientation", `{"streams":[{"codec_type":"video","codec_name":"h264","width":1280,"height":720,
			"side_data_list":[{"side_data_type":"Display Matrix","rotation":180}]}],"format":{}}`, 1280, 720, 180},
		{"no rotation", `{"streams":[{"codec_type":"video","codec_name":"h264","width":432,"height":768}],"format":{}}`, 432, 768, 0},
		{"anamorphic 720x480 shown at 16:9", `{"streams":[{"codec_type":"video","codec_name":"mpeg2video","width":720,"height":480,
			"sample_aspect_ratio":"32:27"}],"format":{}}`, 853, 480, 0},
		{"anamorphic and rotated", `{"streams":[{"codec_type":"video","codec_name":"h264","width":720,"height":480,
			"sample_aspect_ratio":"32:27","side_data_list":[{"side_data_type":"Display Matrix","rotation":90}]}],"format":{}}`, 480, 853, 90},
		{"square pixels reported as 1:1", `{"streams":[{"codec_type":"video","codec_name":"h264","width":1280,"height":720,
			"sample_aspect_ratio":"1:1"}],"format":{}}`, 1280, 720, 0},
		{"unknown SAR 0:1", `{"streams":[{"codec_type":"video","codec_name":"h264","width":1280,"height":720,
			"sample_aspect_ratio":"0:1"}],"format":{}}`, 1280, 720, 0},
	}
	for _, c := range cases {
		got, err := parseProbeOutput([]byte(c.json), "test")
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got.Width != c.w || got.Height != c.h || got.Rotation != c.rotate {
			t.Errorf("%s: got %dx%d rot %d, want %dx%d rot %d", c.name, got.Width, got.Height, got.Rotation, c.w, c.h, c.rotate)
		}
	}
}

func TestNeedsReframe(t *testing.T) {
	sixteenNine := DefaultLadder
	fourThree := []LadderRung{{"480p", 640, 480, 1400, 128}, {"720p", 960, 720, 2800, 128}}
	cases := []struct {
		name   string
		w, h   int
		stored []LadderRung
		want   bool
	}{
		{"16:9 source, 16:9 boxes", 1920, 1080, sixteenNine, false},
		{"854x480 rounds to 16:9", 854, 480, sixteenNine, false},
		{"portrait in 16:9 boxes", 1080, 1920, sixteenNine, true},
		{"432x768 ceremony", 432, 768, sixteenNine, true},
		{"square", 1080, 1080, sixteenNine, true},
		{"4:3 in 16:9 boxes", 1440, 1080, sixteenNine, true},
		// The decision follows the ladder the old output was encoded with: a 4:3
		// source encoded under a 4:3 HLS_LADDER was fine, even if today's config differs.
		{"4:3 encoded with 4:3 boxes", 1440, 1080, fourThree, false},
		{"one stored rung of another aspect", 1920, 1080, append(slices.Clone(sixteenNine), fourThree[0]), true},
		{"no stored ladder: assume the worst", 1920, 1080, nil, true},
	}
	for _, c := range cases {
		if got := needsReframe(c.w, c.h, c.stored); got != c.want {
			t.Errorf("%s: needsReframe = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestVersionBelow(t *testing.T) {
	cases := []struct {
		v, threshold string
		want         bool
	}{
		{"0.1.0", "0.2.0", true}, {"0.2.0", "0.2.0", false}, {"0.10.0", "0.2.0", false},
		{"1.0", "0.2.0", false}, {"", "0.2.0", true}, {"garbage", "0.2.0", true}, {"v0.1.9", "0.2", true},
	}
	for _, c := range cases {
		if got := versionBelow(c.v, c.threshold); got != c.want {
			t.Errorf("versionBelow(%q, %q) = %v, want %v", c.v, c.threshold, got, c.want)
		}
	}
}

package core

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// requireFfmpeg skips without the ffmpeg and ffprobe binaries, unless
// REQUIRE_FFMPEG=1 (CI), where a missing binary must fail rather than skip.
func requireFfmpeg(t *testing.T) {
	t.Helper()
	for _, bin := range []string{findFfmpeg(), findFfprobe()} {
		if _, err := exec.LookPath(bin); err != nil {
			if os.Getenv("REQUIRE_FFMPEG") == "1" {
				t.Fatalf("%s required but not found: %v", bin, err)
			}
			t.Skipf("%s not found; set REQUIRE_FFMPEG=1 to make this fatal", bin)
		}
	}
}

// TestTranscodeToHLSProducesPublishableTrees runs real ffmpeg over a source
// that gets one rendition and one that gets several — the two cases ffmpeg's
// naming templates treat differently — and checks the published layout.
func TestTranscodeToHLSProducesPublishableTrees(t *testing.T) {
	requireFfmpeg(t)
	cases := []struct {
		name, size string
		wantRes    []string
	}{
		{"single rendition (portrait below 360)", "320x568", []string{"320x568"}},
		{"several renditions (portrait 720x1280)", "720x1280", []string{"360x640", "480x854", "720x1280"}},
	}
	resolution := regexp.MustCompile(`RESOLUTION=(\d+x\d+)`)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "src.mp4")
			gen := exec.Command(findFfmpeg(), "-loglevel", "error", "-y",
				"-f", "lavfi", "-i", "testsrc2=size="+c.size+":rate=24",
				"-f", "lavfi", "-i", "sine=frequency=440",
				"-t", "3", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", src)
			if out, err := gen.CombinedOutput(); err != nil {
				t.Fatalf("generating source: %v\n%s", err, out)
			}
			probe, err := ProbeSource(context.Background(), src)
			if err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(dir, "hls")
			err = TranscodeToHLS(context.Background(), TranscodeOptions{
				Input: src, OutputDir: out, HasAudio: probe.HasAudio, MediaTag: "0-2-0-test",
				Ladder: computeEffectiveLadder(DefaultLadder, probe.Width, probe.Height, probe.BitrateKbps, probe.VideoCodec),
			})
			if err != nil {
				t.Fatal(err)
			}

			master, _ := os.ReadFile(filepath.Join(out, "master.m3u8"))
			var got []string
			for _, m := range resolution.FindAllStringSubmatch(string(master), -1) {
				got = append(got, m[1])
			}
			if strings.Join(got, ",") != strings.Join(c.wantRes, ",") {
				t.Errorf("renditions %v, want %v", got, c.wantRes)
			}
			if err := filepath.WalkDir(out, func(p string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				name := d.Name()
				if strings.Contains(name, "%") {
					t.Errorf("unexpanded template in %s", p)
				}
				if strings.HasSuffix(name, ".m4s") || strings.HasSuffix(name, ".mp4") {
					if !strings.Contains(name, "0-2-0-test") {
						t.Errorf("media %s lacks the encode's tag", p)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

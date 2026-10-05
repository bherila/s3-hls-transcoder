package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// TranscodeOptions configures an HLS ABR transcode.
type TranscodeOptions struct {
	Input          string
	OutputDir      string
	Ladder         []LadderRung
	HasAudio       bool
	SegmentSeconds int // default 6
	GOPSize        int // default 48
	// MediaTag names this encode's segment and init objects. Empty means a
	// fresh newMediaTag(); set it only to make arguments deterministic in tests.
	MediaTag string
}

// TranscodeToHLS produces an HLS ABR ladder (fMP4/CMAF) under OutputDir:
// master.m3u8 + per-rung index.m3u8 / init.mp4 / seg_*.m4s.
func TranscodeToHLS(ctx context.Context, opts TranscodeOptions) error {
	if len(opts.Ladder) == 0 {
		return fmt.Errorf("ladder is empty")
	}
	if err := os.MkdirAll(opts.OutputDir, 0o755); err != nil {
		return err
	}
	for _, rung := range opts.Ladder {
		if err := os.MkdirAll(filepath.Join(opts.OutputDir, rung.Name), 0o755); err != nil {
			return err
		}
	}

	if opts.MediaTag == "" {
		tag, err := newMediaTag()
		if err != nil {
			return err
		}
		opts.MediaTag = tag
	}
	cmd := exec.CommandContext(ctx, findFfmpeg(), buildHLSArgs(opts)...)
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg transcode failed: %w\nstderr: %s", err, tail(errb.String(), 2000))
	}
	return nil
}

func buildHLSArgs(opts TranscodeOptions) []string {
	ladder := opts.Ladder
	segmentSeconds := opts.SegmentSeconds
	if segmentSeconds == 0 {
		segmentSeconds = 6
	}
	gopSize := opts.GOPSize
	if gopSize == 0 {
		gopSize = 48
	}

	// Filter graph: split video N ways and scale each to its rung's exact output
	// size (computeEffectiveLadder keeps the source aspect, so no padding).
	// ffmpeg auto-rotates on decode, so the frames are already upright.
	splitOutputs := ""
	for i := range ladder {
		splitOutputs += fmt.Sprintf("[v%d]", i)
	}
	splitClause := fmt.Sprintf("[0:v]split=%d%s", len(ladder), splitOutputs)
	scaleClauses := ""
	for i, rung := range ladder {
		if i > 0 {
			scaleClauses += ";"
		}
		scaleClauses += fmt.Sprintf(
			"[v%d]scale=w=%d:h=%d,setsar=1[s%d]",
			i, rung.Width, rung.Height, i)
	}
	filterComplex := splitClause + ";" + scaleClauses

	args := []string{"-y", "-i", opts.Input, "-filter_complex", filterComplex}

	for i, rung := range ladder {
		args = append(args,
			"-map", fmt.Sprintf("[s%d]", i),
			fmt.Sprintf("-c:v:%d", i), "libx264",
			fmt.Sprintf("-b:v:%d", i), fmt.Sprintf("%dk", rung.VideoBitrateKbps),
			fmt.Sprintf("-maxrate:v:%d", i), fmt.Sprintf("%dk", int(float64(rung.VideoBitrateKbps)*1.07+0.5)),
			fmt.Sprintf("-bufsize:v:%d", i), fmt.Sprintf("%dk", rung.VideoBitrateKbps*2),
			fmt.Sprintf("-profile:v:%d", i), "main",
			fmt.Sprintf("-preset:v:%d", i), "fast",
			fmt.Sprintf("-g:v:%d", i), strconv.Itoa(gopSize),
			fmt.Sprintf("-keyint_min:v:%d", i), strconv.Itoa(gopSize),
			fmt.Sprintf("-sc_threshold:v:%d", i), "0",
		)
	}

	if opts.HasAudio {
		for i, rung := range ladder {
			args = append(args,
				"-map", "0:a:0",
				fmt.Sprintf("-c:a:%d", i), "aac",
				fmt.Sprintf("-b:a:%d", i), fmt.Sprintf("%dk", rung.AudioBitrateKbps),
				fmt.Sprintf("-ac:a:%d", i), "2",
			)
		}
	}

	varStreamMap := ""
	for i, rung := range ladder {
		if i > 0 {
			varStreamMap += " "
		}
		if opts.HasAudio {
			varStreamMap += fmt.Sprintf("v:%d,a:%d,name:%s", i, i, rung.Name)
		} else {
			varStreamMap += fmt.Sprintf("v:%d,name:%s", i, rung.Name)
		}
	}

	args = append(args,
		"-f", "hls",
		"-hls_time", strconv.Itoa(segmentSeconds),
		"-hls_playlist_type", "vod",
		"-hls_segment_type", "fmp4",
		"-hls_flags", "independent_segments",
		// Media names are unique to this encode (see newMediaTag), so a
		// re-encode in place adds segment/init objects and never overwrites
		// ones that cached playlists still reference.
		"-hls_segment_filename", filepath.Join(opts.OutputDir, "%v", "seg_"+opts.MediaTag+"_%05d.m4s"),
		// ffmpeg requires %v in a custom init name once there are several
		// renditions; it expands to the rendition name, in that rendition's dir.
		"-hls_fmp4_init_filename", "init_"+opts.MediaTag+"_%v.mp4",
		"-master_pl_name", "master.m3u8",
		"-var_stream_map", varStreamMap,
		filepath.Join(opts.OutputDir, "%v", "index.m3u8"),
	)
	return args
}

// newMediaTag returns a name for one encode's media objects: the encoder
// version plus a random generation ("0-2-0-3fa9c1e2"). The generation is what
// keeps a retried re-encode (one whose master was published but a later
// metadata write failed) from overwriting the objects that master references;
// playlists carry the names, so nothing else needs to remember it.
func newMediaTag() (string, error) {
	gen := make([]byte, 4)
	if _, err := rand.Read(gen); err != nil {
		return "", err
	}
	return strings.ReplaceAll(Version, ".", "-") + "-" + hex.EncodeToString(gen), nil
}

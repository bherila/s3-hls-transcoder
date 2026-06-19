package core

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// TranscodeOptions configures an HLS ABR transcode.
type TranscodeOptions struct {
	Input          string
	OutputDir      string
	Ladder         []LadderRung
	HasAudio       bool
	SegmentSeconds int // default 6
	GOPSize        int // default 48
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

	// Filter graph: split video N ways, scale + pad each.
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
			"[v%d]scale=w=%d:h=%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2[s%d]",
			i, rung.Width, rung.Height, rung.Width, rung.Height, i)
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
		"-hls_segment_filename", filepath.Join(opts.OutputDir, "%v", "seg_%05d.m4s"),
		"-master_pl_name", "master.m3u8",
		"-var_stream_map", varStreamMap,
		filepath.Join(opts.OutputDir, "%v", "index.m3u8"),
	)
	return args
}

package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os/exec"
	"strconv"
)

// ProbeResult is the subset of ffprobe output we use.
type ProbeResult struct {
	Width           int
	Height          int
	DurationSeconds float64
	BitrateKbps     *int
	VideoCodec      string
	AudioCodec      string
	HasAudio        bool
}

type ffprobeOutput struct {
	Streams []struct {
		CodecType string `json:"codec_type"`
		CodecName string `json:"codec_name"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
		BitRate  string `json:"bit_rate"`
	} `json:"format"`
}

// ProbeSource extracts resolution, duration, audio presence, and bitrate.
func ProbeSource(ctx context.Context, input string) (*ProbeResult, error) {
	cmd := exec.CommandContext(ctx, findFfprobe(),
		"-v", "error", "-print_format", "json", "-show_streams", "-show_format", input)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffprobe failed: %w\nstderr: %s", err, tail(errb.String(), 1000))
	}

	var data ffprobeOutput
	if err := json.Unmarshal(out.Bytes(), &data); err != nil {
		return nil, fmt.Errorf("parsing ffprobe output: %w", err)
	}

	var video, audio *struct {
		CodecType string `json:"codec_type"`
		CodecName string `json:"codec_name"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
	}
	for i := range data.Streams {
		switch data.Streams[i].CodecType {
		case "video":
			if video == nil {
				video = &data.Streams[i]
			}
		case "audio":
			if audio == nil {
				audio = &data.Streams[i]
			}
		}
	}
	if video == nil || video.Width == 0 || video.Height == 0 {
		return nil, fmt.Errorf("no video stream found in %s", input)
	}

	res := &ProbeResult{
		Width:      video.Width,
		Height:     video.Height,
		VideoCodec: video.CodecName,
		HasAudio:   audio != nil,
	}
	if data.Format.Duration != "" {
		if d, err := strconv.ParseFloat(data.Format.Duration, 64); err == nil {
			res.DurationSeconds = d
		}
	}
	if data.Format.BitRate != "" {
		if br, err := strconv.ParseFloat(data.Format.BitRate, 64); err == nil {
			kbps := int(math.Round(br / 1000))
			res.BitrateKbps = &kbps
		}
	}
	if audio != nil {
		res.AudioCodec = audio.CodecName
	}
	return res, nil
}

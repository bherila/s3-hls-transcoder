package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
)

// ProbeResult is the subset of ffprobe output we use.
type ProbeResult struct {
	// Width and Height are the display size: the coded size with the
	// stream's rotation applied (phones store portrait video as rotated
	// landscape frames).
	Width           int
	Height          int
	Rotation        int
	DurationSeconds float64
	BitrateKbps     *int
	VideoCodec      string
	AudioCodec      string
	HasAudio        bool
}

type ffprobeStream struct {
	CodecType string `json:"codec_type"`
	CodecName string `json:"codec_name"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	// SampleAspectRatio is the pixel shape, e.g. "32:27" for anamorphic
	// 720×480 shown at 16:9; "1:1", "0:1" or absent means square pixels.
	SampleAspectRatio string `json:"sample_aspect_ratio"`
	SideDataList      []struct {
		SideDataType string  `json:"side_data_type"`
		Rotation     float64 `json:"rotation"`
	} `json:"side_data_list"`
	Tags struct {
		Rotate string `json:"rotate"`
	} `json:"tags"`
}

type ffprobeOutput struct {
	Streams []ffprobeStream `json:"streams"`
	Format  struct {
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

	return parseProbeOutput(out.Bytes(), input)
}

// parseProbeOutput turns ffprobe's JSON into a ProbeResult.
func parseProbeOutput(raw []byte, input string) (*ProbeResult, error) {
	var data ffprobeOutput
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("parsing ffprobe output: %w", err)
	}

	var video, audio *ffprobeStream
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

	rotation := streamRotation(video)
	width, height := video.Width, video.Height
	// Non-square pixels: the display width is the coded width × SAR, which is
	// what the rungs must be sized from (FFmpeg: DAR = iw/ih × sar).
	if num, den, ok := parseRatio(video.SampleAspectRatio); ok && num != den {
		width = int(math.Round(float64(width) * float64(num) / float64(den)))
	}
	if rotation%180 != 0 {
		width, height = height, width
	}
	res := &ProbeResult{
		Width:      width,
		Height:     height,
		Rotation:   rotation,
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

// streamRotation returns the stream's display rotation in degrees, normalized
// to 0, 90, 180 or 270. Newer ffprobe reports it as Display Matrix side data,
// older versions as a "rotate" tag.
func streamRotation(s *ffprobeStream) int {
	deg := 0.0
	found := false
	for _, sd := range s.SideDataList {
		if sd.SideDataType == "Display Matrix" {
			deg, found = sd.Rotation, true
			break
		}
	}
	if !found && s.Tags.Rotate != "" {
		if v, err := strconv.ParseFloat(s.Tags.Rotate, 64); err == nil {
			deg = v
		}
	}
	r := int(math.Round(deg/90)) * 90 % 360
	if r < 0 {
		r += 360
	}
	return r
}

// parseRatio parses "num:den" with both parts positive.
func parseRatio(s string) (int, int, bool) {
	n, d, found := strings.Cut(s, ":")
	if !found {
		return 0, 0, false
	}
	num, err1 := strconv.Atoi(n)
	den, err2 := strconv.Atoi(d)
	if err1 != nil || err2 != nil || num <= 0 || den <= 0 {
		return 0, 0, false
	}
	return num, den, true
}

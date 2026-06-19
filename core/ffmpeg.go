package core

import (
	"os"
	"os/exec"
	"sync"
)

var (
	ffmpegOnce  sync.Once
	ffprobeOnce sync.Once
	ffmpegPath  string
	ffprobePath string
)

var (
	ffmpegCandidates  = []string{"/usr/local/bin/ffmpeg", "/usr/bin/ffmpeg", "/opt/homebrew/bin/ffmpeg"}
	ffprobeCandidates = []string{"/usr/local/bin/ffprobe", "/usr/bin/ffprobe", "/opt/homebrew/bin/ffprobe"}
)

func resolveBinary(envVar string, candidates []string, name string) string {
	if fromEnv := os.Getenv(envVar); fromEnv != "" {
		return fromEnv
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	// Fall back to the bare name; exec will fail loudly if it is missing.
	return name
}

func findFfmpeg() string {
	ffmpegOnce.Do(func() { ffmpegPath = resolveBinary("FFMPEG_PATH", ffmpegCandidates, "ffmpeg") })
	return ffmpegPath
}

func findFfprobe() string {
	ffprobeOnce.Do(func() { ffprobePath = resolveBinary("FFPROBE_PATH", ffprobeCandidates, "ffprobe") })
	return ffprobePath
}

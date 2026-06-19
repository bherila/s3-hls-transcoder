package core

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
	"os/exec"
	"strconv"
)

// Perceptual fingerprint: a sequence of dHashes over keyframes sampled at a
// fixed cadence, computed from 9x8 grayscale frames produced by ffmpeg. Robust
// to scaling and re-encoding — i.e. "same content, different quality".
//
// The heavy image work (decode, sample, scale, grayscale) is delegated to
// ffmpeg's filter graph, so this output is identical to the original Node
// implementation given the same ffmpeg build. dHash + serialization here are
// deterministic integer/byte math.
const (
	frameW     = 9
	frameH     = 8
	frameBytes = frameW * frameH
	hashBits   = 64
)

// VideoFingerprint is a sequence of per-frame dHashes plus the sampling cadence.
type VideoFingerprint struct {
	Hashes          []uint64
	IntervalSeconds float64
}

// FingerprintVideo samples input at fps=1/interval, scales each frame to 9x8
// grayscale, and dHashes it. intervalSeconds <= 0 defaults to 2.
func FingerprintVideo(ctx context.Context, input string, intervalSeconds float64) (*VideoFingerprint, error) {
	if intervalSeconds <= 0 {
		intervalSeconds = 2
	}
	vf := fmt.Sprintf("fps=1/%s,scale=%d:%d:flags=lanczos,format=gray",
		strconv.FormatFloat(intervalSeconds, 'g', -1, 64), frameW, frameH)

	cmd := exec.CommandContext(ctx, findFfmpeg(), "-i", input, "-vf", vf,
		"-f", "rawvideo", "-pix_fmt", "gray", "-")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg fingerprint failed: %w\nstderr: %s", err, tail(stderr.String(), 1000))
	}

	all := stdout.Bytes()
	numFrames := len(all) / frameBytes
	hashes := make([]uint64, 0, numFrames)
	for i := 0; i < numFrames; i++ {
		hashes = append(hashes, dhash(all[i*frameBytes:(i+1)*frameBytes]))
	}
	return &VideoFingerprint{Hashes: hashes, IntervalSeconds: intervalSeconds}, nil
}

// dhash compares horizontally-adjacent pixels in a 9x8 grayscale frame,
// producing one bit per comparison (8x8 = 64 bits).
func dhash(frame []byte) uint64 {
	var hash uint64
	bit := uint(0)
	for y := 0; y < frameH; y++ {
		for x := 0; x < frameW-1; x++ {
			if frame[y*frameW+x] > frame[y*frameW+x+1] {
				hash |= 1 << bit
			}
			bit++
		}
	}
	return hash
}

// Popcount64 returns the number of set bits.
func Popcount64(n uint64) int { return bits.OnesCount64(n) }

// FingerprintSimilarity is the mean per-frame Hamming similarity in [0,1],
// aligned by index over the shorter sequence (1 = identical, 0 = maximally
// distant). Returns 0 if either sequence is empty.
func FingerprintSimilarity(a, b *VideoFingerprint) float64 {
	minFrames := len(a.Hashes)
	if len(b.Hashes) < minFrames {
		minFrames = len(b.Hashes)
	}
	if minFrames == 0 {
		return 0
	}
	total := 0
	for i := 0; i < minFrames; i++ {
		total += bits.OnesCount64(a.Hashes[i] ^ b.Hashes[i])
	}
	avg := float64(total) / float64(minFrames)
	s := 1 - avg/float64(hashBits)
	if s < 0 {
		s = 0
	}
	if s > 1 {
		s = 1
	}
	return s
}

const headerBytes = 8

// SerializeFingerprint encodes a fingerprint as: float32 LE interval +
// uint32 LE count + count x uint64 LE hashes.
func SerializeFingerprint(fp *VideoFingerprint) []byte {
	buf := make([]byte, headerBytes+len(fp.Hashes)*8)
	binary.LittleEndian.PutUint32(buf[0:], math.Float32bits(float32(fp.IntervalSeconds)))
	binary.LittleEndian.PutUint32(buf[4:], uint32(len(fp.Hashes)))
	for i, h := range fp.Hashes {
		binary.LittleEndian.PutUint64(buf[headerBytes+i*8:], h)
	}
	return buf
}

// DeserializeFingerprint reverses SerializeFingerprint.
func DeserializeFingerprint(buf []byte) (*VideoFingerprint, error) {
	if len(buf) < headerBytes {
		return nil, fmt.Errorf("fingerprint blob too short: %d bytes", len(buf))
	}
	interval := float64(math.Float32frombits(binary.LittleEndian.Uint32(buf[0:])))
	n := binary.LittleEndian.Uint32(buf[4:])
	if len(buf) < headerBytes+int(n)*8 {
		return nil, fmt.Errorf("fingerprint blob truncated: want %d hashes, have %d bytes", n, len(buf))
	}
	hashes := make([]uint64, n)
	for i := 0; i < int(n); i++ {
		hashes[i] = binary.LittleEndian.Uint64(buf[headerBytes+i*8:])
	}
	return &VideoFingerprint{Hashes: hashes, IntervalSeconds: interval}, nil
}

func tail(s string, n int) string {
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

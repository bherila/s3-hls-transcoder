package core

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// PDQ perceptual image hashing. The heavy image work (decode, downsample,
// Jarosz blur, DCT) is delegated to the C++ pdq-photo-hasher tool from
// Facebook/ThreatExchange — exactly how video work is delegated to ffmpeg. We
// only exec it and parse its output, so the hash stays bit-for-bit identical to
// the reference implementation other systems compare against.
//
// A PDQ hash is 256 bits / 64 hex chars; quality is 0..100 (low quality means a
// flat image whose hash is less reliable). Hashes are compared by Hamming
// distance downstream (the app), not here.

var (
	pdqHasherOnce sync.Once
	pdqHasherPath string
)

// pdqHasherCandidates are probed in order when PDQ_HASHER_PATH is unset. The
// upstream Facebook/ThreatExchange Makefile target is named `pdq-photo-hasher`;
// the `-tool` variants are accepted in case a package installs under that name.
var pdqHasherCandidates = []string{
	"/usr/local/bin/pdq-photo-hasher",
	"/usr/bin/pdq-photo-hasher",
	"/usr/local/bin/pdq-photo-hasher-tool",
	"/usr/bin/pdq-photo-hasher-tool",
}

func findPDQHasher() string {
	pdqHasherOnce.Do(func() {
		pdqHasherPath = resolveBinary("PDQ_HASHER_PATH", pdqHasherCandidates, "pdq-photo-hasher")
	})
	return pdqHasherPath
}

// PDQResult is one image's perceptual hash and its quality score.
type PDQResult struct {
	Hash    string // 64 lowercase hex chars (256 bits)
	Quality int    // 0..100
}

// ComputePDQ runs the pdq-photo-hasher tool on a local image file and returns
// its hash. The tool takes the image path as its sole argument and prints the
// hash to stdout; see parsePDQOutput for the accepted output shapes.
func ComputePDQ(ctx context.Context, input string) (PDQResult, error) {
	cmd := exec.CommandContext(ctx, findPDQHasher(), input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return PDQResult{}, fmt.Errorf("pdq hasher failed: %w\nstderr: %s", err, tail(stderr.String(), 1000))
	}

	res, err := parsePDQOutput(stdout.String())
	if err != nil {
		return PDQResult{}, fmt.Errorf("%w\nstdout: %s", err, tail(stdout.String(), 1000))
	}
	return res, nil
}

var hex64Pattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// parsePDQOutput extracts the hash and quality from the tool's stdout. The
// reference tools print comma- or whitespace-separated fields (commonly
// "hash,quality,norm,delta,filename" or "hash quality filename"), sometimes with
// a header line. Rather than pin one layout, we scan tokens: the first 64-hex
// token is the hash, and the first plausible 0..100 integer after it is the
// quality. This tolerates layout differences across tool versions.
func parsePDQOutput(out string) (PDQResult, error) {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.FieldsFunc(line, func(r rune) bool {
			return r == ',' || r == '\t' || r == ' ' || r == '\r'
		})

		hashIdx := -1
		for i, f := range fields {
			if hex64Pattern.MatchString(f) {
				hashIdx = i
				break
			}
		}
		if hashIdx == -1 {
			continue
		}

		quality := 0
		for _, f := range fields[hashIdx+1:] {
			if n, err := strconv.Atoi(f); err == nil && n >= 0 && n <= 100 {
				quality = n
				break
			}
		}
		return PDQResult{Hash: strings.ToLower(fields[hashIdx]), Quality: quality}, nil
	}
	return PDQResult{}, fmt.Errorf("no 64-hex PDQ hash found in hasher output")
}

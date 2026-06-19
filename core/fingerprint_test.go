package core

import (
	"math"
	"testing"
)

// Golden vectors mirror lib/src/fingerprint.test.ts to prove the Go dHash,
// similarity, and serialization are bit-for-bit compatible with the original.

func TestPopcount64(t *testing.T) {
	cases := []struct {
		in   uint64
		want int
	}{
		{0, 0}, {1, 1}, {0xff, 8}, {0xffffffffffffffff, 64},
	}
	for _, c := range cases {
		if got := Popcount64(c.in); got != c.want {
			t.Errorf("Popcount64(%#x) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestFingerprintSimilarity(t *testing.T) {
	fp := &VideoFingerprint{Hashes: []uint64{0x1234567890abcdef, 0xfedcba0987654321}, IntervalSeconds: 2}
	if got := FingerprintSimilarity(fp, fp); got != 1 {
		t.Errorf("identical similarity = %v, want 1", got)
	}

	empty := &VideoFingerprint{Hashes: []uint64{}, IntervalSeconds: 2}
	one := &VideoFingerprint{Hashes: []uint64{0}, IntervalSeconds: 2}
	if got := FingerprintSimilarity(empty, one); got != 0 {
		t.Errorf("empty similarity = %v, want 0", got)
	}

	a := &VideoFingerprint{Hashes: []uint64{0}, IntervalSeconds: 2}
	b := &VideoFingerprint{Hashes: []uint64{0xffffffffffffffff}, IntervalSeconds: 2}
	if got := FingerprintSimilarity(a, b); got != 0 {
		t.Errorf("fully-flipped similarity = %v, want 0", got)
	}

	// Aligns by index over the shorter sequence; trailing frame ignored.
	a3 := &VideoFingerprint{Hashes: []uint64{0, 0, 0xff}, IntervalSeconds: 2}
	b2 := &VideoFingerprint{Hashes: []uint64{0, 0}, IntervalSeconds: 2}
	if got := FingerprintSimilarity(a3, b2); got != 1 {
		t.Errorf("length-mismatch similarity = %v, want 1", got)
	}

	// 8 bits flipped out of 64 = 87.5%.
	a1 := &VideoFingerprint{Hashes: []uint64{0}, IntervalSeconds: 2}
	b1 := &VideoFingerprint{Hashes: []uint64{0xff}, IntervalSeconds: 2}
	if got := FingerprintSimilarity(a1, b1); math.Abs(got-(1-8.0/64.0)) > 1e-9 {
		t.Errorf("intermediate similarity = %v, want %v", got, 1-8.0/64.0)
	}
}

func TestFingerprintSerializationRoundTrip(t *testing.T) {
	fp := &VideoFingerprint{
		Hashes:          []uint64{0x1234567890abcdef, 0xdeadbeefcafebabe, 0, 0xffffffffffffffff},
		IntervalSeconds: 2.5,
	}
	decoded, err := DeserializeFingerprint(SerializeFingerprint(fp))
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(decoded.IntervalSeconds-2.5) > 1e-6 {
		t.Errorf("interval = %v, want 2.5", decoded.IntervalSeconds)
	}
	if len(decoded.Hashes) != len(fp.Hashes) {
		t.Fatalf("hash count = %d, want %d", len(decoded.Hashes), len(fp.Hashes))
	}
	for i := range fp.Hashes {
		if decoded.Hashes[i] != fp.Hashes[i] {
			t.Errorf("hash[%d] = %#x, want %#x", i, decoded.Hashes[i], fp.Hashes[i])
		}
	}
}

func TestFingerprintSerializationEmpty(t *testing.T) {
	fp := &VideoFingerprint{Hashes: []uint64{}, IntervalSeconds: 1}
	decoded, err := DeserializeFingerprint(SerializeFingerprint(fp))
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(decoded.IntervalSeconds-1) > 1e-6 {
		t.Errorf("interval = %v, want 1", decoded.IntervalSeconds)
	}
	if len(decoded.Hashes) != 0 {
		t.Errorf("hash count = %d, want 0", len(decoded.Hashes))
	}
}

// dHash sanity: a frame whose every left pixel exceeds its right neighbor is
// all-ones; the reverse is all-zeros.
func TestDhashBoundaries(t *testing.T) {
	desc := make([]byte, frameBytes) // strictly decreasing per row → left>right always
	for y := 0; y < frameH; y++ {
		for x := 0; x < frameW; x++ {
			desc[y*frameW+x] = byte(frameW - x)
		}
	}
	if got := dhash(desc); got != 0xffffffffffffffff {
		t.Errorf("descending dhash = %#x, want all ones", got)
	}
	asc := make([]byte, frameBytes)
	for y := 0; y < frameH; y++ {
		for x := 0; x < frameW; x++ {
			asc[y*frameW+x] = byte(x)
		}
	}
	if got := dhash(asc); got != 0 {
		t.Errorf("ascending dhash = %#x, want 0", got)
	}
}

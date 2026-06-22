package core

import "testing"

func TestParsePDQOutput(t *testing.T) {
	const hash = "f8f8f0f0e0e0c0c08080000011112222333344445555666677778888999900aa"

	cases := []struct {
		name        string
		in          string
		wantHash    string
		wantQuality int
	}{
		{"comma hash,quality,filename", hash + ",100,/tmp/x.jpg", hash, 100},
		{"tab separated", hash + "\t97\tfoo.png", hash, 97},
		{"space separated", hash + " 42 foo.png", hash, 42},
		{"uppercase normalized", "F8F8F0F0E0E0C0C08080000011112222333344445555666677778888999900AA,55,x", hash, 55},
		{"extra columns norm/delta", hash + ",88,1.0,0,/tmp/x.jpg", hash, 88},
		{"header line then data", "hash,quality,filename\n" + hash + ",73,a.jpg", hash, 73},
		{"no quality defaults to zero", hash + ",,a.jpg", hash, 0},
		{"quality out of range skipped", hash + ",250,a.jpg", hash, 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := parsePDQOutput(c.in)
			if err != nil {
				t.Fatalf("parsePDQOutput(%q) error: %v", c.in, err)
			}
			if res.Hash != c.wantHash {
				t.Errorf("hash = %q, want %q", res.Hash, c.wantHash)
			}
			if res.Quality != c.wantQuality {
				t.Errorf("quality = %d, want %d", res.Quality, c.wantQuality)
			}
		})
	}
}

func TestParsePDQOutputErrors(t *testing.T) {
	cases := []string{
		"",
		"no hash here",
		"too short abcdef0123,100,x",
		"deadbeef,100,x", // 8 hex chars, not 64
	}
	for _, in := range cases {
		if _, err := parsePDQOutput(in); err == nil {
			t.Errorf("parsePDQOutput(%q) expected error, got nil", in)
		}
	}
}

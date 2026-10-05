package core

import (
	"strings"
	"testing"
)

func TestBucketsOverlap(t *testing.T) {
	base := BucketConfig{Bucket: "videos", Endpoint: "https://r2.example.com", AccessKeyID: "ak", SecretAccessKey: "sk", Region: "auto"}
	with := func(mut func(*BucketConfig)) BucketConfig {
		c := base
		mut(&c)
		return c
	}

	cases := []struct {
		name string
		a, b BucketConfig
		want bool
	}{
		{"different endpoint", base, with(func(c *BucketConfig) { c.Endpoint = "https://other.example" }), false},
		{"different bucket", base, with(func(c *BucketConfig) { c.Bucket = "other" }), false},
		{"same, no prefixes", base, base, true},
		{"prefix subsumes a→b", with(func(c *BucketConfig) { c.Prefix = "uploads/" }), with(func(c *BucketConfig) { c.Prefix = "uploads/transcoded/" }), true},
		{"prefix subsumes b→a", with(func(c *BucketConfig) { c.Prefix = "uploads/transcoded/" }), with(func(c *BucketConfig) { c.Prefix = "uploads/" }), true},
		{"disjoint prefixes", with(func(c *BucketConfig) { c.Prefix = "uploads/" }), with(func(c *BucketConfig) { c.Prefix = "transcoded/" }), false},
		{"empty prefix subsumes", base, with(func(c *BucketConfig) { c.Prefix = "anything/" }), true},
		{"endpoint case-insensitive", with(func(c *BucketConfig) { c.Endpoint = "https://R2.Example.Com" }), base, true},
		{"endpoint trailing slash", with(func(c *BucketConfig) { c.Endpoint = "https://r2.example.com/" }), base, true},
	}
	for _, c := range cases {
		if got := BucketsOverlap(c.a, c.b); got != c.want {
			t.Errorf("%s: BucketsOverlap = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestLoadConfigRejectsSharedDestinations(t *testing.T) {
	t.Setenv("BUCKETS_CONFIG_FILE", "")
	t.Setenv("BUCKETS_CONFIG", `[
		{"source":{"bucket":"src-a","endpoint":"https://r2.example.com"},"dest":{"bucket":"shared-dest","endpoint":"https://R2.Example.com/path"},"accessKeyId":"ak","secretAccessKey":"sk"},
		{"source":{"bucket":"src-b","endpoint":"https://r2.example.com"},"dest":{"bucket":"shared-dest","endpoint":"https://r2.example.com"},"accessKeyId":"ak","secretAccessKey":"sk"}
	]`)
	_, err := LoadConfig(PlatformLocal)
	if err == nil {
		t.Fatal("expected shared destination buckets to be rejected")
	}
	if !strings.Contains(err.Error(), "destination buckets must be unique") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLoadConfigAllowsDistinctDestinations(t *testing.T) {
	t.Setenv("BUCKETS_CONFIG_FILE", "")
	t.Setenv("BUCKETS_CONFIG", `[
		{"source":{"bucket":"src-a","endpoint":"https://r2.example.com"},"dest":{"bucket":"dest-a","endpoint":"https://r2.example.com"},"accessKeyId":"ak","secretAccessKey":"sk"},
		{"source":{"bucket":"src-b","endpoint":"https://r2.example.com"},"dest":{"bucket":"dest-b","endpoint":"https://r2.example.com"},"accessKeyId":"ak","secretAccessKey":"sk"}
	]`)
	cfg, err := LoadConfig(PlatformLocal)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Pairs) != 2 {
		t.Errorf("pairs = %d, want 2", len(cfg.Pairs))
	}
}

func TestReencodeThresholdCannotExceedEncoderVersion(t *testing.T) {
	t.Setenv("BUCKETS_CONFIG_FILE", "")
	t.Setenv("BUCKETS_CONFIG", `[{"source":{"bucket":"src","endpoint":"https://r2.example.com"},"dest":{"bucket":"dst","endpoint":"https://r2.example.com"},"accessKeyId":"ak","secretAccessKey":"sk"}]`)

	t.Setenv("REENCODE_BELOW_VERSION", Version)
	if cfg, err := LoadConfig(PlatformLocal); err != nil || cfg.ReencodeBelowVersion != Version {
		t.Fatalf("threshold equal to Version should load: cfg=%+v err=%v", cfg, err)
	}

	// Restamps write Version, so a newer threshold would re-check every output forever.
	t.Setenv("REENCODE_BELOW_VERSION", "99.0.0")
	if _, err := LoadConfig(PlatformLocal); err == nil || !strings.Contains(err.Error(), "newer than this encoder") {
		t.Fatalf("expected a threshold above Version to be rejected, got %v", err)
	}

	t.Setenv("REENCODE_BELOW_VERSION", "not-a-version")
	if _, err := LoadConfig(PlatformLocal); err == nil {
		t.Fatal("expected a malformed threshold to be rejected")
	}
}

func TestLadderRungNamesMustBePublishable(t *testing.T) {
	rung := func(name string) string {
		return `[{"name":"` + name + `","width":640,"height":360,"videoBitrateKbps":800,"audioBitrateKbps":96}]`
	}
	for _, ok := range []string{"360p", "mobile/360p", "low_360-v2.1"} {
		if _, err := parseLadder(rung(ok)); err != nil {
			t.Errorf("%q should be accepted: %v", ok, err)
		}
	}
	// Accepted before, but every encode would then fail validation (or a proxy).
	for _, bad := range []string{"mobile~360p", "360 p", "../360p", "a/./b", "/360p", "360p%v"} {
		if _, err := parseLadder(rung(bad)); err == nil {
			t.Errorf("%q should be rejected at startup", bad)
		}
	}
}

package core

import "testing"

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

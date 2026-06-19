package core

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Platform selects platform-specific defaults (currently only the runtime ceiling).
type Platform string

const (
	PlatformAWSLambda           Platform = "aws-lambda"
	PlatformCloudflareContainer Platform = "cloudflare-container"
	PlatformLocal               Platform = "local"
)

// BucketConfig is a fully-resolved S3 bucket target.
type BucketConfig struct {
	Bucket          string
	Endpoint        string
	AccessKeyID     string
	SecretAccessKey string
	Region          string
	Prefix          string
}

// BucketPair is one source→dest transcoding job.
type BucketPair struct {
	Source BucketConfig
	Dest   BucketConfig
}

// LadderRung is one ABR rung.
type LadderRung struct {
	Name             string `json:"name"`
	Width            int    `json:"width"`
	Height           int    `json:"height"`
	VideoBitrateKbps int    `json:"videoBitrateKbps"`
	AudioBitrateKbps int    `json:"audioBitrateKbps"`
}

// Config is the fully-resolved runtime configuration.
type Config struct {
	Pairs                 []BucketPair
	Ladder                []LadderRung
	MaxRuntimeSeconds     float64
	LockTTLMultiplier     float64
	BudgetMultiplier      float64
	PerceptualThreshold   float64
	PerceptualDryRun      bool
	CleanupDeletedSources bool
	CleanupDryRun         bool
	ErrorTombstones       bool
	TombstoneMaxAttempts  int
	MaxConcurrency        int
	LogLevel              LogLevel
	Platform              Platform
}

// DefaultLadder is the v1 H.264/AAC ABR ladder.
var DefaultLadder = []LadderRung{
	{"360p", 640, 360, 800, 96},
	{"480p", 854, 480, 1400, 128},
	{"720p", 1280, 720, 2800, 128},
	{"1080p", 1920, 1080, 5000, 192},
}

var platformDefaultMaxRuntime = map[Platform]float64{
	PlatformAWSLambda:           900,
	PlatformCloudflareContainer: 3600,
	PlatformLocal:               3600,
}

// LoadConfig builds a Config from the environment for the given platform.
func LoadConfig(platform Platform) (*Config, error) {
	pairs, err := loadPairs()
	if err != nil {
		return nil, err
	}
	if err := validateNoOverlaps(pairs); err != nil {
		return nil, err
	}
	ladder, err := parseLadder(os.Getenv("HLS_LADDER"))
	if err != nil {
		return nil, err
	}
	logLevel, err := parseLogLevel(os.Getenv("LOG_LEVEL"))
	if err != nil {
		return nil, err
	}
	maxRuntime, err := parseNumber("MAX_RUNTIME_SECONDS", platformDefaultMaxRuntime[platform])
	if err != nil {
		return nil, err
	}
	lockTTLMul, err := parseNumber("LOCK_TTL_MULTIPLIER", 1.5)
	if err != nil {
		return nil, err
	}
	budgetMul, err := parseNumber("BUDGET_MULTIPLIER", 0.75)
	if err != nil {
		return nil, err
	}
	threshold, err := parseNumber("PERCEPTUAL_THRESHOLD", 0.95)
	if err != nil {
		return nil, err
	}
	maxConc, err := parseNumber("MAX_CONCURRENCY", 1)
	if err != nil {
		return nil, err
	}
	tombstoneAttempts, err := parseNumber("TOMBSTONE_MAX_ATTEMPTS", 3)
	if err != nil {
		return nil, err
	}

	return &Config{
		Pairs:                 pairs,
		Ladder:                ladder,
		MaxRuntimeSeconds:     maxRuntime,
		LockTTLMultiplier:     lockTTLMul,
		BudgetMultiplier:      budgetMul,
		PerceptualThreshold:   threshold,
		PerceptualDryRun:      os.Getenv("PERCEPTUAL_DRY_RUN") == "true",
		CleanupDeletedSources: os.Getenv("CLEANUP_DELETED_SOURCES") == "true",
		CleanupDryRun:         os.Getenv("CLEANUP_DRY_RUN") == "true",
		ErrorTombstones:       os.Getenv("ERROR_TOMBSTONES") != "false",
		TombstoneMaxAttempts:  int(tombstoneAttempts),
		MaxConcurrency:        int(maxConc),
		LogLevel:              logLevel,
		Platform:              platform,
	}, nil
}

type bucketInput struct {
	Bucket          string `json:"bucket"`
	Endpoint        string `json:"endpoint"`
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
	Region          string `json:"region"`
	Prefix          string `json:"prefix"`
}

type pairInput struct {
	Source          *bucketInput `json:"source"`
	Dest            *bucketInput `json:"dest"`
	AccessKeyID     string       `json:"accessKeyId"`
	SecretAccessKey string       `json:"secretAccessKey"`
	Region          string       `json:"region"`
}

// loadPairs resolves bucket pairs in priority order: BUCKETS_CONFIG_FILE,
// BUCKETS_CONFIG, then a single pair from SOURCE_*/DEST_* env vars.
func loadPairs() ([]BucketPair, error) {
	if path := os.Getenv("BUCKETS_CONFIG_FILE"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("BUCKETS_CONFIG_FILE: %w", err)
		}
		return parseBucketsConfigJSON(raw, "BUCKETS_CONFIG_FILE")
	}
	if raw := os.Getenv("BUCKETS_CONFIG"); raw != "" {
		return parseBucketsConfigJSON([]byte(raw), "BUCKETS_CONFIG")
	}
	pair, err := singlePairFromEnv()
	if err != nil {
		return nil, err
	}
	return []BucketPair{pair}, nil
}

func parseBucketsConfigJSON(raw []byte, sourceName string) ([]BucketPair, error) {
	var inputs []pairInput
	if err := json.Unmarshal(raw, &inputs); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", sourceName, err)
	}
	if len(inputs) == 0 {
		return nil, fmt.Errorf("%s must be a non-empty JSON array of bucket pairs", sourceName)
	}
	pairs := make([]BucketPair, 0, len(inputs))
	for i, in := range inputs {
		if in.Source == nil || in.Dest == nil {
			return nil, fmt.Errorf("%s[%d] must have both 'source' and 'dest' fields", sourceName, i)
		}
		src, err := resolveBucket(*in.Source, in, "source", i, sourceName)
		if err != nil {
			return nil, err
		}
		dst, err := resolveBucket(*in.Dest, in, "dest", i, sourceName)
		if err != nil {
			return nil, err
		}
		pairs = append(pairs, BucketPair{Source: src, Dest: dst})
	}
	return pairs, nil
}

func resolveBucket(b bucketInput, pair pairInput, side string, idx int, sourceName string) (BucketConfig, error) {
	if b.Bucket == "" {
		return BucketConfig{}, fmt.Errorf("%s[%d].%s.bucket is required", sourceName, idx, side)
	}
	if b.Endpoint == "" {
		return BucketConfig{}, fmt.Errorf("%s[%d].%s.endpoint is required for bucket '%s'", sourceName, idx, side, b.Bucket)
	}
	envPrefix := "DEST"
	if side == "source" {
		envPrefix = "SOURCE"
	}
	accessKeyID := firstNonEmpty(b.AccessKeyID, pair.AccessKeyID, os.Getenv(envPrefix+"_ACCESS_KEY_ID"))
	secretAccessKey := firstNonEmpty(b.SecretAccessKey, pair.SecretAccessKey, os.Getenv(envPrefix+"_SECRET_ACCESS_KEY"))
	region := firstNonEmpty(b.Region, pair.Region, os.Getenv(envPrefix+"_REGION"), "auto")
	if accessKeyID == "" || secretAccessKey == "" {
		return BucketConfig{}, fmt.Errorf("%s[%d].%s bucket '%s' is missing credentials (bucket/pair level or %s_ACCESS_KEY_ID / %s_SECRET_ACCESS_KEY)",
			sourceName, idx, side, b.Bucket, envPrefix, envPrefix)
	}
	return BucketConfig{
		Bucket: b.Bucket, Endpoint: b.Endpoint, AccessKeyID: accessKeyID,
		SecretAccessKey: secretAccessKey, Region: region, Prefix: b.Prefix,
	}, nil
}

func singlePairFromEnv() (BucketPair, error) {
	src, err := requiredBucket("SOURCE")
	if err != nil {
		return BucketPair{}, err
	}
	src.Prefix = os.Getenv("SOURCE_PREFIX")
	dst, err := requiredBucket("DEST")
	if err != nil {
		return BucketPair{}, err
	}
	return BucketPair{Source: src, Dest: dst}, nil
}

func requiredBucket(prefix string) (BucketConfig, error) {
	get := func(suffix string) (string, error) {
		v := os.Getenv(prefix + "_" + suffix)
		if v == "" {
			return "", fmt.Errorf("missing required env var: %s_%s", prefix, suffix)
		}
		return v, nil
	}
	bucket, err := get("BUCKET")
	if err != nil {
		return BucketConfig{}, err
	}
	endpoint, err := get("ENDPOINT")
	if err != nil {
		return BucketConfig{}, err
	}
	ak, err := get("ACCESS_KEY_ID")
	if err != nil {
		return BucketConfig{}, err
	}
	sk, err := get("SECRET_ACCESS_KEY")
	if err != nil {
		return BucketConfig{}, err
	}
	region := os.Getenv(prefix + "_REGION")
	if region == "" {
		region = "auto"
	}
	return BucketConfig{Bucket: bucket, Endpoint: endpoint, AccessKeyID: ak, SecretAccessKey: sk, Region: region}, nil
}

func validateNoOverlaps(pairs []BucketPair) error {
	if len(pairs) == 0 {
		return fmt.Errorf("at least one bucket pair must be configured")
	}
	for i := range pairs {
		for j := range pairs {
			if BucketsOverlap(pairs[i].Source, pairs[j].Dest) {
				return fmt.Errorf("configuration error: source pairs[%d] '%s' (prefix=%q) at %s overlaps with destination pairs[%d] '%s' (prefix=%q) at %s; source and destination must be disjoint",
					i, pairs[i].Source.Bucket, pairs[i].Source.Prefix, pairs[i].Source.Endpoint,
					j, pairs[j].Dest.Bucket, pairs[j].Dest.Prefix, pairs[j].Dest.Endpoint)
			}
		}
	}
	return nil
}

// BucketsOverlap reports whether two buckets share endpoint + name and one
// prefix is a prefix of the other (the empty prefix subsumes everything).
func BucketsOverlap(a, b BucketConfig) bool {
	if normalizeEndpoint(a.Endpoint) != normalizeEndpoint(b.Endpoint) {
		return false
	}
	if a.Bucket != b.Bucket {
		return false
	}
	return strings.HasPrefix(a.Prefix, b.Prefix) || strings.HasPrefix(b.Prefix, a.Prefix)
}

func normalizeEndpoint(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return strings.ToLower(s)
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func parseLadder(raw string) ([]LadderRung, error) {
	if raw == "" {
		out := make([]LadderRung, len(DefaultLadder))
		copy(out, DefaultLadder)
		return out, nil
	}
	var rungs []LadderRung
	if err := json.Unmarshal([]byte(raw), &rungs); err != nil {
		return nil, fmt.Errorf("HLS_LADDER is not valid JSON: %w", err)
	}
	if len(rungs) == 0 {
		return nil, fmt.Errorf("HLS_LADDER must be a non-empty JSON array")
	}
	for _, r := range rungs {
		if r.Name == "" || r.Width == 0 || r.Height == 0 || r.VideoBitrateKbps == 0 || r.AudioBitrateKbps == 0 {
			return nil, fmt.Errorf("HLS_LADDER rung is malformed: %+v", r)
		}
	}
	return rungs, nil
}

func parseLogLevel(raw string) (LogLevel, error) {
	v := strings.ToLower(raw)
	if v == "" {
		return LevelInfo, nil
	}
	switch LogLevel(v) {
	case LevelDebug, LevelInfo, LevelWarn, LevelError:
		return LogLevel(v), nil
	}
	return "", fmt.Errorf("LOG_LEVEL must be debug|info|warn|error, got: %s", raw)
}

func parseNumber(name string, fallback float64) (float64, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be numeric, got: %s", name, raw)
	}
	return n, nil
}

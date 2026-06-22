package core

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Image perceptual-hashing pipeline. It shares all of the transcoder's
// scaffolding — the global dest-bucket lock, the source scanner, the per-source
// mapping cache, error tombstones, and the runtime budget — but the per-object
// work is just "download → PDQ hash → write mapping" instead of a transcode.
// The worker computes hashes only; all duplicate decisioning lives in the app
// that reads these mappings.

const (
	imageMappingPrefix = "image-mappings/"
	imageMappingSuffix = ".json"
)

// ImageMapping points a source image key at its PDQ perceptual hash. This is the
// client contract: the app reads image-mappings/<source-key>.json and uses
// pdqHash for near-duplicate detection. App-neutral by design — no consumer
// names or columns leak into the format.
type ImageMapping struct {
	SourceKey          string `json:"sourceKey"`
	SourceEtag         string `json:"sourceEtag"`
	SourceSize         int64  `json:"sourceSize"`
	SourceLastModified string `json:"sourceLastModified"`
	PDQHash            string `json:"pdqHash"`
	Quality            int    `json:"quality"`
	HashedAt           string `json:"hashedAt"`
	HasherVersion      string `json:"hasherVersion"`
}

// ImageMappingKey is the dest-bucket key for a source key's PDQ mapping.
func ImageMappingKey(sourceKey string) string {
	return imageMappingPrefix + sourceKey + imageMappingSuffix
}

// ReadImageMapping returns the PDQ mapping for a source key, or nil if none exists.
func ReadImageMapping(ctx context.Context, client *s3.Client, bucket, sourceKey string) (*ImageMapping, error) {
	return getJSONObject[ImageMapping](ctx, client, bucket, ImageMappingKey(sourceKey))
}

// WriteImageMapping writes (overwrites) a source key's PDQ mapping.
func WriteImageMapping(ctx context.Context, client *s3.Client, bucket string, m ImageMapping) error {
	return putJSONObject(ctx, client, bucket, ImageMappingKey(m.SourceKey), m)
}

// IsCachedImageMapping reports whether an existing mapping already covers the
// current source object (ETag + size match) — i.e. nothing to re-hash.
func IsCachedImageMapping(m *ImageMapping, etag string, size int64) bool {
	return m != nil && m.SourceEtag == etag && m.SourceSize == size
}

// RunImagesOnce processes all configured bucket pairs sequentially for image
// PDQ hashing, sharing the runtime budget. Mirrors RunOnce; each pair takes its
// own dest-bucket lock, and a lock collision skips that pair, not the rest.
func RunImagesOnce(ctx context.Context, opts OrchestratorOptions) RunSummary {
	cfg, logger := opts.Config, opts.Logger
	startedAt := time.Now()

	lockTTL := ComputeLockTTLSeconds(cfg.MaxRuntimeSeconds, cfg.LockTTLMultiplier)
	budget := ComputeBudgetSeconds(cfg.MaxRuntimeSeconds, cfg.BudgetMultiplier)
	budgetEndsAt := startedAt.Add(time.Duration(budget) * time.Second)

	var sum RunSummary
	sum.TotalPairs = len(cfg.Pairs)

	for i := range cfg.Pairs {
		pair := cfg.Pairs[i]
		if time.Now().After(budgetEndsAt) {
			logger.Info("budget exhausted before processing remaining pairs", Fields{"completedPairs": i, "totalPairs": len(cfg.Pairs)})
			break
		}
		sourceClient := NewS3Client(pair.Source)
		destClient := NewS3Client(pair.Dest)

		res, acquired := runImagePair(ctx, pairArgs{
			pair: pair, cfg: cfg, logger: logger,
			sourceClient: sourceClient, destClient: destClient,
			lockTTLSeconds: lockTTL, budgetEndsAt: budgetEndsAt, pairIndex: i,
		})
		sum.Processed += res.Processed
		sum.Cached += res.Cached
		sum.Errored += res.Errored
		sum.Failed += res.Failed
		if acquired {
			sum.PairsProcessed++
		}
	}

	sum.DurationMs = time.Since(startedAt).Milliseconds()
	logger.Info("image run complete", Fields{
		"processed": sum.Processed, "cached": sum.Cached,
		"errored": sum.Errored, "failed": sum.Failed,
		"durationMs": sum.DurationMs, "pairsProcessed": sum.PairsProcessed, "totalPairs": sum.TotalPairs,
	})
	return sum
}

func runImagePair(ctx context.Context, a pairArgs) (pairResult, bool) {
	var res pairResult
	lock, err := AcquireLock(ctx, AcquireOptions{
		Client: a.destClient, Bucket: a.pair.Dest.Bucket, Platform: a.cfg.Platform,
		MaxRuntimeSeconds: a.cfg.MaxRuntimeSeconds, LockTTLSeconds: a.lockTTLSeconds, Logger: a.logger,
	})
	if err != nil {
		res.Failed++
		a.logger.Error("failed to acquire pair lock", Fields{"pairIndex": a.pairIndex, "error": err.Error()})
		return res, false
	}
	if lock == nil {
		return res, false
	}
	defer lock.Release(ctx)

	a.logger.Info("starting image pair", Fields{
		"pairIndex": a.pairIndex, "sourceBucket": a.pair.Source.Bucket, "destBucket": a.pair.Dest.Bucket,
	})

	scanOpts := ScanOptions{Prefix: a.pair.Source.Prefix, Filter: IsImageKey}
	for source, err := range ScanSource(ctx, a.sourceClient, a.pair.Source.Bucket, scanOpts) {
		if err != nil {
			res.Failed++
			a.logger.Error("source scan failed", Fields{"pairIndex": a.pairIndex, "error": err.Error()})
			break
		}
		if time.Now().After(a.budgetEndsAt) {
			a.logger.Info("budget exhausted in pair", Fields{"pairIndex": a.pairIndex})
			break
		}

		outcome, perr := processImage(ctx, a, source)
		if perr != nil {
			res.Failed++
			a.logger.Error("image processing failed", Fields{"pairIndex": a.pairIndex, "sourceKey": source.Key, "error": perr.Error()})
			if a.cfg.ErrorTombstones {
				writeFailureTombstone(ctx, a, source, perr)
			}
			continue
		}
		switch outcome {
		case outcomeTranscoded:
			res.Processed++
		case outcomeCached:
			res.Cached++
		case outcomeTombstoned:
			res.Errored++
		}
	}

	return res, true
}

func processImage(ctx context.Context, a pairArgs, source SourceObject) (processOutcome, error) {
	dest := a.pair.Dest.Bucket

	// 1. Mapping cache check — already hashed at this exact version.
	existing, err := ReadImageMapping(ctx, a.destClient, dest, source.Key)
	if err != nil {
		return 0, err
	}
	if IsCachedImageMapping(existing, source.ETag, source.Size) {
		a.logger.Debug("image mapping cache hit", Fields{"sourceKey": source.Key})
		return outcomeCached, nil
	}

	// 2. Tombstone check — skip a deterministically-broken source version.
	if a.cfg.ErrorTombstones {
		tomb, err := ReadTombstone(ctx, a.destClient, dest, source.Key)
		if err != nil {
			return 0, err
		}
		if tomb.Blocks(source.ETag, source.Size, a.cfg.TombstoneMaxAttempts) {
			a.logger.Info("skipping tombstoned source", Fields{"sourceKey": source.Key, "attempts": tomb.Attempts})
			return outcomeTombstoned, nil
		}
	}

	// No per-image lease: PDQ hashing is cheap and the global pair lock already
	// serializes work. The lease exists for the video path to make raising
	// MAX_CONCURRENCY safe under expensive transcodes; it is not worth a tiny
	// object per image here.

	tempDir, err := os.MkdirTemp("", "imagehasher-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(tempDir)
	localSource := filepath.Join(tempDir, "source")

	// 3. Download (the SHA-256 from DownloadAndHash is unused here — exact-bytes
	// dedup is the app's job — but it streams to disk in one pass, which is what
	// the hasher needs).
	a.logger.Info("downloading image", Fields{"sourceKey": source.Key, "sizeBytes": source.Size})
	if _, err := DownloadAndHash(ctx, a.sourceClient, a.pair.Source.Bucket, source.Key, localSource); err != nil {
		return 0, err
	}

	// 4. PDQ hash.
	result, err := ComputePDQ(ctx, localSource)
	if err != nil {
		return 0, err
	}
	a.logger.Info("hashed image", Fields{"sourceKey": source.Key, "quality": result.Quality})

	// 5. Write mapping.
	m := ImageMapping{
		SourceKey: source.Key, SourceEtag: source.ETag, SourceSize: source.Size,
		SourceLastModified: source.LastModified.UTC().Format(time.RFC3339Nano),
		PDQHash:            result.Hash,
		Quality:            result.Quality,
		HashedAt:           nowISO(),
		HasherVersion:      Version,
	}
	if err := WriteImageMapping(ctx, a.destClient, dest, m); err != nil {
		return 0, err
	}

	clearTombstone(ctx, a, source.Key)
	return outcomeTranscoded, nil
}

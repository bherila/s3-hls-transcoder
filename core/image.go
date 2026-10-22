package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
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
	SourceKey string `json:"sourceKey"`
	// SourceBucket/SourceEndpoint record which pair wrote the mapping so that
	// cleanup never treats another pair's mapping as one of its own orphans.
	SourceBucket       string `json:"sourceBucket"`
	SourceEndpoint     string `json:"sourceEndpoint"`
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

func sourceKeyFromImageMappingKey(key string) (string, bool) {
	if !strings.HasPrefix(key, imageMappingPrefix) || !strings.HasSuffix(key, imageMappingSuffix) {
		return "", false
	}
	return key[len(imageMappingPrefix) : len(key)-len(imageMappingSuffix)], true
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

// RunImagesOnce runs one fair, globally serial sweep of image bucket pairs.
func RunImagesOnce(ctx context.Context, opts OrchestratorOptions) RunSummary {
	next := 0
	return runScheduled(ctx, opts, &next, processImage, true)
}

// RunImageCleanupPass deletes image-mapping objects whose source image no longer
// exists in the source bucket. Only mappings tagged with this pair's source
// bucket and endpoint are candidates, so one pair's cleanup cannot delete
// another pair's mappings when several pairs share a destination bucket.
// Unlike the video cleanup it needs no refcounting:
// each mapping is a row-private hash with no shared output tree to GC.
func RunImageCleanupPass(ctx context.Context, opts CleanupOptions) (CleanupResult, error) {
	opts.Logger.Info("image cleanup: enumerating live source keys", Fields{"sourceBucket": opts.SourceBucket})
	liveSources := map[string]bool{}
	for obj, err := range ScanSource(ctx, opts.SourceClient, opts.SourceBucket, ScanOptions{
		Prefix: opts.SourcePrefix, Filter: func(string) bool { return true },
	}) {
		if err != nil {
			return CleanupResult{}, err
		}
		liveSources[obj.Key] = true
	}

	var res CleanupResult
	var token *string
	for {
		out, err := opts.DestClient.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket: aws.String(opts.DestBucket), Prefix: aws.String(imageMappingPrefix), ContinuationToken: token,
		})
		if err != nil {
			return res, err
		}
		for _, o := range out.Contents {
			if o.Key == nil || !strings.HasSuffix(*o.Key, imageMappingSuffix) {
				continue
			}
			sourceKey, ok := sourceKeyFromImageMappingKey(*o.Key)
			if !ok {
				continue
			}
			if opts.SourcePrefix != "" && !strings.HasPrefix(sourceKey, opts.SourcePrefix) {
				continue
			}
			if liveSources[sourceKey] {
				continue
			}
			m, err := ReadImageMapping(ctx, opts.DestClient, opts.DestBucket, sourceKey)
			if err != nil {
				return res, err
			}
			if m == nil {
				continue
			}
			if m.SourceBucket != opts.SourceBucket || m.SourceEndpoint != opts.SourceEndpoint {
				continue
			}
			res.OrphanMappingsFound++
			if opts.DryRun {
				res.OrphanMappingsDeleted++
				continue
			}
			if _, err := opts.DestClient.DeleteObject(ctx, &s3.DeleteObjectInput{
				Bucket: aws.String(opts.DestBucket), Key: aws.String(*o.Key),
			}); err != nil {
				return res, err
			}
			res.OrphanMappingsDeleted++
		}
		if out.IsTruncated != nil && *out.IsTruncated && out.NextContinuationToken != nil {
			token = out.NextContinuationToken
		} else {
			break
		}
	}

	opts.Logger.Info("image cleanup pass complete", Fields{
		"orphanMappingsFound": res.OrphanMappingsFound, "orphanMappingsDeleted": res.OrphanMappingsDeleted, "dryRun": opts.DryRun,
	})
	return res, nil
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
		SourceKey: source.Key, SourceBucket: a.pair.Source.Bucket, SourceEndpoint: a.pair.Source.Endpoint,
		SourceEtag: source.ETag, SourceSize: source.Size,
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

package core

import (
	"context"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// CleanupOptions configures the orphan-GC pass.
type CleanupOptions struct {
	SourceClient *s3.Client
	DestClient   *s3.Client
	SourceBucket string
	DestBucket   string
	SourcePrefix string
	Logger       *Logger
	DryRun       bool
}

// CleanupResult summarizes a cleanup pass.
type CleanupResult struct {
	OrphanMappingsFound   int
	OrphanMappingsDeleted int
	ContentIDsGCd         int
	ContentIDsRetained    int
	ObjectsDeleted        int
}

type orphanMapping struct {
	sourceKey string
	contentID string
}

// RunCleanupPass removes transcoded output for sources deleted from the source
// bucket. Refcount-aware: a content ID's by-id/ tree is GC'd only when all
// mappings pointing at it are orphans. Orphan mapping objects are always deleted.
func RunCleanupPass(ctx context.Context, opts CleanupOptions) (CleanupResult, error) {
	opts.Logger.Info("cleanup: enumerating live source keys", Fields{"sourceBucket": opts.SourceBucket})
	liveSources := map[string]bool{}
	for obj, err := range ScanSource(ctx, opts.SourceClient, opts.SourceBucket, ScanOptions{
		Prefix: opts.SourcePrefix, Filter: func(string) bool { return true },
	}) {
		if err != nil {
			return CleanupResult{}, err
		}
		liveSources[obj.Key] = true
	}
	opts.Logger.Info("cleanup: live source count", Fields{"count": len(liveSources)})

	orphans, err := findOrphanMappings(ctx, opts, liveSources)
	if err != nil {
		return CleanupResult{}, err
	}
	if len(orphans) == 0 {
		opts.Logger.Info("cleanup: no orphan mappings", nil)
		return CleanupResult{}, nil
	}
	opts.Logger.Info("cleanup: found orphan mappings", Fields{"count": len(orphans), "dryRun": opts.DryRun})

	byContentID := map[string][]string{}
	for _, o := range orphans {
		byContentID[o.contentID] = append(byContentID[o.contentID], o.sourceKey)
	}

	res := CleanupResult{OrphanMappingsFound: len(orphans)}
	for contentID, orphanSourceKeys := range byContentID {
		allMappings, err := FindMappingsForContentID(ctx, opts.DestClient, opts.DestBucket, contentID)
		if err != nil {
			return res, err
		}
		orphanSet := map[string]bool{}
		for _, k := range orphanSourceKeys {
			orphanSet[k] = true
		}
		liveCount := 0
		for _, k := range allMappings {
			if !orphanSet[k] {
				liveCount++
			}
		}

		if liveCount == 0 {
			opts.Logger.Info("cleanup: contentId fully orphaned; gc-ing transcoded output", Fields{"contentId": contentID, "orphanMappings": len(orphanSourceKeys), "dryRun": opts.DryRun})
			res.ContentIDsGCd++
			if !opts.DryRun {
				n, err := DeleteByIDDirectory(ctx, opts.DestClient, opts.DestBucket, contentID)
				if err != nil {
					return res, err
				}
				res.ObjectsDeleted += n
				if err := DeleteFingerprint(ctx, opts.DestClient, opts.DestBucket, contentID); err != nil {
					return res, err
				}
				if err := RemoveIndexEntry(ctx, opts.DestClient, opts.DestBucket, contentID); err != nil {
					return res, err
				}
			}
		} else {
			opts.Logger.Info("cleanup: contentId still has live references; retaining", Fields{"contentId": contentID, "liveMappings": liveCount, "dryRun": opts.DryRun})
			res.ContentIDsRetained++
		}

		if opts.DryRun {
			res.OrphanMappingsDeleted += len(orphanSourceKeys)
		} else {
			for _, sk := range orphanSourceKeys {
				if _, err := opts.DestClient.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(opts.DestBucket), Key: aws.String(MappingKey(sk))}); err != nil {
					return res, err
				}
				res.OrphanMappingsDeleted++
			}
		}
	}

	opts.Logger.Info("cleanup pass complete", Fields{"orphanMappingsFound": res.OrphanMappingsFound, "orphanMappingsDeleted": res.OrphanMappingsDeleted, "contentIdsGcd": res.ContentIDsGCd, "contentIdsRetained": res.ContentIDsRetained, "objectsDeleted": res.ObjectsDeleted, "dryRun": opts.DryRun})
	return res, nil
}

func findOrphanMappings(ctx context.Context, opts CleanupOptions, liveSources map[string]bool) ([]orphanMapping, error) {
	var orphans []orphanMapping
	var token *string
	for {
		out, err := opts.DestClient.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket: aws.String(opts.DestBucket), Prefix: aws.String(mappingPrefix), ContinuationToken: token,
		})
		if err != nil {
			return nil, err
		}
		for _, o := range out.Contents {
			if o.Key == nil || !strings.HasSuffix(*o.Key, mappingSuffix) {
				continue
			}
			sourceKey, ok := sourceKeyFromMappingKey(*o.Key)
			if !ok {
				continue
			}
			if opts.SourcePrefix != "" && !strings.HasPrefix(sourceKey, opts.SourcePrefix) {
				continue
			}
			if liveSources[sourceKey] {
				continue
			}
			m, err := ReadMapping(ctx, opts.DestClient, opts.DestBucket, sourceKey)
			if err != nil {
				return nil, err
			}
			if m == nil {
				continue
			}
			orphans = append(orphans, orphanMapping{sourceKey: sourceKey, contentID: m.ContentID})
		}
		if out.IsTruncated != nil && *out.IsTruncated && out.NextContinuationToken != nil {
			token = out.NextContinuationToken
		} else {
			return orphans, nil
		}
	}
}

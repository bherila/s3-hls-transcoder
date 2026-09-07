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
	// SourceEndpoint identifies, together with SourceBucket, the pair whose
	// mappings this pass may treat as orphans.
	SourceEndpoint string
	DestBucket     string
	SourcePrefix   string
	Logger         *Logger
	DryRun         bool
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
//
// Only mappings tagged with this pair's source bucket and endpoint (plus
// SourcePrefix, if set) are candidates, so one pair's cleanup cannot stomp on
// another pair's mappings if multiple pairs share a dest bucket.
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
		// The reverse index answers "who still points at this content?" in one
		// GET; only content written before the index existed falls back to the
		// full mappings/ scan (and is backfilled by ListRefs).
		refs, err := ListRefs(ctx, opts.DestClient, opts.DestBucket, contentID, !opts.DryRun)
		if err != nil {
			return res, err
		}
		orphanSet := map[string]bool{}
		for _, k := range orphanSourceKeys {
			orphanSet[k] = true
		}
		liveKeys, err := verifyLiveRefs(ctx, opts, contentID, refs, orphanSet)
		if err != nil {
			return res, err
		}
		liveCount := len(liveKeys)

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
				// The reverse index lives outside by-id/, so deleting the
				// content tree no longer disposes of it.
				if err := DeleteRefs(ctx, opts.DestClient, opts.DestBucket, contentID); err != nil {
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
			continue
		}
		for _, sk := range orphanSourceKeys {
			if _, err := opts.DestClient.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(opts.DestBucket), Key: aws.String(MappingKey(sk))}); err != nil {
				return res, err
			}
			res.OrphanMappingsDeleted++
		}
		// Retained content keeps its index; rewrite it without the references
		// just deleted (and any that verification found stale). GC-d content
		// had its index deleted above.
		if liveCount > 0 && len(liveKeys) != len(refs) {
			if err := WriteRefs(ctx, opts.DestClient, opts.DestBucket, contentID, liveKeys); err != nil {
				return res, err
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
			if m.SourceBucket != opts.SourceBucket || m.SourceEndpoint != opts.SourceEndpoint {
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

// verifyLiveRefs returns the reverse-index entries that still hold: a source
// key not in orphanSet whose mapping object exists and still points at
// contentID. The index is deliberately allowed to over-report (a reference is
// added before its mapping is written and removed after it is deleted), so
// every retention decision is confirmed against the named mapping — one GET per
// candidate, against the whole-bucket scan this replaces.
func verifyLiveRefs(ctx context.Context, opts CleanupOptions, contentID string, refs []string, orphanSet map[string]bool) ([]string, error) {
	var live []string
	for _, sk := range refs {
		if orphanSet[sk] {
			continue
		}
		m, err := ReadMapping(ctx, opts.DestClient, opts.DestBucket, sk)
		if err != nil {
			return nil, err
		}
		if m == nil || m.ContentID != contentID {
			opts.Logger.Debug("cleanup: pruning stale reverse-index entry", Fields{"contentId": contentID, "sourceKey": sk})
			continue
		}
		live = append(live, sk)
	}
	return live, nil
}

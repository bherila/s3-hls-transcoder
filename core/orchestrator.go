package core

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// OrchestratorOptions configures a run.
type OrchestratorOptions struct {
	Config *Config
	Logger *Logger
}

// RunSummary is the per-invocation outcome.
type RunSummary struct {
	Processed      int   `json:"processed"`
	Cached         int   `json:"cached"`
	Deduped        int   `json:"deduped"`
	Busy           int   `json:"busy"`
	Errored        int   `json:"errored"` // skipped due to a blocking tombstone
	Failed         int   `json:"failed"`  // failed this run (tombstone written/updated)
	DurationMs     int64 `json:"durationMs"`
	PairsProcessed int   `json:"pairsProcessed"`
	TotalPairs     int   `json:"totalPairs"`
}

type processOutcome int

const (
	outcomeTranscoded processOutcome = iota
	outcomeDeduped
	outcomeCached
	outcomeLeaseBusy
	outcomeTombstoned
)

// RunOnce processes all configured bucket pairs sequentially, sharing the
// runtime budget. Each pair takes its own dest-bucket lock; a lock collision
// skips that pair, not the rest.
func RunOnce(ctx context.Context, opts OrchestratorOptions) RunSummary {
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

		res, acquired := runPair(ctx, pairArgs{
			pair: pair, cfg: cfg, logger: logger,
			sourceClient: sourceClient, destClient: destClient,
			lockTTLSeconds: lockTTL, budgetEndsAt: budgetEndsAt, pairIndex: i,
		})
		sum.Processed += res.Processed
		sum.Cached += res.Cached
		sum.Deduped += res.Deduped
		sum.Busy += res.Busy
		sum.Errored += res.Errored
		sum.Failed += res.Failed
		if acquired {
			sum.PairsProcessed++
		}
	}

	sum.DurationMs = time.Since(startedAt).Milliseconds()
	logger.Info("run complete", Fields{
		"processed": sum.Processed, "cached": sum.Cached, "deduped": sum.Deduped,
		"busy": sum.Busy, "errored": sum.Errored, "failed": sum.Failed,
		"durationMs": sum.DurationMs, "pairsProcessed": sum.PairsProcessed, "totalPairs": sum.TotalPairs,
	})
	return sum
}

type pairArgs struct {
	pair           BucketPair
	cfg            *Config
	logger         *Logger
	sourceClient   *s3.Client
	destClient     *s3.Client
	lockTTLSeconds float64
	budgetEndsAt   time.Time
	pairIndex      int
}

type pairResult struct {
	Processed, Cached, Deduped, Busy, Errored, Failed int
}

func runPair(ctx context.Context, a pairArgs) (pairResult, bool) {
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

	a.logger.Info("starting pair", Fields{
		"pairIndex": a.pairIndex, "sourceBucket": a.pair.Source.Bucket, "destBucket": a.pair.Dest.Bucket,
	})

	scanOpts := ScanOptions{Prefix: a.pair.Source.Prefix}
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

		outcome, perr := processSource(ctx, a, source)
		if perr != nil {
			res.Failed++
			a.logger.Error("source processing failed", Fields{"pairIndex": a.pairIndex, "sourceKey": source.Key, "error": perr.Error()})
			if a.cfg.ErrorTombstones {
				writeFailureTombstone(ctx, a, source, perr)
			}
			continue
		}
		switch outcome {
		case outcomeTranscoded:
			res.Processed++
		case outcomeDeduped:
			res.Deduped++
		case outcomeCached:
			res.Cached++
		case outcomeLeaseBusy:
			res.Busy++
		case outcomeTombstoned:
			res.Errored++
		}
	}

	if a.cfg.CleanupDeletedSources && time.Now().Before(a.budgetEndsAt) {
		if _, err := RunCleanupPass(ctx, CleanupOptions{
			SourceClient: a.sourceClient, DestClient: a.destClient,
			SourceBucket: a.pair.Source.Bucket, DestBucket: a.pair.Dest.Bucket,
			SourcePrefix: a.pair.Source.Prefix, Logger: a.logger, DryRun: a.cfg.CleanupDryRun,
		}); err != nil {
			a.logger.Error("cleanup pass failed", Fields{"pairIndex": a.pairIndex, "error": err.Error()})
		}
	}
	return res, true
}

func processSource(ctx context.Context, a pairArgs, source SourceObject) (processOutcome, error) {
	dest := a.pair.Dest.Bucket

	// 1. Mapping cache check.
	existing, err := ReadMapping(ctx, a.destClient, dest, source.Key)
	if err != nil {
		return 0, err
	}
	if IsCachedMapping(existing, source.ETag, source.Size) {
		a.logger.Debug("mapping cache hit", Fields{"sourceKey": source.Key})
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

	tempDir, err := os.MkdirTemp("", "transcoder-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(tempDir)
	localSource := filepath.Join(tempDir, "source")

	// 3. Download + hash.
	a.logger.Info("downloading", Fields{"sourceKey": source.Key, "sizeBytes": source.Size})
	dl, err := DownloadAndHash(ctx, a.sourceClient, a.pair.Source.Bucket, source.Key, localSource)
	if err != nil {
		return 0, err
	}
	if dl.Bytes != source.Size {
		a.logger.Warn("downloaded size differs from listing", Fields{"sourceKey": source.Key, "listed": source.Size, "downloaded": dl.Bytes})
	}
	contentID := FormatContentID(SchemeSHA256, dl.SHA256)

	// 4. Byte-hash dedup.
	exists, err := TranscodedOutputExists(ctx, a.destClient, dest, contentID)
	if err != nil {
		return 0, err
	}
	if exists {
		a.logger.Info("byte-hash dedup hit", Fields{"sourceKey": source.Key, "contentId": contentID})
		if err := WriteMapping(ctx, a.destClient, dest, buildMapping(source, contentID)); err != nil {
			return 0, err
		}
		clearTombstone(ctx, a, source.Key)
		return outcomeDeduped, nil
	}

	// 5. Per-video lease.
	lease, err := AcquireLease(ctx, AcquireOptions{
		Client: a.destClient, Bucket: dest, Platform: a.cfg.Platform,
		MaxRuntimeSeconds: a.cfg.MaxRuntimeSeconds, LockTTLSeconds: a.lockTTLSeconds, Logger: a.logger,
	}, contentID)
	if err != nil {
		return 0, err
	}
	if lease == nil {
		a.logger.Info("per-video lease busy; skipping", Fields{"sourceKey": source.Key, "contentId": contentID})
		return outcomeLeaseBusy, nil
	}
	defer lease.Release(ctx)

	// 6. Probe.
	probe, err := ProbeSource(ctx, localSource)
	if err != nil {
		return 0, err
	}
	a.logger.Info("probed source", Fields{"sourceKey": source.Key, "width": probe.Width, "height": probe.Height, "hasAudio": probe.HasAudio})

	// 7. Effective ladder.
	ladder := computeEffectiveLadder(a.cfg.Ladder, probe.Width, probe.Height)

	// 8. Fingerprint.
	fp, err := FingerprintVideo(ctx, localSource, 2)
	if err != nil {
		return 0, err
	}

	// 9. Perceptual match.
	match, err := FindPerceptualMatch(ctx, a.destClient, dest, fp, a.cfg.PerceptualThreshold)
	if err != nil {
		return 0, err
	}
	var pendingRepointFrom string
	if match != nil {
		incomingHigher := isHigherQuality(probe, match.Entry)
		a.logger.Info("perceptual match", Fields{"sourceKey": source.Key, "matchedContentId": match.ContentID, "similarity": match.Similarity, "incomingHigherQuality": incomingHigher, "dryRun": a.cfg.PerceptualDryRun})
		if !a.cfg.PerceptualDryRun {
			if !incomingHigher {
				if err := WriteMapping(ctx, a.destClient, dest, buildMapping(source, match.ContentID)); err != nil {
					return 0, err
				}
				clearTombstone(ctx, a, source.Key)
				return outcomeDeduped, nil
			}
			pendingRepointFrom = match.ContentID
		}
	}

	// 10. Transcode.
	outputDir := filepath.Join(tempDir, "hls")
	a.logger.Info("transcoding", Fields{"sourceKey": source.Key, "rungs": rungNames(ladder)})
	if err := TranscodeToHLS(ctx, TranscodeOptions{Input: localSource, OutputDir: outputDir, Ladder: ladder, HasAudio: probe.HasAudio}); err != nil {
		return 0, err
	}

	// 11. Upload HLS tree.
	if _, err := UploadDirectory(ctx, a.destClient, dest, ByIDPrefix(contentID), outputDir, 8); err != nil {
		return 0, err
	}

	// 12. Fingerprint blob + index.
	if err := UploadFingerprint(ctx, a.destClient, dest, contentID, SerializeFingerprint(fp)); err != nil {
		return 0, err
	}
	entry := FingerprintIndexEntry{ContentID: contentID, IntervalSeconds: fp.IntervalSeconds, HashCount: len(fp.Hashes), Width: probe.Width, Height: probe.Height, EncodedAt: nowISO(), VideoBitrateKbps: probe.BitrateKbps}
	if err := UpsertIndexEntry(ctx, a.destClient, dest, entry); err != nil {
		return 0, err
	}

	// 13. Metadata + mapping.
	md := OutputMetadata{ContentID: contentID, EncoderVersion: Version, EncodedAt: nowISO(), Source: MetadataSource{Width: probe.Width, Height: probe.Height, DurationSeconds: probe.DurationSeconds, BitrateKbps: probe.BitrateKbps}, Ladder: ladder}
	if err := WriteMetadata(ctx, a.destClient, dest, md); err != nil {
		return 0, err
	}
	if err := WriteMapping(ctx, a.destClient, dest, buildMapping(source, contentID)); err != nil {
		return 0, err
	}
	a.logger.Info("transcode complete", Fields{"sourceKey": source.Key, "contentId": contentID})

	// 14. Repoint + GC on quality upgrade.
	if pendingRepointFrom != "" {
		if err := repointAndGC(ctx, a, pendingRepointFrom, contentID); err != nil {
			return 0, err
		}
	}

	clearTombstone(ctx, a, source.Key)
	return outcomeTranscoded, nil
}

func writeFailureTombstone(ctx context.Context, a pairArgs, source SourceObject, cause error) {
	attempts := 1
	if prev, err := ReadTombstone(ctx, a.destClient, a.pair.Dest.Bucket, source.Key); err == nil && prev != nil &&
		prev.SourceEtag == source.ETag && prev.SourceSize == source.Size {
		attempts = prev.Attempts + 1
	}
	t := ErrorTombstone{
		SourceKey: source.Key, SourceEtag: source.ETag, SourceSize: source.Size,
		SourceLastModified: source.LastModified.UTC().Format(time.RFC3339Nano),
		Error:              cause.Error(), FailedAt: nowISO(), Attempts: attempts, EncoderVersion: Version,
	}
	if err := WriteTombstone(ctx, a.destClient, a.pair.Dest.Bucket, t); err != nil {
		a.logger.Warn("failed to write error tombstone", Fields{"sourceKey": source.Key, "error": err.Error()})
	}
}

func clearTombstone(ctx context.Context, a pairArgs, sourceKey string) {
	if !a.cfg.ErrorTombstones {
		return
	}
	if err := DeleteTombstone(ctx, a.destClient, a.pair.Dest.Bucket, sourceKey); err != nil {
		a.logger.Warn("failed to clear error tombstone", Fields{"sourceKey": sourceKey, "error": err.Error()})
	}
}

func buildMapping(source SourceObject, contentID string) SourceMapping {
	return SourceMapping{
		SourceKey: source.Key, SourceEtag: source.ETag, SourceSize: source.Size,
		SourceLastModified: source.LastModified.UTC().Format(time.RFC3339Nano),
		ContentID:          contentID, HLSRoot: MasterPlaylistKey(contentID),
		EncodedAt: nowISO(), EncoderVersion: Version,
	}
}

func computeEffectiveLadder(full []LadderRung, w, h int) []LadderRung {
	var filtered []LadderRung
	for _, r := range full {
		if r.Width <= w && r.Height <= h {
			filtered = append(filtered, r)
		}
	}
	if len(filtered) > 0 {
		return filtered
	}
	return []LadderRung{full[0]}
}

func isHigherQuality(probe *ProbeResult, stored FingerprintIndexEntry) bool {
	in := probe.Width * probe.Height
	st := stored.Width * stored.Height
	if in != st {
		return in > st
	}
	if probe.BitrateKbps != nil && stored.VideoBitrateKbps != nil {
		return *probe.BitrateKbps > *stored.VideoBitrateKbps
	}
	return false
}

func repointAndGC(ctx context.Context, a pairArgs, oldContentID, newContentID string) error {
	dest := a.pair.Dest.Bucket
	sourceKeys, err := FindMappingsForContentID(ctx, a.destClient, dest, oldContentID)
	if err != nil {
		return err
	}
	a.logger.Info("repointing mappings to new transcoded output", Fields{"oldContentId": oldContentID, "newContentId": newContentID, "mappingCount": len(sourceKeys)})
	for _, sk := range sourceKeys {
		old, err := ReadMapping(ctx, a.destClient, dest, sk)
		if err != nil {
			return err
		}
		if old == nil {
			continue
		}
		old.ContentID = newContentID
		old.HLSRoot = MasterPlaylistKey(newContentID)
		old.EncoderVersion = Version
		if err := WriteMapping(ctx, a.destClient, dest, *old); err != nil {
			return err
		}
	}
	if _, err := DeleteByIDDirectory(ctx, a.destClient, dest, oldContentID); err != nil {
		return err
	}
	if err := DeleteFingerprint(ctx, a.destClient, dest, oldContentID); err != nil {
		return err
	}
	return RemoveIndexEntry(ctx, a.destClient, dest, oldContentID)
}

func rungNames(ladder []LadderRung) []string {
	names := make([]string, len(ladder))
	for i, r := range ladder {
		names[i] = r.Name
	}
	return names
}

func nowISO() string { return time.Now().UTC().Format(time.RFC3339Nano) }

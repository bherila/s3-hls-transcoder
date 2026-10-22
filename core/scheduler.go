package core

import (
	"context"
	"fmt"
	"iter"
	"sync"
	"time"
)

type sourceProcessor func(context.Context, pairArgs, SourceObject) (processOutcome, error)

type scheduledPair struct {
	args pairArgs
	next func() (SourceObject, error, bool)
	stop func()
	lock *LockHandle
	done bool
}

// NewTranscoderRunner retains the next tenant between sweeps. Calls on one
// runner are serialized: there is at most one active source job across tenants.
func NewTranscoderRunner(opts OrchestratorOptions) RunFunc {
	return newRunner(opts, processSource, false)
}

// NewImageRunner provides the same scheduler for the image worker.
func NewImageRunner(opts OrchestratorOptions) RunFunc {
	return newRunner(opts, processImage, true)
}

func newRunner(opts OrchestratorOptions, process sourceProcessor, images bool) RunFunc {
	var mu sync.Mutex
	next := 0
	return func(ctx context.Context) RunSummary {
		mu.Lock()
		defer mu.Unlock()
		return runScheduled(ctx, opts, &next, process, images)
	}
}

// runScheduled pulls one source per tenant per round. Iterators retain only a
// listing page, not the entire bucket. Locks remain held while a pair's scan is
// suspended, preserving the existing bucket-global index/cleanup invariants.
func runScheduled(parent context.Context, opts OrchestratorOptions, nextPair *int, process sourceProcessor, images bool) RunSummary {
	cfg, logger := opts.Config, opts.Logger
	startedAt := time.Now()
	budget := ComputeBudgetSeconds(cfg.MaxRuntimeSeconds, cfg.BudgetMultiplier)
	endsAt := startedAt.Add(time.Duration(budget * float64(time.Second)))
	ctx, cancel := context.WithDeadline(parent, endsAt)
	defer cancel()
	sum := RunSummary{TotalPairs: len(cfg.Pairs)}
	pairs := make([]scheduledPair, len(cfg.Pairs))
	// Cleanup must have a live context even after the shared deadline or SIGTERM.
	defer func() {
		for i := range pairs {
			closeScheduledPair(&pairs[i])
		}
	}()
	start := 0
	if len(pairs) > 0 {
		start = *nextPair % len(pairs)
	}
	remaining := len(pairs)
	for remaining > 0 && ctx.Err() == nil {
		for offset := range pairs {
			if ctx.Err() != nil {
				break
			}
			i := (start + offset) % len(pairs)
			p := &pairs[i]
			if p.done {
				continue
			}
			// Include lock acquisition and listing in the turn: an unavailable
			// tenant must not monopolize every sweep before reaching process.
			*nextPair = (i + 1) % len(pairs)
			if p.next == nil {
				pair := cfg.Pairs[i]
				name := pair.Name
				if name == "" {
					name = fmt.Sprintf("pair-%d", i)
				}
				p.args = pairArgs{
					pair: pair, cfg: cfg,
					logger:       logger.WithFields(Fields{"tenant": name, "pairIndex": i}),
					sourceClient: NewS3Client(pair.Source), destClient: NewS3Client(pair.Dest),
					lockTTLSeconds: ComputeLockTTLSeconds(cfg.MaxRuntimeSeconds, cfg.LockTTLMultiplier),
				}
				lock, err := AcquireLock(ctx, AcquireOptions{
					Client: p.args.destClient, Bucket: pair.Dest.Bucket, Platform: cfg.Platform,
					MaxRuntimeSeconds: cfg.MaxRuntimeSeconds, LockTTLSeconds: p.args.lockTTLSeconds, Logger: p.args.logger,
				})
				if err != nil || lock == nil {
					if err != nil && ctx.Err() == nil {
						sum.Failed++
						p.args.logger.Error("failed to acquire pair lock", Fields{"error": err.Error()})
					}
					p.done = true
					remaining--
					continue
				}
				p.lock = lock
				sum.PairsProcessed++
				p.args.logger.Info("starting pair", Fields{"sourceBucket": pair.Source.Bucket, "destBucket": pair.Dest.Bucket})
				scan := ScanOptions{Prefix: pair.Source.Prefix}
				if images {
					scan.Filter = IsImageKey
				}
				p.next, p.stop = iter.Pull2(ScanSource(ctx, p.args.sourceClient, pair.Source.Bucket, scan))
			}
			source, err, ok := p.next()
			if ctx.Err() != nil {
				break
			}
			if !ok || err != nil {
				if err != nil {
					sum.Failed++
					p.args.logger.Error("source scan failed", Fields{"error": err.Error()})
				} else if cfg.CleanupDeletedSources {
					cleanup := CleanupOptions{
						SourceClient: p.args.sourceClient, DestClient: p.args.destClient,
						SourceBucket: p.args.pair.Source.Bucket, SourceEndpoint: p.args.pair.Source.Endpoint,
						DestBucket: p.args.pair.Dest.Bucket, SourcePrefix: p.args.pair.Source.Prefix,
						Logger: p.args.logger, DryRun: cfg.CleanupDryRun,
					}
					var cleanupErr error
					if images {
						_, cleanupErr = RunImageCleanupPass(ctx, cleanup)
					} else {
						_, cleanupErr = RunCleanupPass(ctx, cleanup)
					}
					if cleanupErr != nil && ctx.Err() == nil {
						sum.Failed++
						p.args.logger.Error("cleanup pass failed", Fields{"error": cleanupErr.Error()})
					}
				}
				closeScheduledPair(p)
				p.done = true
				remaining--
				continue
			}
			outcome, err := process(ctx, p.args, source)
			if ctx.Err() != nil {
				break
			} // interruption is not a bad source
			if err != nil {
				sum.Failed++
				p.args.logger.Error("source processing failed", Fields{"sourceKey": source.Key, "error": err.Error()})
				if cfg.ErrorTombstones {
					writeFailureTombstone(ctx, p.args, source, err)
				}
				continue
			}
			switch outcome {
			case outcomeTranscoded:
				sum.Processed++
			case outcomeDeduped:
				sum.Deduped++
			case outcomeCached:
				sum.Cached++
			case outcomeLeaseBusy:
				sum.Busy++
			case outcomeTombstoned:
				sum.Errored++
			}
		}
	}
	sum.DurationMs = time.Since(startedAt).Milliseconds()
	logger.Info("run complete", Fields{
		"processed": sum.Processed, "cached": sum.Cached, "deduped": sum.Deduped,
		"busy": sum.Busy, "errored": sum.Errored, "failed": sum.Failed,
		"durationMs": sum.DurationMs, "pairsProcessed": sum.PairsProcessed, "totalPairs": sum.TotalPairs,
		"interrupted": ctx.Err() != nil,
	})
	return sum
}

func closeScheduledPair(p *scheduledPair) {
	if p.stop != nil {
		p.stop()
		p.stop = nil
	}
	if p.lock != nil {
		releaseAfterRun(p.lock)
		p.lock = nil
	}
}

func releaseAfterRun(lock *LockHandle) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lock.Release(ctx)
}

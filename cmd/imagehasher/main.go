// Command imagehasher computes PDQ perceptual hashes for images in a source
// bucket and writes them to a results bucket for an app to read. It is the image
// sibling of the transcoder: same configuration, locking, scanning, caching, and
// trigger modes (see core.Serve), but the per-object work is a PDQ hash instead
// of a transcode. It shells out to the C++ pdq-photo-hasher tool, exactly as the
// transcoder shells out to ffmpeg.
//
// Trigger modes (selected by environment):
//
//   - one-shot (cron): no REDIS_URL and no POLL_FALLBACK_SECONDS — run a single
//     pass and exit (non-zero if any source failed).
//   - poll: POLL_FALLBACK_SECONDS set, no REDIS_URL — run a pass, sleep, repeat.
//   - wake: REDIS_URL set — run a pass, then BLPOP PDQ_QUEUE with
//     POLL_FALLBACK_SECONDS as a safety-net timeout, so a pushed request is
//     hashed within seconds while the timeout still guarantees a periodic sweep.
//
// Point SOURCE_* at the image bucket and DEST_* at the results bucket. The app
// reads image-mappings/<source-key>.json and uses its pdqHash field.
package main

import (
	"context"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/bherila/s3-hls-transcoder/core"
)

func main() {
	cfg, err := core.LoadConfig(core.PlatformLocal)
	if err != nil {
		os.Stderr.WriteString("config error: " + err.Error() + "\n")
		os.Exit(1)
	}
	logger := core.NewLogger(cfg.LogLevel)
	logger.Info("imagehasher starting", core.Fields{"platform": "local", "version": core.Version, "pairs": len(cfg.Pairs)})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opts := core.ServeOptions{
		Logger:          logger,
		RedisURL:        os.Getenv("REDIS_URL"),
		Queue:           getenv("PDQ_QUEUE", "pdq:requests"),
		FallbackSeconds: envInt("POLL_FALLBACK_SECONDS", 0),
		Run: func(ctx context.Context) core.RunSummary {
			return core.RunImagesOnce(ctx, core.OrchestratorOptions{Config: cfg, Logger: logger})
		},
	}

	sum, err := core.Serve(ctx, opts)
	if err != nil {
		logger.Error("serve error", core.Fields{"error": err.Error()})
		os.Exit(1)
	}
	if opts.IsOneShot() && sum.Failed > 0 {
		os.Exit(1)
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

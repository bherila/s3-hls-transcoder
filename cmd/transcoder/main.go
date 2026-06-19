// Command transcoder runs the HLS transcoder. It supports three modes,
// selected by environment:
//
//   - one-shot (cron): no REDIS_URL and no POLL_FALLBACK_SECONDS — run a single
//     pass and exit (non-zero if any source failed).
//   - poll: POLL_FALLBACK_SECONDS set, no REDIS_URL — run a pass, sleep, repeat.
//   - wake (recommended co-located with the app): REDIS_URL set — run a pass,
//     then BLPOP TRANSCODE_QUEUE with POLL_FALLBACK_SECONDS as a safety-net
//     timeout. The app LPUSHes on upload, so HLS appears within seconds, while
//     the timeout still guarantees a periodic sweep.
package main

import (
	"context"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/bherila/s3-hls-transcoder/core"
	"github.com/redis/go-redis/v9"
)

func main() {
	cfg, err := core.LoadConfig(core.PlatformLocal)
	if err != nil {
		// No logger yet (log level lives in config); fail loudly to stderr.
		os.Stderr.WriteString("config error: " + err.Error() + "\n")
		os.Exit(1)
	}
	logger := core.NewLogger(cfg.LogLevel)
	logger.Info("transcoder starting", core.Fields{"platform": "local", "version": core.Version, "pairs": len(cfg.Pairs)})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	redisURL := os.Getenv("REDIS_URL")
	fallback := envInt("POLL_FALLBACK_SECONDS", 0)

	// One-shot (cron) mode.
	if redisURL == "" && fallback == 0 {
		sum := core.RunOnce(ctx, core.OrchestratorOptions{Config: cfg, Logger: logger})
		if sum.Failed > 0 {
			os.Exit(1)
		}
		return
	}

	runDaemon(ctx, cfg, logger, redisURL, fallback)
}

func runDaemon(ctx context.Context, cfg *core.Config, logger *core.Logger, redisURL string, fallback int) {
	if fallback <= 0 {
		fallback = 900 // safety-net sweep cadence when running long-lived
	}
	queue := getenv("TRANSCODE_QUEUE", "transcode:requests")

	var rdb *redis.Client
	if redisURL != "" {
		opt, err := redis.ParseURL(redisURL)
		if err != nil {
			logger.Error("invalid REDIS_URL", core.Fields{"error": err.Error()})
			os.Exit(1)
		}
		rdb = redis.NewClient(opt)
		defer rdb.Close()
	}
	logger.Info("entering daemon mode", core.Fields{"wake": rdb != nil, "queue": queue, "fallbackSeconds": fallback})

	for ctx.Err() == nil {
		core.RunOnce(ctx, core.OrchestratorOptions{Config: cfg, Logger: logger})
		if ctx.Err() != nil {
			return
		}

		if rdb == nil {
			sleepCtx(ctx, time.Duration(fallback)*time.Second)
			continue
		}

		// Block until a wake request or the safety-net timeout. A single pass
		// drains all pending work, so we don't need to dequeue every request.
		_, err := rdb.BLPop(ctx, time.Duration(fallback)*time.Second, queue).Result()
		switch {
		case err == redis.Nil:
			// timeout → periodic sweep
		case err != nil:
			if ctx.Err() != nil {
				return
			}
			logger.Warn("redis BLPOP error; backing off", core.Fields{"error": err.Error()})
			sleepCtx(ctx, 5*time.Second)
		default:
			logger.Debug("woke on transcode request", nil)
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
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

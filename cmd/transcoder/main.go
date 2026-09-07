// Command transcoder runs the HLS transcoder. Its trigger mode is selected by
// environment (see core.Serve):
//
//   - one-shot (cron): no wake source and no POLL_FALLBACK_SECONDS — run a
//     single pass and exit (non-zero if any source failed).
//   - poll: POLL_FALLBACK_SECONDS set, no wake source — run a pass, sleep,
//     repeat.
//   - wake (recommended co-located with the app): REDIS_URL and/or
//     WAKE_HTTP_ADDR set — run a pass, then wait for a wake request with
//     POLL_FALLBACK_SECONDS as a safety-net timeout. The app LPUSHes
//     TRANSCODE_QUEUE, or a bucket event notification POSTs /wake, so HLS
//     appears within seconds while the timeout still guarantees a periodic
//     sweep.
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
		// No logger yet (log level lives in config); fail loudly to stderr.
		os.Stderr.WriteString("config error: " + err.Error() + "\n")
		os.Exit(1)
	}
	logger := core.NewLogger(cfg.LogLevel)
	logger.Info("transcoder starting", core.Fields{"platform": "local", "version": core.Version, "pairs": len(cfg.Pairs)})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opts := core.ServeOptions{
		Logger:          logger,
		RedisURL:        os.Getenv("REDIS_URL"),
		Queue:           getenv("TRANSCODE_QUEUE", "transcode:requests"),
		WakeHTTPAddr:    os.Getenv("WAKE_HTTP_ADDR"),
		WakeHTTPToken:   os.Getenv("WAKE_HTTP_TOKEN"),
		FallbackSeconds: envInt("POLL_FALLBACK_SECONDS", 0),
		Run: func(ctx context.Context) core.RunSummary {
			return core.RunOnce(ctx, core.OrchestratorOptions{Config: cfg, Logger: logger})
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

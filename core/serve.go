package core

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RunFunc executes one processing pass and returns its summary.
type RunFunc func(ctx context.Context) RunSummary

// ServeOptions configures the trigger loop shared by entrypoints (transcoder,
// imagehasher). Keeping the loop here guarantees every worker supports the same
// three modes identically, and that an environment can switch between them by
// config alone.
type ServeOptions struct {
	Logger          *Logger
	RedisURL        string // empty → no wake queue
	Queue           string // BLPOP key for wake mode; defaults to "transcode:requests"
	FallbackSeconds int    // poll/safety-net cadence; 0 with empty RedisURL → one-shot
	Run             RunFunc
}

// IsOneShot reports whether the options select a single cron pass (no wake
// queue and no poll interval).
func (o ServeOptions) IsOneShot() bool {
	return o.RedisURL == "" && o.FallbackSeconds == 0
}

// Serve runs the trigger mode selected by opts:
//
//   - one-shot (cron): RedisURL=="" and FallbackSeconds==0 — run a single pass
//     and return its summary (callers use it for the process exit code).
//   - poll: FallbackSeconds>0, RedisURL=="" — run a pass, sleep, repeat.
//   - wake: RedisURL set — run a pass, then BLPOP the queue with FallbackSeconds
//     as a safety-net timeout, so a pushed request is picked up within seconds
//     while the timeout still guarantees a periodic sweep.
//
// Daemon modes loop until ctx is cancelled and then return a zero summary; only
// the one-shot summary is meaningful.
func Serve(ctx context.Context, opts ServeOptions) (RunSummary, error) {
	if opts.IsOneShot() {
		return opts.Run(ctx), nil
	}

	fallback := opts.FallbackSeconds
	if fallback <= 0 {
		fallback = 900 // safety-net sweep cadence when running long-lived
	}
	queue := opts.Queue
	if queue == "" {
		queue = "transcode:requests"
	}

	var rdb *redis.Client
	if opts.RedisURL != "" {
		o, err := redis.ParseURL(opts.RedisURL)
		if err != nil {
			return RunSummary{}, fmt.Errorf("invalid REDIS_URL: %w", err)
		}
		rdb = redis.NewClient(o)
		defer rdb.Close()
	}
	opts.Logger.Info("entering daemon mode", Fields{"wake": rdb != nil, "queue": queue, "fallbackSeconds": fallback})

	for ctx.Err() == nil {
		opts.Run(ctx)
		if ctx.Err() != nil {
			return RunSummary{}, nil
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
				return RunSummary{}, nil
			}
			opts.Logger.Warn("redis BLPOP error; backing off", Fields{"error": err.Error()})
			sleepCtx(ctx, 5*time.Second)
		default:
			opts.Logger.Debug("woke on request", nil)
		}
	}
	return RunSummary{}, nil
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

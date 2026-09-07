package core

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// RunFunc executes one processing pass and returns its summary.
type RunFunc func(ctx context.Context) RunSummary

// ServeOptions configures the trigger loop shared by entrypoints (transcoder,
// imagehasher). Keeping the loop here guarantees every worker supports the same
// modes identically, and that an environment can switch between them by config
// alone.
type ServeOptions struct {
	Logger          *Logger
	RedisURL        string // empty → no wake queue
	Queue           string // BLPOP key for wake mode; defaults to "transcode:requests"
	WakeHTTPAddr    string // empty → no HTTP wake endpoint; e.g. ":8787"
	WakeHTTPToken   string // when set, POST /wake must present it as a bearer token
	FallbackSeconds int    // poll/safety-net cadence; 0 with no wake source → one-shot
	Run             RunFunc
}

// IsOneShot reports whether the options select a single cron pass (no wake
// source and no poll interval).
func (o ServeOptions) IsOneShot() bool {
	return o.RedisURL == "" && o.WakeHTTPAddr == "" && o.FallbackSeconds == 0
}

// Serve runs the trigger mode selected by opts:
//
//   - one-shot (cron): no wake source and FallbackSeconds==0 — run a single
//     pass and return its summary (callers use it for the process exit code).
//   - poll: FallbackSeconds>0, no wake source — run a pass, sleep, repeat.
//   - wake: RedisURL and/or WakeHTTPAddr set — run a pass, then wait for a wake
//     request or the FallbackSeconds safety-net timeout, so an upload is picked
//     up within seconds while the timeout still guarantees a periodic sweep.
//
// Wake requests are only signals: every pass is a full scan of the source
// bucket, so a lost, duplicated, or unrelated request costs at most one extra
// pass and can never leave an upload unprocessed. Requests arriving during a
// pass coalesce into a single follow-up run.
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

	// Buffered depth 1: requests that arrive during a pass coalesce into one
	// follow-up run, because a single pass drains all pending work anyway.
	wake := make(chan struct{}, 1)

	var rdb *redis.Client
	if opts.RedisURL != "" {
		o, err := redis.ParseURL(opts.RedisURL)
		if err != nil {
			return RunSummary{}, fmt.Errorf("invalid REDIS_URL: %w", err)
		}
		rdb = redis.NewClient(o)
		defer rdb.Close()
		go watchQueue(ctx, rdb, queue, time.Duration(fallback)*time.Second, wake, opts.Logger)
	}

	if opts.WakeHTTPAddr != "" {
		stop, err := serveWakeHTTP(ctx, opts, wake)
		if err != nil {
			return RunSummary{}, err
		}
		defer stop()
	}

	opts.Logger.Info("entering daemon mode", Fields{
		"redisWake": rdb != nil, "queue": queue,
		"httpWake": opts.WakeHTTPAddr != "", "httpWakeAddr": opts.WakeHTTPAddr,
		"fallbackSeconds": fallback,
	})

	timer := time.NewTimer(time.Duration(fallback) * time.Second)
	defer timer.Stop()
	for ctx.Err() == nil {
		opts.Run(ctx)
		if ctx.Err() != nil {
			return RunSummary{}, nil
		}

		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(time.Duration(fallback) * time.Second)
		select {
		case <-ctx.Done():
		case <-wake:
			opts.Logger.Debug("woke on request", nil)
		case <-timer.C:
			// safety-net sweep
		}
	}
	return RunSummary{}, nil
}

// watchQueue blocks on the Redis wake queue and signals wake for each request.
// A single pass drains all pending work, so it does not matter that requests
// pushed while a pass is running collapse into one follow-up run.
func watchQueue(ctx context.Context, rdb *redis.Client, queue string, timeout time.Duration, wake chan<- struct{}, logger *Logger) {
	for ctx.Err() == nil {
		_, err := rdb.BLPop(ctx, timeout, queue).Result()
		switch {
		case errors.Is(err, redis.Nil):
			// timeout → nothing queued; the fallback timer handles sweeps
		case err != nil:
			if ctx.Err() != nil {
				return
			}
			logger.Warn("redis BLPOP error; backing off", Fields{"error": err.Error()})
			sleepCtx(ctx, 5*time.Second)
		default:
			signal(wake)
		}
	}
}

// serveWakeHTTP starts the HTTP wake endpoint and returns a shutdown function.
//
// The endpoint exists so that any event source an operator already has — an S3
// or R2 event notification routed through a Lambda, a Worker, or a queue
// consumer, or the uploading app itself — can shorten the wait for a new upload
// without the worker needing a client for that source. Request bodies are
// ignored: the pass that follows is a full scan.
func serveWakeHTTP(ctx context.Context, opts ServeOptions, wake chan<- struct{}) (func(), error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/wake", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !wakeTokenValid(opts.WakeHTTPToken, r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		signal(wake)
		opts.Logger.Debug("wake requested over HTTP", Fields{"remoteAddr": r.RemoteAddr})
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("accepted\n"))
	})

	server := &http.Server{
		Addr:              opts.WakeHTTPAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	listener, err := newListener(ctx, opts.WakeHTTPAddr)
	if err != nil {
		return nil, fmt.Errorf("WAKE_HTTP_ADDR %q: %w", opts.WakeHTTPAddr, err)
	}
	if opts.WakeHTTPToken == "" {
		opts.Logger.Warn("HTTP wake endpoint has no token; anyone who can reach it can trigger a pass", Fields{"addr": listener.Addr().String()})
	}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			opts.Logger.Error("wake HTTP server stopped", Fields{"error": err.Error()})
		}
	}()
	return func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}, nil
}

// wakeTokenValid checks the shared secret, when one is configured, against
// either an Authorization: Bearer header or X-Wake-Token.
func wakeTokenValid(token string, r *http.Request) bool {
	if token == "" {
		return true
	}
	presented := r.Header.Get("X-Wake-Token")
	if presented == "" {
		presented = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(token)) == 1
}

// signal delivers a wake without blocking; a pending signal already covers it.
func signal(wake chan<- struct{}) {
	select {
	case wake <- struct{}{}:
	default:
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

func newListener(ctx context.Context, addr string) (net.Listener, error) {
	var lc net.ListenConfig
	return lc.Listen(ctx, "tcp", addr)
}

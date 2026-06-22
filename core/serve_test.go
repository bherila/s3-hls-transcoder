package core

import (
	"context"
	"testing"
)

func TestServeOneShotRunsOnceAndReturnsSummary(t *testing.T) {
	calls := 0
	opts := ServeOptions{
		Logger: NewLogger(LevelError),
		Run: func(ctx context.Context) RunSummary {
			calls++
			return RunSummary{Failed: 2}
		},
	}
	if !opts.IsOneShot() {
		t.Fatal("expected one-shot mode with no RedisURL and zero fallback")
	}

	sum, err := Serve(context.Background(), opts)
	if err != nil {
		t.Fatalf("Serve error: %v", err)
	}
	if calls != 1 {
		t.Errorf("Run called %d times, want 1", calls)
	}
	if sum.Failed != 2 {
		t.Errorf("summary not propagated: Failed = %d, want 2", sum.Failed)
	}
}

func TestServePollExitsOnCancelledContext(t *testing.T) {
	// Poll mode (no redis, fallback>0). A pre-cancelled context must make the
	// loop exit immediately without looping forever; Run may execute zero or one
	// time depending on the cancellation race, but Serve must return.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	opts := ServeOptions{
		Logger:          NewLogger(LevelError),
		FallbackSeconds: 1,
		Run:             func(ctx context.Context) RunSummary { return RunSummary{} },
	}
	if opts.IsOneShot() {
		t.Fatal("poll mode should not be one-shot")
	}

	done := make(chan struct{})
	go func() {
		_, _ = Serve(ctx, opts)
		close(done)
	}()
	<-done // will hang and fail the test via timeout if Serve does not return
}

func TestServeInvalidRedisURL(t *testing.T) {
	opts := ServeOptions{
		Logger:          NewLogger(LevelError),
		RedisURL:        "://not-a-url",
		FallbackSeconds: 1,
		Run:             func(ctx context.Context) RunSummary { return RunSummary{} },
	}
	if _, err := Serve(context.Background(), opts); err == nil {
		t.Error("expected error for invalid REDIS_URL")
	}
}

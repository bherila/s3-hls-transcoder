package core

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
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

// --- HTTP wake endpoint -----------------------------------------------------

// startWakeServer starts Serve on a free port with the HTTP wake endpoint
// enabled and returns its base URL. FallbackSeconds is deliberately long so
// that every pass observed in a test is attributable to a wake request, not to
// the safety net.
func startWakeServer(t *testing.T, token string, run RunFunc) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	opts := ServeOptions{
		Logger:          NewLogger(LevelError),
		WakeHTTPAddr:    addr,
		WakeHTTPToken:   token,
		FallbackSeconds: 3600,
		Run:             run,
	}
	if opts.IsOneShot() {
		t.Fatal("an HTTP wake address must select daemon mode")
	}

	served := make(chan error, 1)
	go func() {
		_, err := Serve(ctx, opts)
		served <- err
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-served:
			if err != nil {
				t.Errorf("Serve returned error: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("Serve did not return after cancellation")
		}
	})
	return "http://" + addr
}

// startCountingWakeServer starts a wake server whose passes are instantaneous,
// reporting each one on the returned channel. The startup pass is consumed
// before returning so tests begin from a known state.
func startCountingWakeServer(t *testing.T, token string) (string, <-chan struct{}) {
	t.Helper()
	runs := make(chan struct{}, 16)
	base := startWakeServer(t, token, func(context.Context) RunSummary {
		runs <- struct{}{}
		return RunSummary{}
	})
	waitForPass(t, runs, "startup pass")
	return base, runs
}

func waitForPass(t *testing.T, runs <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-runs:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func postWake(t *testing.T, base, header, value string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/wake", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if header != "" {
		req.Header.Set(header, value)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /wake: %v", err)
	}
	return res
}

func TestServeHTTPWakeTriggersAPass(t *testing.T) {
	base, runs := startCountingWakeServer(t, "")

	res := postWake(t, base, "", "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusAccepted)
	}
	waitForPass(t, runs, "pass triggered by POST /wake")
}

func TestServeHTTPWakeRequiresTheConfiguredToken(t *testing.T) {
	base, runs := startCountingWakeServer(t, "s3cret")

	for _, tc := range []struct{ header, value string }{
		{"", ""},
		{"X-Wake-Token", "wrong"},
		{"Authorization", "Bearer wrong"},
	} {
		res := postWake(t, base, tc.header, tc.value)
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("header %q=%q: status = %d, want %d", tc.header, tc.value, res.StatusCode, http.StatusUnauthorized)
		}
		res.Body.Close()
	}
	if unexpected := len(runs); unexpected != 0 {
		t.Errorf("unauthorized requests triggered %d passes", unexpected)
	}

	for _, tc := range []struct{ header, value string }{
		{"X-Wake-Token", "s3cret"},
		{"Authorization", "Bearer s3cret"},
	} {
		res := postWake(t, base, tc.header, tc.value)
		if res.StatusCode != http.StatusAccepted {
			t.Errorf("header %q: status = %d, want %d", tc.header, res.StatusCode, http.StatusAccepted)
		}
		res.Body.Close()
		waitForPass(t, runs, "pass triggered by an authorized wake")
	}
}

func TestServeHTTPWakeRejectsGet(t *testing.T) {
	base, _ := startCountingWakeServer(t, "")

	res, err := http.Get(base + "/wake")
	if err != nil {
		t.Fatalf("GET /wake: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", res.StatusCode, http.StatusMethodNotAllowed)
	}
}

func TestServeHTTPWakeServesHealthz(t *testing.T) {
	base, _ := startCountingWakeServer(t, "s3cret")

	// Liveness must not require the wake token: it is for process supervisors.
	res, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", res.StatusCode, http.StatusOK)
	}
}

func TestServeHTTPWakeCoalescesRequestsDuringAPass(t *testing.T) {
	// A burst arriving while a pass is running must produce exactly one further
	// pass: that pass rescans the whole bucket, so it already covers every
	// upload the burst announced.
	started := make(chan struct{}, 16)
	release := make(chan struct{})
	base := startWakeServer(t, "", func(context.Context) RunSummary {
		started <- struct{}{}
		<-release
		return RunSummary{}
	})

	waitForPass(t, started, "startup pass to begin")
	for range 5 {
		postWake(t, base, "", "").Body.Close()
	}
	close(release) // let the startup pass finish and the loop consume the wake

	waitForPass(t, started, "the single coalesced follow-up pass")
	time.Sleep(200 * time.Millisecond)
	if extra := len(started); extra != 0 {
		t.Errorf("burst of 5 requests produced %d extra passes; requests should coalesce", extra)
	}
}

func TestServeRejectsAnUnusableWakeAddress(t *testing.T) {
	opts := ServeOptions{
		Logger:       NewLogger(LevelError),
		WakeHTTPAddr: "127.0.0.1:not-a-port",
		Run:          func(context.Context) RunSummary { return RunSummary{} },
	}
	if _, err := Serve(context.Background(), opts); err == nil {
		t.Error("expected an error for an unusable WAKE_HTTP_ADDR")
	}
}

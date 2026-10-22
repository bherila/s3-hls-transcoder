package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type schedulerS3 struct {
	mu                 sync.Mutex
	locks              map[string][]byte
	objects            map[string][]string
	failLock, failScan string
	stallScan          string
	etags              map[string]string
}

func schedulerFixture(t *testing.T) (OrchestratorOptions, *schedulerS3) {
	t.Helper()
	f := &schedulerS3{locks: map[string][]byte{}, objects: map[string][]string{
		"source-a": {"videos/first.mp4", "videos/second.mp4", "videos/third.mp4"},
		"source-b": {"videos/first.mp4", "videos/second.mp4"},
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/")
		bucket := strings.SplitN(path, "/", 2)[0]
		// Each tenant has different credentials, including on the same endpoint.
		suffix := bucket[len(bucket)-1:]
		if !strings.Contains(r.Header.Get("Authorization"), "Credential=key-"+suffix+"/") {
			t.Errorf("wrong credentials for %s", bucket)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.URL.Query().Get("list-type") == "2" {
			if bucket == f.stallScan {
				f.mu.Unlock()
				<-r.Context().Done()
				f.mu.Lock()
				return
			}
			if bucket == f.failScan {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			if got := r.URL.Query().Get("prefix"); got != "videos/" {
				t.Errorf("prefix = %q", got)
			}
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprint(w, `<ListBucketResult><IsTruncated>false</IsTruncated>`)
			keys := slices.Clone(f.objects[bucket])
			slices.Sort(keys)
			for _, key := range keys {
				if after := r.URL.Query().Get("start-after"); after != "" && key <= after {
					continue
				}
				etag := "etag"
				if changed := f.etags[bucket+"/"+key]; changed != "" {
					etag = changed
				}
				fmt.Fprintf(w, `<Contents><Key>%s</Key><ETag>%s</ETag><Size>100</Size><LastModified>2026-10-01T00:00:00Z</LastModified></Contents>`, key, etag)
			}
			fmt.Fprint(w, `</ListBucketResult>`)
			return
		}
		switch r.Method {
		case http.MethodPut:
			if bucket == f.failLock {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			if _, ok := f.locks[path]; ok && r.Header.Get("If-None-Match") == "*" {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			f.locks[path], _ = io.ReadAll(r.Body)
		case http.MethodGet:
			body, ok := f.locks[path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("ETag", etagFor(body))
			_, _ = w.Write(body)
		case http.MethodDelete:
			delete(f.locks, path)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(server.Close)
	var pairs []BucketPair
	for _, suffix := range []string{"a", "b"} {
		source := BucketConfig{Bucket: "source-" + suffix, Endpoint: server.URL, AccessKeyID: "key-" + suffix, SecretAccessKey: "secret-" + suffix, Region: "auto", Prefix: "videos/"}
		dest := source
		dest.Bucket = "dest-" + suffix
		dest.Prefix = ""
		pairs = append(pairs, BucketPair{Name: suffix, Source: source, Dest: dest})
	}
	return OrchestratorOptions{Config: &Config{Pairs: pairs, MaxRuntimeSeconds: 60, BudgetMultiplier: 0.75, LockTTLMultiplier: 1.5, Platform: PlatformLocal}, Logger: NewLogger(LevelError)}, f
}

func assertSchedulerLocksReleased(t *testing.T, f *schedulerS3) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for key := range f.locks {
		if strings.HasSuffix(key, "/"+GlobalLockKey) || strings.HasSuffix(key, "/.processing") {
			t.Errorf("lock leaked: %s", key)
		}
		if strings.Contains(key, "/errors/") {
			t.Errorf("interruption created a failure tombstone: %s", key)
		}
	}
}

func TestSchedulerRoundRobinAndIsolation(t *testing.T) {
	opts, f := schedulerFixture(t)
	var order []string
	next := 0
	sum := runScheduled(context.Background(), opts, &next, func(ctx context.Context, a pairArgs, s SourceObject) (processOutcome, error) {
		order = append(order, a.pair.Name+":"+s.Key)
		if a.pair.Source.Bucket != "source-"+a.pair.Name || a.pair.Dest.Bucket != "dest-"+a.pair.Name {
			t.Fatal("mixed tenant routing")
		}
		return outcomeCached, nil
	}, false)
	want := []string{"a:videos/first.mp4", "b:videos/first.mp4", "a:videos/second.mp4", "b:videos/second.mp4", "a:videos/third.mp4"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	if sum.Cached != 5 || sum.PairsProcessed != 2 || sum.Failed != 0 {
		t.Fatalf("summary = %+v", sum)
	}
	assertSchedulerLocksReleased(t, f)
}

func TestRunnerRotatesAfterCancellationWithoutTombstones(t *testing.T) {
	opts, f := schedulerFixture(t)
	opts.Config.ErrorTombstones = true
	var order []string
	runner := newRunner(opts, func(ctx context.Context, a pairArgs, s SourceObject) (processOutcome, error) {
		order = append(order, a.pair.Name)
		<-ctx.Done()
		return 0, ctx.Err()
	}, false)
	for range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
		sum := runner(ctx)
		cancel()
		if sum.Failed != 0 {
			t.Fatalf("interruption counted as source failure: %+v", sum)
		}
		assertSchedulerLocksReleased(t, f) // would include a tombstone PUT, too
	}
	if !reflect.DeepEqual(order, []string{"a", "b"}) {
		t.Fatalf("next tenant not retained: %v", order)
	}
}

func TestSchedulerEnforcesSharedDeadline(t *testing.T) {
	opts, f := schedulerFixture(t)
	opts.Config.MaxRuntimeSeconds, opts.Config.BudgetMultiplier = 1, 1
	next := 0
	started := time.Now()
	sum := runScheduled(context.Background(), opts, &next, func(ctx context.Context, _ pairArgs, _ SourceObject) (processOutcome, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	}, false)
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("deadline did not stop work: %s", elapsed)
	}
	if next != 1 || sum.Failed != 0 {
		t.Fatalf("next=%d summary=%+v", next, sum)
	}
	assertSchedulerLocksReleased(t, f)
}

func TestSchedulerSkipsBusyOrFailingTenant(t *testing.T) {
	for _, failure := range []string{"busy", "lock", "scan"} {
		t.Run(failure, func(t *testing.T) {
			opts, f := schedulerFixture(t)
			switch failure {
			case "busy":
				body, _ := json.Marshal(LockBody{WorkerID: "other", StartedAt: time.Now().UTC().Format(time.RFC3339Nano), LockTTLSeconds: 120})
				f.locks["dest-a/"+GlobalLockKey] = body
			case "lock":
				f.failLock = "dest-a"
			case "scan":
				f.failScan = "source-a"
			}
			next, count := 0, 0
			sum := runScheduled(context.Background(), opts, &next, func(ctx context.Context, a pairArgs, _ SourceObject) (processOutcome, error) {
				if a.pair.Name != "b" {
					t.Fatal("processed blocked tenant")
				}
				count++
				return outcomeTranscoded, nil
			}, false)
			wantFailures := 1
			if failure == "lock" {
				wantFailures++ // This destination also refuses the coordinator write.
			}
			if failure == "busy" {
				wantFailures = 0
				delete(f.locks, "dest-a/"+GlobalLockKey)
			}
			if count != 2 || sum.Failed != wantFailures {
				t.Fatalf("count=%d summary=%+v", count, sum)
			}
			assertSchedulerLocksReleased(t, f)
		})
	}
}

func TestRunnerRotatesAfterStalledListing(t *testing.T) {
	opts, f := schedulerFixture(t)
	f.stallScan = "source-a"
	var order []string
	runner := newRunner(opts, func(ctx context.Context, a pairArgs, s SourceObject) (processOutcome, error) {
		order = append(order, a.pair.Name)
		return outcomeCached, nil
	}, false)
	for range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
		runner(ctx)
		cancel()
		assertSchedulerLocksReleased(t, f)
	}
	if len(order) == 0 || order[0] != "b" {
		t.Fatalf("stalled tenant starved b: %v", order)
	}
}

func TestWorkersCacheSameSourceKeyInDedicatedBuckets(t *testing.T) {
	for _, images := range []bool{false, true} {
		t.Run(fmt.Sprintf("images=%v", images), func(t *testing.T) {
			opts, f := schedulerFixture(t)
			key := "videos/shared.mp4"
			if images {
				key = "videos/shared.jpg"
			}
			for _, suffix := range []string{"a", "b"} {
				f.objects["source-"+suffix] = []string{key}
				var body []byte
				mappingKey := MappingKey(key)
				if images {
					mappingKey = ImageMappingKey(key)
					body, _ = json.Marshal(ImageMapping{SourceKey: key, SourceBucket: "source-" + suffix, SourceEtag: "etag", SourceSize: 100, PDQHash: suffix})
				} else {
					body, _ = json.Marshal(SourceMapping{SourceKey: key, SourceBucket: "source-" + suffix, SourceEtag: "etag", SourceSize: 100, ContentID: "sha256:" + suffix})
				}
				f.locks["dest-"+suffix+"/"+mappingKey] = body
			}
			var sum RunSummary
			if images {
				sum = RunImagesOnce(context.Background(), opts)
			} else {
				sum = RunOnce(context.Background(), opts)
			}
			if sum.Cached != 2 || sum.Failed != 0 {
				t.Fatalf("summary = %+v", sum)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.locks) != 2 {
				t.Fatalf("expected only two original mappings, got %v", f.locks)
			}
		})
	}
}

func TestSchedulerCheckpointProgressSurvivesNewCronRunnerAndWraps(t *testing.T) {
	opts, f := schedulerFixture(t)
	opts.Config.MaxRuntimeSeconds, opts.Config.BudgetMultiplier = 1, 1
	// B gets one cached turn before A's expensive source exhausts the budget.
	next := 1
	runScheduled(context.Background(), opts, &next, func(ctx context.Context, a pairArgs, source SourceObject) (processOutcome, error) {
		if a.pair.Name == "a" {
			<-ctx.Done()
			return 0, ctx.Err()
		}
		return outcomeCached, nil
	}, false)
	assertSchedulerLocksReleased(t, f)
	for _, tenant := range []string{"a", "b"} {
		if _, ok := f.locks["dest-"+tenant+"/"+checkpointKey(false)]; !ok {
			t.Fatalf("missing durable progress for tenant %s", tenant)
		}
	}
	var order []string
	opts.Config.MaxRuntimeSeconds = 60
	// A new runner has no in-memory tenant cursor, like a fresh cron process.
	newRunner(opts, func(_ context.Context, a pairArgs, source SourceObject) (processOutcome, error) {
		order = append(order, a.pair.Name+":"+source.Key)
		return outcomeCached, nil
	}, false)(context.Background())
	want := []string{"b:videos/second.mp4", "a:videos/second.mp4", "a:videos/third.mp4"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("fresh runner did not progress beyond interrupted/cached prefixes: %v", order)
	}
	// A changed source behind the old cursor must be revisited after EOF wraps.
	f.etags = map[string]string{"source-a/videos/first.mp4": "changed"}
	f.objects["source-b"] = []string{"videos/second.mp4"} // Deleted cursor keys are valid StartAfter boundaries.
	var changed bool
	newRunner(opts, func(_ context.Context, a pairArgs, source SourceObject) (processOutcome, error) {
		if a.pair.Name == "a" && source.Key == "videos/first.mp4" {
			changed = source.ETag == "changed"
		}
		return outcomeCached, nil
	}, false)(context.Background())
	if !changed {
		t.Fatal("full wrap did not retry the interrupted source's updated version")
	}
	assertSchedulerLocksReleased(t, f)
}

func TestSchedulerFreshCronRotatesPastExpensiveFirstTenant(t *testing.T) {
	opts, f := schedulerFixture(t)
	opts.Config.MaxRuntimeSeconds, opts.Config.BudgetMultiplier = 1, 1
	newRunner(opts, func(ctx context.Context, a pairArgs, _ SourceObject) (processOutcome, error) {
		if a.pair.Name != "a" {
			t.Fatal("expected first tenant to consume this run")
		}
		<-ctx.Done()
		return 0, ctx.Err()
	}, false)(context.Background())
	opts.Config.MaxRuntimeSeconds = 60
	var order []string
	newRunner(opts, func(_ context.Context, a pairArgs, s SourceObject) (processOutcome, error) {
		order = append(order, a.pair.Name+":"+s.Key)
		return outcomeCached, nil
	}, false)(context.Background())
	if len(order) < 2 || order[0] != "b:videos/first.mp4" || order[1] != "a:videos/second.mp4" {
		t.Fatalf("fresh cron must visit B before A's remaining backlog: %v", order)
	}
	var retried bool
	newRunner(opts, func(_ context.Context, a pairArgs, s SourceObject) (processOutcome, error) {
		retried = retried || (a.pair.Name == "a" && s.Key == "videos/first.mp4")
		return outcomeCached, nil
	}, false)(context.Background())
	if !retried {
		t.Fatal("interrupted first source was not retried after wrapping")
	}
	assertSchedulerLocksReleased(t, f)
}

func TestSchedulerTenantCheckpointIdentityAndValidation(t *testing.T) {
	opts, f := schedulerFixture(t)
	pairs := opts.Config.Pairs
	key := tenantCheckpointKey(pairs, false)
	changed := slices.Clone(pairs)
	changed[0].Source.Prefix = "other/"
	if key == tenantCheckpointKey(changed, false) || key == tenantCheckpointKey(pairs, true) {
		t.Fatal("different inputs/workers shared a coordinator")
	}
	changed = slices.Clone(pairs)
	changed[0].Dest.SecretAccessKey = "rotated-secret"
	if key != tenantCheckpointKey(changed, false) {
		t.Fatal("credential rotation reset scheduling progress")
	}
	f.locks[pairs[0].Dest.Bucket+"/"+key] = []byte(`{"version":1,"nextPair":99}`)
	if _, err := readTenantCheckpoint(context.Background(), pairs, false, 0); err == nil {
		t.Fatal("accepted an out-of-range tenant index")
	}
}

func TestSchedulerSignalRetriesInterruptedSource(t *testing.T) {
	opts, f := schedulerFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	next := 0
	runScheduled(ctx, opts, &next, func(_ context.Context, _ pairArgs, source SourceObject) (processOutcome, error) {
		if source.Key == "videos/second.mp4" {
			cancel()
			return 0, ctx.Err()
		}
		return outcomeCached, nil
	}, false)
	var first string
	newRunner(opts, func(_ context.Context, a pairArgs, source SourceObject) (processOutcome, error) {
		if first == "" && a.pair.Name == "a" {
			first = source.Key
		}
		return outcomeCached, nil
	}, false)(context.Background())
	if first != "videos/second.mp4" {
		t.Fatalf("signal must retry the interrupted source, got %q", first)
	}
	assertSchedulerLocksReleased(t, f)
}

func TestSchedulerDeletedCursorKeyAndInputIdentity(t *testing.T) {
	opts, f := schedulerFixture(t)
	pair := opts.Config.Pairs[0]
	a := pairArgs{pair: pair, destClient: NewS3Client(pair.Dest)}
	ctx := context.Background()
	if err := writeSourceCheckpoint(ctx, a, false, "videos/first.mp4"); err != nil {
		t.Fatal(err)
	}
	f.objects["source-a"] = []string{"videos/second.mp4"}
	var first string
	newRunner(opts, func(_ context.Context, a pairArgs, source SourceObject) (processOutcome, error) {
		if first == "" && a.pair.Name == "a" {
			first = source.Key
		}
		return outcomeCached, nil
	}, false)(ctx)
	if first != "videos/second.mp4" {
		t.Fatalf("deleted cursor key prevented progress: %q", first)
	}
	if err := writeSourceCheckpoint(ctx, a, false, "videos/second.mp4"); err != nil {
		t.Fatal(err)
	}
	a.pair.Source.Prefix = "changed/"
	if key, err := readSourceCheckpoint(ctx, a, false); err != nil || key != "" {
		t.Fatalf("a new input must not inherit old progress: %q, %v", key, err)
	}
	if key, err := readSourceCheckpoint(ctx, pairArgs{pair: pair, destClient: a.destClient}, true); err != nil || key != "" {
		t.Fatalf("image and video progress must be isolated: %q, %v", key, err)
	}
}

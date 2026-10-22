package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
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
}

func schedulerFixture(t *testing.T) (OrchestratorOptions, *schedulerS3) {
	t.Helper()
	f := &schedulerS3{locks: map[string][]byte{}, objects: map[string][]string{
		"source-a": {"videos/shared.mp4", "videos/second.mp4", "videos/third.mp4"},
		"source-b": {"videos/shared.mp4", "videos/second.mp4"},
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
			for _, key := range f.objects[bucket] {
				fmt.Fprintf(w, `<Contents><Key>%s</Key><ETag>etag</ETag><Size>100</Size><LastModified>2026-10-01T00:00:00Z</LastModified></Contents>`, key)
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
			if _, ok := f.locks[path]; ok {
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
	if len(f.locks) != 0 {
		t.Errorf("locks leaked: %v", f.locks)
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
	want := []string{"a:videos/shared.mp4", "b:videos/shared.mp4", "a:videos/second.mp4", "b:videos/second.mp4", "a:videos/third.mp4"}
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

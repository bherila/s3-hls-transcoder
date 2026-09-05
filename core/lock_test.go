package core

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type lockObject struct {
	body []byte
	etag string
}

type lockS3Fake struct {
	mu       sync.Mutex
	objects  map[string]lockObject
	afterGet func(key string)
	getOnce  sync.Once
}

func newLockS3Client(t *testing.T, fake *lockS3Fake) (*s3.Client, func()) {
	t.Helper()
	fake.objects = map[string]lockObject{}
	server := httptest.NewServer(http.HandlerFunc(fake.serveHTTP))
	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(server.URL),
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		UsePathStyle: true,
	})
	return client, server.Close
}

func (f *lockS3Fake) putLock(key, worker string, startedAt time.Time, ttl float64) {
	body, _ := json.Marshal(LockBody{WorkerID: worker, Platform: "local", Hostname: "test", StartedAt: startedAt.UTC().Format(time.RFC3339Nano), LockTTLSeconds: ttl})
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = lockObject{body: body, etag: etagFor(body)}
}

func (f *lockS3Fake) workerID(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	obj, ok := f.objects[key]
	if !ok {
		return ""
	}
	var body LockBody
	_ = json.Unmarshal(obj.body, &body)
	return body.WorkerID
}

func (f *lockS3Fake) serveHTTP(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/bucket/")
	switch r.Method {
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("If-None-Match") == "*" {
			if _, exists := f.objects[key]; exists {
				http.Error(w, "precondition failed", http.StatusPreconditionFailed)
				return
			}
		}
		f.objects[key] = lockObject{body: body, etag: etagFor(body)}
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		f.mu.Lock()
		obj, ok := f.objects[key]
		f.mu.Unlock()
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("ETag", obj.etag)
		_, _ = w.Write(obj.body)
		if f.afterGet != nil {
			f.getOnce.Do(func() { f.afterGet(key) })
		}
	case http.MethodDelete:
		f.mu.Lock()
		defer f.mu.Unlock()
		obj, ok := f.objects[key]
		if match := r.Header.Get("If-Match"); match != "" && (!ok || match != obj.etag) {
			http.Error(w, "precondition failed", http.StatusPreconditionFailed)
			return
		}
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unsupported", http.StatusMethodNotAllowed)
	}
}

func etagFor(body []byte) string {
	sum := md5.Sum(body)
	return fmt.Sprintf("%q", hex.EncodeToString(sum[:]))
}

func TestReleaseDoesNotDeleteAnotherWorkersLock(t *testing.T) {
	ctx := context.Background()
	fake := &lockS3Fake{}
	client, closeServer := newLockS3Client(t, fake)
	defer closeServer()

	fake.putLock(GlobalLockKey, "successor", time.Now(), 60)
	h := &LockHandle{WorkerID: "expired-owner", client: client, bucket: "bucket", key: GlobalLockKey, logger: NewLogger(LevelError)}

	h.Release(ctx)

	if got := fake.workerID(GlobalLockKey); got != "successor" {
		t.Fatalf("Release deleted or changed successor lock, got worker %q", got)
	}
}

func TestStaleTakeoverDoesNotDeleteFreshLockAfterRead(t *testing.T) {
	ctx := context.Background()
	fake := &lockS3Fake{}
	client, closeServer := newLockS3Client(t, fake)
	defer closeServer()

	fake.putLock(GlobalLockKey, "stale", time.Now().Add(-time.Hour), 1)
	fake.afterGet = func(key string) { fake.putLock(key, "fresh", time.Now(), 60) }

	h, err := AcquireLock(ctx, AcquireOptions{Client: client, Bucket: "bucket", Platform: PlatformLocal, MaxRuntimeSeconds: 10, LockTTLSeconds: 1, Logger: NewLogger(LevelError)})
	if err != nil {
		t.Fatalf("AcquireLock returned error: %v", err)
	}
	if h != nil {
		t.Fatalf("AcquireLock unexpectedly acquired lock after racing fresh worker")
	}
	if got := fake.workerID(GlobalLockKey); got != "fresh" {
		t.Fatalf("stale takeover deleted or changed fresh lock, got worker %q", got)
	}
}

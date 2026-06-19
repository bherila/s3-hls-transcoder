package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// GlobalLockKey is the destination-bucket key of the single-runner lock.
const GlobalLockKey = ".transcoder.lock"

// LockBody is the JSON written into a lock/lease object.
type LockBody struct {
	WorkerID       string  `json:"workerId"`
	Platform       string  `json:"platform"`
	Hostname       string  `json:"hostname"`
	StartedAt      string  `json:"startedAt"`
	ExpectedEndBy  string  `json:"expectedEndBy"`
	LockTTLSeconds float64 `json:"lockTtlSeconds"`
}

// LockHandle is a held lock; call Release to drop it.
type LockHandle struct {
	WorkerID string
	client   *s3.Client
	bucket   string
	key      string
	logger   *Logger
}

// Release deletes the lock object. Best-effort: a failure is logged and the
// lock is left to expire after its TTL.
func (h *LockHandle) Release(ctx context.Context) {
	if _, err := h.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(h.bucket), Key: aws.String(h.key)}); err != nil {
		h.logger.Warn("failed to release lock; will expire after TTL", Fields{"key": h.key, "workerId": h.WorkerID, "error": err.Error()})
		return
	}
	h.logger.Info("released lock", Fields{"key": h.key, "workerId": h.WorkerID})
}

// AcquireOptions configures a lock acquisition.
type AcquireOptions struct {
	Client            *s3.Client
	Bucket            string
	Key               string // defaults to GlobalLockKey
	Platform          Platform
	MaxRuntimeSeconds float64
	LockTTLSeconds    float64
	Logger            *Logger
}

// AcquireLock takes a lock via conditional PUT (If-None-Match: *). Returns
// (handle, nil) on success, (nil, nil) if held by a live worker (caller should
// exit cleanly), or (nil, err) on a transport error. A stale lock (past TTL)
// is deleted and re-taken.
func AcquireLock(ctx context.Context, opts AcquireOptions) (*LockHandle, error) {
	key := opts.Key
	if key == "" {
		key = GlobalLockKey
	}

	handle, err := tryPutLock(ctx, opts, key)
	if err != nil {
		return nil, err
	}
	if handle != nil {
		return handle, nil
	}

	existing, err := readLock(ctx, opts.Client, opts.Bucket, key)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		// Race: lock vanished between PUT and GET. Try once more.
		return tryPutLock(ctx, opts, key)
	}

	startedAt, _ := time.Parse(time.RFC3339Nano, existing.StartedAt)
	ageSeconds := time.Since(startedAt).Seconds()
	if ageSeconds < existing.LockTTLSeconds {
		opts.Logger.Info("lock held by live worker; exiting", Fields{
			"key": key, "heldBy": existing.WorkerID,
			"ageSeconds": math.Round(ageSeconds), "ttlSeconds": existing.LockTTLSeconds,
		})
		return nil, nil
	}

	opts.Logger.Warn("stale lock found; attempting takeover", Fields{
		"key": key, "staleWorkerId": existing.WorkerID, "ageSeconds": math.Round(ageSeconds),
	})
	if _, err := opts.Client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(opts.Bucket), Key: aws.String(key)}); err != nil {
		opts.Logger.Warn("failed to delete stale lock; will retry PUT anyway", Fields{"key": key, "error": err.Error()})
	}
	return tryPutLock(ctx, opts, key)
}

func tryPutLock(ctx context.Context, opts AcquireOptions, key string) (*LockHandle, error) {
	workerID := newWorkerID()
	now := time.Now().UTC()
	host, _ := os.Hostname()
	body := LockBody{
		WorkerID:       workerID,
		Platform:       string(opts.Platform),
		Hostname:       host,
		StartedAt:      now.Format(time.RFC3339Nano),
		ExpectedEndBy:  now.Add(time.Duration(opts.MaxRuntimeSeconds) * time.Second).Format(time.RFC3339Nano),
		LockTTLSeconds: opts.LockTTLSeconds,
	}
	data, _ := json.MarshalIndent(body, "", "  ")

	_, err := opts.Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(opts.Bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/json"),
		IfNoneMatch: aws.String("*"),
	})
	if err != nil {
		if IsPreconditionFailed(err) {
			return nil, nil
		}
		return nil, err
	}
	opts.Logger.Info("acquired lock", Fields{"key": key, "workerId": workerID})
	return &LockHandle{WorkerID: workerID, client: opts.Client, bucket: opts.Bucket, key: key, logger: opts.Logger}, nil
}

func readLock(ctx context.Context, client *s3.Client, bucket, key string) (*LockBody, error) {
	out, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	defer out.Body.Close()
	var body LockBody
	if err := json.NewDecoder(out.Body).Decode(&body); err != nil {
		return nil, err
	}
	return &body, nil
}

// LeaseKey is the per-video lease object key.
func LeaseKey(contentID string) string { return "by-id/" + contentID + "/.processing" }

// AcquireLease takes a per-video lease using the same primitive as the global
// lock, keyed by content ID.
func AcquireLease(ctx context.Context, opts AcquireOptions, contentID string) (*LockHandle, error) {
	opts.Key = LeaseKey(contentID)
	return AcquireLock(ctx, opts)
}

// ComputeLockTTLSeconds = ceil(maxRuntime * multiplier).
func ComputeLockTTLSeconds(maxRuntime, multiplier float64) float64 {
	return math.Ceil(maxRuntime * multiplier)
}

// ComputeBudgetSeconds = floor(maxRuntime * multiplier).
func ComputeBudgetSeconds(maxRuntime, multiplier float64) float64 {
	return math.Floor(maxRuntime * multiplier)
}

func newWorkerID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

package core

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type tenantCheckpoint struct {
	Version  int `json:"version"`
	NextPair int `json:"nextPair"`
}

// This is a scheduling hint only: concurrent runners still require every
// destination lock. Hash ordered public identities, never credentials or keys.
func tenantCheckpointKey(pairs []BucketPair, images bool) string {
	identities := make([][5]string, len(pairs))
	for i, p := range pairs {
		identities[i] = [5]string{normalizeEndpoint(p.Source.Endpoint), p.Source.Bucket, p.Source.Prefix, normalizeEndpoint(p.Dest.Endpoint), p.Dest.Bucket}
	}
	b, _ := json.Marshal(identities)
	kind := "transcoder"
	if images {
		kind = "imagehasher"
	}
	return fmt.Sprintf(".scheduler/%s-%x.json", kind, sha256.Sum256(b))
}

func readTenantCheckpoint(ctx context.Context, pairs []BucketPair, images bool, fallback int) (int, error) {
	if len(pairs) < 2 {
		return fallback, nil
	}
	p := pairs[0]
	c, err := getJSONObject[tenantCheckpoint](ctx, NewS3Client(p.Dest), p.Dest.Bucket, tenantCheckpointKey(pairs, images))
	if err != nil || c == nil {
		return fallback, err
	}
	if c.Version != 1 || c.NextPair < 0 || c.NextPair >= len(pairs) {
		return 0, fmt.Errorf("invalid scheduler tenant checkpoint")
	}
	return c.NextPair, nil
}

func writeTenantCheckpoint(ctx context.Context, pairs []BucketPair, images bool, next int) error {
	if len(pairs) < 2 {
		return nil
	}
	p := pairs[0]
	return putJSONObject(ctx, NewS3Client(p.Dest), p.Dest.Bucket, tenantCheckpointKey(pairs, images), tenantCheckpoint{Version: 1, NextPair: next})
}

// One bounded checkpoint per destination/worker, protected by its global lock.
// Source keys are private bookkeeping, never placed under the player-facing tree.
type sourceCheckpoint struct {
	Version        int    `json:"version"`
	SourceBucket   string `json:"sourceBucket"`
	SourceEndpoint string `json:"sourceEndpoint"`
	SourcePrefix   string `json:"sourcePrefix"`
	LastSourceKey  string `json:"lastSourceKey"`
}

func checkpointKey(images bool) string {
	if images {
		return ".imagehasher.cursor.json"
	}
	return ".transcoder.cursor.json"
}

func readSourceCheckpoint(ctx context.Context, a pairArgs, images bool) (string, error) {
	c, err := getJSONObject[sourceCheckpoint](ctx, a.destClient, a.pair.Dest.Bucket, checkpointKey(images))
	if err != nil || c == nil {
		return "", err
	}
	if c.Version != 1 {
		return "", fmt.Errorf("unsupported scheduler checkpoint version %d", c.Version)
	}
	if c.SourceBucket != a.pair.Source.Bucket || normalizeEndpoint(c.SourceEndpoint) != normalizeEndpoint(a.pair.Source.Endpoint) || c.SourcePrefix != a.pair.Source.Prefix {
		return "", nil // A newly configured input must start with its own full scan.
	}
	if !strings.HasPrefix(c.LastSourceKey, c.SourcePrefix) {
		return "", fmt.Errorf("scheduler checkpoint is outside the configured source prefix")
	}
	return c.LastSourceKey, nil
}

func writeSourceCheckpoint(ctx context.Context, a pairArgs, images bool, key string) error {
	return putJSONObject(ctx, a.destClient, a.pair.Dest.Bucket, checkpointKey(images), sourceCheckpoint{
		Version: 1, SourceBucket: a.pair.Source.Bucket, SourceEndpoint: a.pair.Source.Endpoint,
		SourcePrefix: a.pair.Source.Prefix, LastSourceKey: key,
	})
}

func clearSourceCheckpoint(ctx context.Context, a pairArgs, images bool) error {
	_, err := a.destClient.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(a.pair.Dest.Bucket), Key: aws.String(checkpointKey(images)),
	})
	if IsNotFound(err) {
		return nil
	}
	return err
}

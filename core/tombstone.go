package core

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const (
	tombstonePrefix = "errors/"
	tombstoneSuffix = ".json"
)

// ErrorTombstone records a transcode failure for a specific source version, so
// the originating app can surface it and cron-driven runs stop retrying a
// deterministically-broken file. Written to errors/<source-key>.json.
type ErrorTombstone struct {
	SourceKey          string `json:"sourceKey"`
	SourceEtag         string `json:"sourceEtag"`
	SourceSize         int64  `json:"sourceSize"`
	SourceLastModified string `json:"sourceLastModified"`
	Error              string `json:"error"`
	FailedAt           string `json:"failedAt"`
	Attempts           int    `json:"attempts"`
	EncoderVersion     string `json:"encoderVersion"`
}

// TombstoneKey is the dest-bucket key for a source key's error tombstone.
func TombstoneKey(sourceKey string) string { return tombstonePrefix + sourceKey + tombstoneSuffix }

// Blocks reports whether this tombstone should suppress reprocessing of the
// current source version: it must match the same ETag+size and have reached the
// attempt ceiling. A changed source (new ETag/size) does not match, so a
// re-uploaded/fixed file is retried.
func (t *ErrorTombstone) Blocks(etag string, size int64, maxAttempts int) bool {
	return t != nil && t.SourceEtag == etag && t.SourceSize == size && t.Attempts >= maxAttempts
}

// ReadTombstone returns the tombstone for a source key, or nil if none exists.
func ReadTombstone(ctx context.Context, client *s3.Client, bucket, sourceKey string) (*ErrorTombstone, error) {
	return getJSONObject[ErrorTombstone](ctx, client, bucket, TombstoneKey(sourceKey))
}

// WriteTombstone writes (overwrites) a source key's tombstone.
func WriteTombstone(ctx context.Context, client *s3.Client, bucket string, t ErrorTombstone) error {
	return putJSONObject(ctx, client, bucket, TombstoneKey(t.SourceKey), t)
}

// DeleteTombstone removes a source key's tombstone. No-op if absent.
func DeleteTombstone(ctx context.Context, client *s3.Client, bucket, sourceKey string) error {
	_, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(TombstoneKey(sourceKey))})
	if err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}

package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// DownloadResult is the outcome of a stream-and-hash download.
type DownloadResult struct {
	SHA256 string
	Bytes  int64
}

// DownloadAndHash streams an S3 object to localPath while computing its SHA-256,
// saving a second full read for the byte-hash dedup check.
func DownloadAndHash(ctx context.Context, client *s3.Client, bucket, key, localPath string) (DownloadResult, error) {
	out, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return DownloadResult{}, err
	}
	defer out.Body.Close()

	f, err := os.Create(localPath)
	if err != nil {
		return DownloadResult{}, err
	}
	defer f.Close()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), out.Body)
	if err != nil {
		return DownloadResult{}, fmt.Errorf("streaming s3://%s/%s: %w", bucket, key, err)
	}
	return DownloadResult{SHA256: hex.EncodeToString(h.Sum(nil)), Bytes: n}, nil
}

package core

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const fingerprintIndexKey = "fingerprints/index.json"

// FingerprintIndexEntry is one row in the perceptual lookup index.
type FingerprintIndexEntry struct {
	ContentID        string  `json:"contentId"`
	IntervalSeconds  float64 `json:"intervalSeconds"`
	HashCount        int     `json:"hashCount"`
	Width            int     `json:"width"`
	Height           int     `json:"height"`
	VideoBitrateKbps *int    `json:"videoBitrateKbps,omitempty"`
	EncodedAt        string  `json:"encodedAt"`
}

// FingerprintIndex is the perceptual lookup index stored at fingerprints/index.json.
type FingerprintIndex struct {
	Version int                     `json:"version"`
	Entries []FingerprintIndexEntry `json:"entries"`
}

// FingerprintKey is the dest-bucket key for a content ID's raw fingerprint blob.
func FingerprintKey(contentID string) string { return "fingerprints/" + contentID + ".bin" }

// ReadIndex returns the fingerprint index, or an empty v1 index if absent.
func ReadIndex(ctx context.Context, client *s3.Client, bucket string) (*FingerprintIndex, error) {
	idx, err := getJSONObject[FingerprintIndex](ctx, client, bucket, fingerprintIndexKey)
	if err != nil {
		return nil, err
	}
	if idx == nil {
		return &FingerprintIndex{Version: 1}, nil
	}
	if idx.Version != 1 {
		return nil, fmt.Errorf("unsupported fingerprint index version: %d", idx.Version)
	}
	return idx, nil
}

// WriteIndex overwrites the fingerprint index.
func WriteIndex(ctx context.Context, client *s3.Client, bucket string, index FingerprintIndex) error {
	return putJSONObject(ctx, client, bucket, fingerprintIndexKey, index)
}

// UpsertIndexEntry inserts or replaces an entry by content ID.
func UpsertIndexEntry(ctx context.Context, client *s3.Client, bucket string, entry FingerprintIndexEntry) error {
	idx, err := ReadIndex(ctx, client, bucket)
	if err != nil {
		return err
	}
	filtered := idx.Entries[:0:0]
	for _, e := range idx.Entries {
		if e.ContentID != entry.ContentID {
			filtered = append(filtered, e)
		}
	}
	filtered = append(filtered, entry)
	return WriteIndex(ctx, client, bucket, FingerprintIndex{Version: 1, Entries: filtered})
}

// RemoveIndexEntry removes an entry by content ID. No-op if absent.
func RemoveIndexEntry(ctx context.Context, client *s3.Client, bucket, contentID string) error {
	idx, err := ReadIndex(ctx, client, bucket)
	if err != nil {
		return err
	}
	filtered := idx.Entries[:0:0]
	for _, e := range idx.Entries {
		if e.ContentID != contentID {
			filtered = append(filtered, e)
		}
	}
	if len(filtered) == len(idx.Entries) {
		return nil
	}
	return WriteIndex(ctx, client, bucket, FingerprintIndex{Version: 1, Entries: filtered})
}

// UploadFingerprint writes a content ID's raw fingerprint blob.
func UploadFingerprint(ctx context.Context, client *s3.Client, bucket, contentID string, blob []byte) error {
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(FingerprintKey(contentID)),
		Body:        bytes.NewReader(blob),
		ContentType: aws.String("application/octet-stream"),
	})
	return err
}

// ReadFingerprint returns a content ID's stored fingerprint, or nil if absent.
func ReadFingerprint(ctx context.Context, client *s3.Client, bucket, contentID string) (*VideoFingerprint, error) {
	out, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(FingerprintKey(contentID))})
	if err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	defer out.Body.Close()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, err
	}
	return DeserializeFingerprint(data)
}

// DeleteFingerprint removes a content ID's fingerprint blob. No-op if absent.
func DeleteFingerprint(ctx context.Context, client *s3.Client, bucket, contentID string) error {
	_, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(FingerprintKey(contentID))})
	if err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}

// PerceptualMatch is the best fingerprint match found at or above threshold.
type PerceptualMatch struct {
	ContentID  string
	Similarity float64
	Entry      FingerprintIndexEntry
}

// FindPerceptualMatch linear-scans the index, pre-filtering by frame-count ratio
// before downloading + comparing each blob. Returns the best match >= threshold.
func FindPerceptualMatch(ctx context.Context, client *s3.Client, bucket string, incoming *VideoFingerprint, threshold float64) (*PerceptualMatch, error) {
	idx, err := ReadIndex(ctx, client, bucket)
	if err != nil {
		return nil, err
	}
	var best *PerceptualMatch
	for _, entry := range idx.Entries {
		lo, hi := entry.HashCount, len(incoming.Hashes)
		if lo > hi {
			lo, hi = hi, lo
		}
		if hi == 0 || float64(lo)/float64(hi) < 0.7 {
			continue
		}
		stored, err := ReadFingerprint(ctx, client, bucket, entry.ContentID)
		if err != nil {
			return nil, err
		}
		if stored == nil {
			continue
		}
		sim := FingerprintSimilarity(incoming, stored)
		if sim >= threshold && (best == nil || sim > best.Similarity) {
			best = &PerceptualMatch{ContentID: entry.ContentID, Similarity: sim, Entry: entry}
		}
	}
	return best, nil
}

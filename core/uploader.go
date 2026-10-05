package core

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// ContentTypeFor returns an HLS-aware content type for a filename.
func ContentTypeFor(filename string) string {
	switch {
	case strings.HasSuffix(filename, ".m3u8"):
		return "application/vnd.apple.mpegurl"
	case strings.HasSuffix(filename, ".m4s"):
		return "video/iso.segment"
	case strings.HasSuffix(filename, ".mp4"):
		return "video/mp4"
	case strings.HasSuffix(filename, ".ts"):
		return "video/mp2t"
	case strings.HasSuffix(filename, ".vtt"):
		return "text/vtt"
	case strings.HasSuffix(filename, ".json"):
		return "application/json"
	default:
		return "application/octet-stream"
	}
}

// uploadTier orders an HLS tree's uploads so a reader never sees a playlist
// before what it references: media first, then each rendition's playlist,
// then master.m3u8. A re-encode in place therefore switches over only when
// the new master lands, and an interrupted upload leaves the old one live.
func uploadTier(rel string) int {
	rel = filepath.ToSlash(rel)
	switch {
	case rel == "master.m3u8":
		return 2
	case strings.HasSuffix(rel, ".m3u8"):
		return 1
	default:
		return 0
	}
}

// UploadDirectory uploads every file under localDir to bucket under keyPrefix,
// preserving relative paths, with HLS-aware content types, in uploadTier
// order (each tier completes before the next starts). Large segments use
// multipart automatically via the upload manager.
func UploadDirectory(ctx context.Context, client *s3.Client, bucket, keyPrefix, localDir string, concurrency int) ([]string, error) {
	var tiers [3][]string
	if err := filepath.WalkDir(localDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, err := filepath.Rel(localDir, path)
			if err != nil {
				return err
			}
			tiers[uploadTier(rel)] = append(tiers[uploadTier(rel)], path)
		}
		return nil
	}); err != nil {
		return nil, err
	}

	var keys []string
	for _, files := range tiers {
		tierKeys, err := uploadFiles(ctx, client, bucket, keyPrefix, localDir, files, concurrency)
		if err != nil {
			return nil, err
		}
		keys = append(keys, tierKeys...)
	}
	return keys, nil
}

// uploadFiles uploads files (paths under localDir) concurrently.
func uploadFiles(ctx context.Context, client *s3.Client, bucket, keyPrefix, localDir string, files []string, concurrency int) ([]string, error) {
	if concurrency <= 0 {
		concurrency = 8
	}
	uploader := manager.NewUploader(client)
	keys := make([]string, len(files))

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		sem      = make(chan struct{}, concurrency)
	)
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
		mu.Unlock()
	}

	for i, path := range files {
		sem <- struct{}{}
		wg.Add(1)
		go func(i int, path string) {
			defer wg.Done()
			defer func() { <-sem }()
			rel, err := filepath.Rel(localDir, path)
			if err != nil {
				fail(err)
				return
			}
			key := keyPrefix + filepath.ToSlash(rel)
			f, err := os.Open(path)
			if err != nil {
				fail(err)
				return
			}
			defer f.Close()
			if _, err := uploader.Upload(ctx, &s3.PutObjectInput{
				Bucket:      aws.String(bucket),
				Key:         aws.String(key),
				Body:        f,
				ContentType: aws.String(ContentTypeFor(rel)),
			}); err != nil {
				fail(fmt.Errorf("upload %s: %w", key, err))
				return
			}
			keys[i] = key
		}(i, path)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return keys, nil
}

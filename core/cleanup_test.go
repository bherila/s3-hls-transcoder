package core

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// cleanupS3Fake is a minimal path-style S3 stand-in covering the calls the
// cleanup pass makes: ListObjectsV2, GetObject, PutObject, DeleteObject and
// DeleteObjects. Objects are keyed by "<bucket>/<key>".
type cleanupS3Fake struct {
	mu      sync.Mutex
	objects map[string][]byte
	deletes []string
}

func newCleanupS3Client(t *testing.T, fake *cleanupS3Fake) *s3.Client {
	t.Helper()
	if fake.objects == nil {
		fake.objects = map[string][]byte{}
	}
	server := httptest.NewServer(http.HandlerFunc(fake.serveHTTP))
	t.Cleanup(server.Close)
	return s3.New(s3.Options{
		BaseEndpoint: aws.String(server.URL),
		Region:       "us-east-1",
		Credentials:  credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		UsePathStyle: true,
	})
}

func (f *cleanupS3Fake) put(bucket, key string, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[bucket+"/"+key] = body
}

func (f *cleanupS3Fake) has(bucket, key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[bucket+"/"+key]
	return ok
}

func (f *cleanupS3Fake) deletedKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]string(nil), f.deletes...)
	sort.Strings(out)
	return out
}

type listContents struct {
	Key          string `xml:"Key"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	LastModified string `xml:"LastModified"`
}

type listBucketResult struct {
	XMLName     xml.Name       `xml:"ListBucketResult"`
	Name        string         `xml:"Name"`
	Prefix      string         `xml:"Prefix"`
	KeyCount    int            `xml:"KeyCount"`
	IsTruncated bool           `xml:"IsTruncated"`
	Contents    []listContents `xml:"Contents"`
}

type deleteRequest struct {
	Objects []struct {
		Key string `xml:"Key"`
	} `xml:"Object"`
}

func (f *cleanupS3Fake) serveHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")
	q := r.URL.Query()

	switch {
	case r.Method == http.MethodGet && key == "" && q.Get("list-type") == "2":
		prefix := q.Get("prefix")
		f.mu.Lock()
		res := listBucketResult{Name: bucket, Prefix: prefix}
		for k, body := range f.objects {
			b, objKey, _ := strings.Cut(k, "/")
			if b != bucket || !strings.HasPrefix(objKey, prefix) {
				continue
			}
			res.Contents = append(res.Contents, listContents{
				Key: objKey, ETag: etagFor(body), Size: int64(len(body)), LastModified: "2026-01-01T00:00:00.000Z",
			})
		}
		f.mu.Unlock()
		sort.Slice(res.Contents, func(i, j int) bool { return res.Contents[i].Key < res.Contents[j].Key })
		res.KeyCount = len(res.Contents)
		w.Header().Set("Content-Type", "application/xml")
		_ = xml.NewEncoder(w).Encode(res)
	case r.Method == http.MethodGet:
		f.mu.Lock()
		body, ok := f.objects[bucket+"/"+key]
		f.mu.Unlock()
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("ETag", etagFor(body))
		_, _ = w.Write(body)
	case r.Method == http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.objects[bucket+"/"+key] = body
		f.mu.Unlock()
		w.Header().Set("ETag", etagFor(body))
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodDelete:
		f.mu.Lock()
		delete(f.objects, bucket+"/"+key)
		f.deletes = append(f.deletes, bucket+"/"+key)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && q.Has("delete"):
		raw, _ := io.ReadAll(r.Body)
		var req deleteRequest
		if err := xml.Unmarshal(raw, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		var deleted []string
		for _, o := range req.Objects {
			delete(f.objects, bucket+"/"+o.Key)
			f.deletes = append(f.deletes, bucket+"/"+o.Key)
			deleted = append(deleted, o.Key)
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/xml")
		fmt.Fprint(w, `<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
		for _, k := range deleted {
			fmt.Fprintf(w, "<Deleted><Key>%s</Key></Deleted>", k)
		}
		fmt.Fprint(w, "</DeleteResult>")
	default:
		http.Error(w, "unsupported", http.StatusMethodNotAllowed)
	}
}

func testMapping(sourceKey, sourceBucket, sourceEndpoint, contentID string) SourceMapping {
	return SourceMapping{
		SourceKey: sourceKey, SourceBucket: sourceBucket, SourceEndpoint: sourceEndpoint,
		SourceEtag: "etag", SourceSize: 1024, SourceLastModified: "2026-01-01T00:00:00Z",
		ContentID: contentID, HLSRoot: MasterPlaylistKey(contentID),
		EncodedAt: "2026-01-01T00:00:00Z", EncoderVersion: "test",
	}
}

func cleanupOptsForPairA(client *s3.Client) CleanupOptions {
	return CleanupOptions{
		SourceClient: client, DestClient: client,
		SourceBucket: "source-a", SourceEndpoint: "https://source-a.example.com",
		DestBucket: "dest", Logger: NewLogger(LevelError),
	}
}

func TestCleanupIgnoresMappingsOwnedByAnotherPair(t *testing.T) {
	fake := &cleanupS3Fake{}
	client := newCleanupS3Client(t, fake)

	// Pair A's source has one live file; the shared dest holds a mapping that
	// pair B wrote and whose source key does not exist in pair A's bucket.
	fake.put("source-a", "a/live.mp4", map[string]string{"data": "x"})
	fake.put("dest", MappingKey("b/live-in-pair-B.mp4"),
		testMapping("b/live-in-pair-B.mp4", "source-b", "https://source-b.example.com", "sha256:pair-b-content"))
	fake.put("dest", MasterPlaylistKey("sha256:pair-b-content"), map[string]string{"m3u8": "x"})

	res, err := RunCleanupPass(context.Background(), cleanupOptsForPairA(client))
	if err != nil {
		t.Fatalf("RunCleanupPass: %v", err)
	}
	if res != (CleanupResult{}) {
		t.Errorf("expected no cleanup activity, got %+v", res)
	}
	if got := fake.deletedKeys(); len(got) != 0 {
		t.Errorf("expected no deletes, got %v", got)
	}
	if !fake.has("dest", MappingKey("b/live-in-pair-B.mp4")) {
		t.Error("pair B's mapping was deleted")
	}
}

func TestCleanupIgnoresLegacyMappingsWithoutOwnership(t *testing.T) {
	fake := &cleanupS3Fake{}
	client := newCleanupS3Client(t, fake)

	fake.put("dest", MappingKey("a/old.mp4"), testMapping("a/old.mp4", "", "", "sha256:legacy"))

	res, err := RunCleanupPass(context.Background(), cleanupOptsForPairA(client))
	if err != nil {
		t.Fatalf("RunCleanupPass: %v", err)
	}
	if res.OrphanMappingsFound != 0 {
		t.Errorf("legacy mapping without ownership treated as orphan: %+v", res)
	}
	if !fake.has("dest", MappingKey("a/old.mp4")) {
		t.Error("legacy mapping was deleted")
	}
}

func TestCleanupDeletesOrphansOwnedByCurrentPair(t *testing.T) {
	fake := &cleanupS3Fake{}
	client := newCleanupS3Client(t, fake)

	const contentID = "sha256:pair-a-content"
	fake.put("dest", MappingKey("a/deleted.mp4"),
		testMapping("a/deleted.mp4", "source-a", "https://source-a.example.com", contentID))
	fake.put("dest", MasterPlaylistKey(contentID), map[string]string{"m3u8": "x"})
	fake.put("dest", FingerprintKey(contentID), map[string]string{"fp": "x"})
	fake.put("dest", fingerprintIndexKey, FingerprintIndex{Version: 1, Entries: []FingerprintIndexEntry{{ContentID: contentID}}})

	res, err := RunCleanupPass(context.Background(), cleanupOptsForPairA(client))
	if err != nil {
		t.Fatalf("RunCleanupPass: %v", err)
	}
	if res.OrphanMappingsFound != 1 || res.OrphanMappingsDeleted != 1 || res.ContentIDsGCd != 1 {
		t.Errorf("unexpected result: %+v", res)
	}
	if fake.has("dest", MappingKey("a/deleted.mp4")) {
		t.Error("orphan mapping was not deleted")
	}
	if fake.has("dest", MasterPlaylistKey(contentID)) {
		t.Error("orphaned by-id output was not garbage-collected")
	}
	if fake.has("dest", FingerprintKey(contentID)) {
		t.Error("orphaned fingerprint was not deleted")
	}
}

func testImageMapping(sourceKey, sourceBucket, sourceEndpoint string) ImageMapping {
	return ImageMapping{
		SourceKey: sourceKey, SourceBucket: sourceBucket, SourceEndpoint: sourceEndpoint,
		SourceEtag: "etag", SourceSize: 512, SourceLastModified: "2026-01-01T00:00:00Z",
		PDQHash: strings.Repeat("a", 64), Quality: 100,
		HashedAt: "2026-01-01T00:00:00Z", HasherVersion: "test",
	}
}

func TestImageCleanupIgnoresMappingsOwnedByAnotherPair(t *testing.T) {
	fake := &cleanupS3Fake{}
	client := newCleanupS3Client(t, fake)

	// The shared dest holds a mapping pair B wrote, for a key absent from pair A's bucket.
	fake.put("source-a", "a/live.jpg", map[string]string{"data": "x"})
	fake.put("dest", ImageMappingKey("b/live-in-pair-B.jpg"),
		testImageMapping("b/live-in-pair-B.jpg", "source-b", "https://source-b.example.com"))

	res, err := RunImageCleanupPass(context.Background(), cleanupOptsForPairA(client))
	if err != nil {
		t.Fatalf("RunImageCleanupPass: %v", err)
	}
	if res.OrphanMappingsFound != 0 || res.OrphanMappingsDeleted != 0 {
		t.Errorf("expected no cleanup activity, got %+v", res)
	}
	if !fake.has("dest", ImageMappingKey("b/live-in-pair-B.jpg")) {
		t.Error("pair B's image mapping was deleted")
	}
}

func TestImageCleanupIgnoresLegacyMappingsWithoutOwnership(t *testing.T) {
	fake := &cleanupS3Fake{}
	client := newCleanupS3Client(t, fake)

	fake.put("dest", ImageMappingKey("a/old.jpg"), testImageMapping("a/old.jpg", "", ""))

	res, err := RunImageCleanupPass(context.Background(), cleanupOptsForPairA(client))
	if err != nil {
		t.Fatalf("RunImageCleanupPass: %v", err)
	}
	if res.OrphanMappingsFound != 0 {
		t.Errorf("legacy image mapping treated as orphan: %+v", res)
	}
	if !fake.has("dest", ImageMappingKey("a/old.jpg")) {
		t.Error("legacy image mapping was deleted")
	}
}

func TestImageCleanupDeletesOrphansOwnedByCurrentPair(t *testing.T) {
	fake := &cleanupS3Fake{}
	client := newCleanupS3Client(t, fake)

	fake.put("dest", ImageMappingKey("a/deleted.jpg"),
		testImageMapping("a/deleted.jpg", "source-a", "https://source-a.example.com"))

	res, err := RunImageCleanupPass(context.Background(), cleanupOptsForPairA(client))
	if err != nil {
		t.Fatalf("RunImageCleanupPass: %v", err)
	}
	if res.OrphanMappingsFound != 1 || res.OrphanMappingsDeleted != 1 {
		t.Errorf("unexpected result: %+v", res)
	}
	if fake.has("dest", ImageMappingKey("a/deleted.jpg")) {
		t.Error("orphan image mapping was not deleted")
	}
}

func TestImageCleanupDryRunDeletesNothing(t *testing.T) {
	fake := &cleanupS3Fake{}
	client := newCleanupS3Client(t, fake)

	fake.put("dest", ImageMappingKey("a/deleted.jpg"),
		testImageMapping("a/deleted.jpg", "source-a", "https://source-a.example.com"))

	opts := cleanupOptsForPairA(client)
	opts.DryRun = true
	res, err := RunImageCleanupPass(context.Background(), opts)
	if err != nil {
		t.Fatalf("RunImageCleanupPass: %v", err)
	}
	if res.OrphanMappingsFound != 1 || res.OrphanMappingsDeleted != 1 {
		t.Errorf("dry run should still report the orphan: %+v", res)
	}
	if !fake.has("dest", ImageMappingKey("a/deleted.jpg")) {
		t.Error("dry run deleted the mapping")
	}
}

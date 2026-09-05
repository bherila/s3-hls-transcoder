package core

import (
	"context"
	"fmt"
	"testing"
)

const refsContentID = "sha256:refs-content"

func TestAddRefCreatesAndAppends(t *testing.T) {
	fake := &cleanupS3Fake{}
	client := newCleanupS3Client(t, fake)
	ctx := context.Background()

	if err := AddRef(ctx, client, "dest", refsContentID, "a/one.mp4"); err != nil {
		t.Fatalf("AddRef: %v", err)
	}
	if err := AddRef(ctx, client, "dest", refsContentID, "a/two.mp4"); err != nil {
		t.Fatalf("AddRef: %v", err)
	}
	// Re-adding an existing reference must not duplicate it.
	if err := AddRef(ctx, client, "dest", refsContentID, "a/one.mp4"); err != nil {
		t.Fatalf("AddRef: %v", err)
	}

	refs, err := ReadRefs(ctx, client, "dest", refsContentID)
	if err != nil {
		t.Fatalf("ReadRefs: %v", err)
	}
	if refs == nil {
		t.Fatal("expected a reverse index to exist")
	}
	if got, want := fmt.Sprint(refs.SourceKeys), "[a/one.mp4 a/two.mp4]"; got != want {
		t.Errorf("source keys = %s, want %s", got, want)
	}
	if refs.Version != 1 || refs.ContentID != refsContentID {
		t.Errorf("unexpected index header: %+v", refs)
	}
}

func TestRemoveRef(t *testing.T) {
	fake := &cleanupS3Fake{}
	client := newCleanupS3Client(t, fake)
	ctx := context.Background()

	// Removing from a missing index is a no-op, not an error.
	if err := RemoveRef(ctx, client, "dest", refsContentID, "a/one.mp4"); err != nil {
		t.Fatalf("RemoveRef on missing index: %v", err)
	}
	if fake.has("dest", RefsKey(refsContentID)) {
		t.Error("RemoveRef created an index for content that had none")
	}

	for _, k := range []string{"a/one.mp4", "a/two.mp4"} {
		if err := AddRef(ctx, client, "dest", refsContentID, k); err != nil {
			t.Fatalf("AddRef: %v", err)
		}
	}
	if err := RemoveRef(ctx, client, "dest", refsContentID, "a/one.mp4"); err != nil {
		t.Fatalf("RemoveRef: %v", err)
	}

	refs, err := ReadRefs(ctx, client, "dest", refsContentID)
	if err != nil {
		t.Fatalf("ReadRefs: %v", err)
	}
	if got, want := fmt.Sprint(refs.SourceKeys), "[a/two.mp4]"; got != want {
		t.Errorf("source keys = %s, want %s", got, want)
	}
}

func TestListRefsBackfillsFromMappingScan(t *testing.T) {
	fake := &cleanupS3Fake{}
	client := newCleanupS3Client(t, fake)
	ctx := context.Background()

	// Content written before the reverse index existed: mappings only.
	fake.put("dest", MappingKey("a/legacy.mp4"),
		testMapping("a/legacy.mp4", "source-a", "https://source-a.example.com", refsContentID))
	fake.put("dest", MappingKey("a/other.mp4"),
		testMapping("a/other.mp4", "source-a", "https://source-a.example.com", "sha256:unrelated"))

	keys, err := ListRefs(ctx, client, "dest", refsContentID, true)
	if err != nil {
		t.Fatalf("ListRefs: %v", err)
	}
	if got, want := fmt.Sprint(keys), "[a/legacy.mp4]"; got != want {
		t.Errorf("keys = %s, want %s", got, want)
	}
	if !fake.has("dest", RefsKey(refsContentID)) {
		t.Fatal("scan result was not backfilled into the reverse index")
	}

	// The backfilled index answers the next call without another scan.
	before := fake.getCount("dest", MappingKey("a/other.mp4"))
	if _, err := ListRefs(ctx, client, "dest", refsContentID, true); err != nil {
		t.Fatalf("ListRefs: %v", err)
	}
	if after := fake.getCount("dest", MappingKey("a/other.mp4")); after != before {
		t.Errorf("unrelated mapping was read again: %d → %d", before, after)
	}
}

func TestListRefsDoesNotPersistWhenAskedNotTo(t *testing.T) {
	fake := &cleanupS3Fake{}
	client := newCleanupS3Client(t, fake)

	fake.put("dest", MappingKey("a/legacy.mp4"),
		testMapping("a/legacy.mp4", "source-a", "https://source-a.example.com", refsContentID))

	keys, err := ListRefs(context.Background(), client, "dest", refsContentID, false)
	if err != nil {
		t.Fatalf("ListRefs: %v", err)
	}
	if got, want := fmt.Sprint(keys), "[a/legacy.mp4]"; got != want {
		t.Errorf("keys = %s, want %s", got, want)
	}
	if fake.has("dest", RefsKey(refsContentID)) {
		t.Error("dry-run backfill wrote an index")
	}
}

func TestCleanupUsesReverseIndexInsteadOfScanningAllMappings(t *testing.T) {
	fake := &cleanupS3Fake{}
	client := newCleanupS3Client(t, fake)

	const orphanContent = "sha256:orphan-content"
	fake.put("dest", MappingKey("a/deleted.mp4"),
		testMapping("a/deleted.mp4", "source-a", "https://source-a.example.com", orphanContent))
	fake.put("dest", RefsKey(orphanContent), ContentRefs{
		Version: 1, ContentID: orphanContent, SourceKeys: []string{"a/deleted.mp4"},
	})
	fake.put("dest", MasterPlaylistKey(orphanContent), map[string]string{"m3u8": "x"})

	// Unrelated live content that a whole-mappings scan would have read.
	var untouched []string
	for i := range 20 {
		key := fmt.Sprintf("a/live-%02d.mp4", i)
		fake.put("source-a", key, map[string]string{"data": "x"})
		fake.put("dest", MappingKey(key),
			testMapping(key, "source-a", "https://source-a.example.com", fmt.Sprintf("sha256:live-%02d", i)))
		untouched = append(untouched, MappingKey(key))
	}

	res, err := RunCleanupPass(context.Background(), cleanupOptsForPairA(client))
	if err != nil {
		t.Fatalf("RunCleanupPass: %v", err)
	}
	if res.ContentIDsGCd != 1 || res.OrphanMappingsDeleted != 1 {
		t.Errorf("unexpected result: %+v", res)
	}
	for _, k := range untouched {
		if n := fake.getCount("dest", k); n != 0 {
			t.Errorf("mapping %s was read %d times; the reverse index should have made the scan unnecessary", k, n)
		}
	}
}

func TestCleanupRetainsContentWithALiveReferenceAndPrunesTheOrphan(t *testing.T) {
	fake := &cleanupS3Fake{}
	client := newCleanupS3Client(t, fake)

	const shared = "sha256:shared-content"
	// Two sources deduped onto one content ID; only one still exists.
	fake.put("source-a", "a/live.mp4", map[string]string{"data": "x"})
	fake.put("dest", MappingKey("a/live.mp4"),
		testMapping("a/live.mp4", "source-a", "https://source-a.example.com", shared))
	fake.put("dest", MappingKey("a/deleted.mp4"),
		testMapping("a/deleted.mp4", "source-a", "https://source-a.example.com", shared))
	fake.put("dest", RefsKey(shared), ContentRefs{
		Version: 1, ContentID: shared, SourceKeys: []string{"a/deleted.mp4", "a/live.mp4"},
	})
	fake.put("dest", MasterPlaylistKey(shared), map[string]string{"m3u8": "x"})

	res, err := RunCleanupPass(context.Background(), cleanupOptsForPairA(client))
	if err != nil {
		t.Fatalf("RunCleanupPass: %v", err)
	}
	if res.ContentIDsRetained != 1 || res.ContentIDsGCd != 0 {
		t.Errorf("unexpected result: %+v", res)
	}
	if !fake.has("dest", MasterPlaylistKey(shared)) {
		t.Error("content with a live reference was garbage-collected")
	}

	refs, err := ReadRefs(context.Background(), client, "dest", shared)
	if err != nil {
		t.Fatalf("ReadRefs: %v", err)
	}
	if got, want := fmt.Sprint(refs.SourceKeys), "[a/live.mp4]"; got != want {
		t.Errorf("index after cleanup = %s, want %s", got, want)
	}
}

func TestCleanupIgnoresStaleReverseIndexEntries(t *testing.T) {
	fake := &cleanupS3Fake{}
	client := newCleanupS3Client(t, fake)

	const contentID = "sha256:stale-ref-content"
	fake.put("dest", MappingKey("a/deleted.mp4"),
		testMapping("a/deleted.mp4", "source-a", "https://source-a.example.com", contentID))
	// "a/vanished.mp4" has no mapping object at all: a reference left behind by
	// an interrupted run. It must not pin the content forever.
	fake.put("dest", RefsKey(contentID), ContentRefs{
		Version: 1, ContentID: contentID, SourceKeys: []string{"a/deleted.mp4", "a/vanished.mp4"},
	})
	fake.put("dest", MasterPlaylistKey(contentID), map[string]string{"m3u8": "x"})

	res, err := RunCleanupPass(context.Background(), cleanupOptsForPairA(client))
	if err != nil {
		t.Fatalf("RunCleanupPass: %v", err)
	}
	if res.ContentIDsGCd != 1 {
		t.Errorf("stale reference blocked GC: %+v", res)
	}
	if fake.has("dest", MasterPlaylistKey(contentID)) {
		t.Error("fully orphaned content was not garbage-collected")
	}
}

func TestCleanupDoesNotCountAnotherPairsMappingAsStale(t *testing.T) {
	fake := &cleanupS3Fake{}
	client := newCleanupS3Client(t, fake)

	const shared = "sha256:cross-pair-content"
	// Pair B's source still holds the file; pair A's copy was deleted. The
	// reverse index is bucket-global, so pair B's live mapping must keep the
	// content alive even though pair A is the one running cleanup.
	fake.put("dest", MappingKey("a/deleted.mp4"),
		testMapping("a/deleted.mp4", "source-a", "https://source-a.example.com", shared))
	fake.put("dest", MappingKey("b/live.mp4"),
		testMapping("b/live.mp4", "source-b", "https://source-b.example.com", shared))
	fake.put("dest", RefsKey(shared), ContentRefs{
		Version: 1, ContentID: shared, SourceKeys: []string{"a/deleted.mp4", "b/live.mp4"},
	})
	fake.put("dest", MasterPlaylistKey(shared), map[string]string{"m3u8": "x"})

	res, err := RunCleanupPass(context.Background(), cleanupOptsForPairA(client))
	if err != nil {
		t.Fatalf("RunCleanupPass: %v", err)
	}
	if res.ContentIDsRetained != 1 || res.ContentIDsGCd != 0 {
		t.Errorf("unexpected result: %+v", res)
	}
	if !fake.has("dest", MasterPlaylistKey(shared)) {
		t.Error("content referenced by another pair was garbage-collected")
	}
	if !fake.has("dest", MappingKey("b/live.mp4")) {
		t.Error("another pair's mapping was deleted")
	}
}

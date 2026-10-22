package core

import (
	"context"
	"fmt"
	"slices"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const refsPrefix = "refs/"
const maxIndexedRefs = 1000

// ContentRefs is the reverse index for one content ID: the source keys whose
// mapping currently points at it. It lives at refs/<contentID>.json, alongside
// the other bookkeeping prefixes and deliberately outside by-id/: that prefix
// is served to players, and the index names source keys — which are private,
// and shared across every source that deduped onto the same content. Keeping
// it out also leaves by-id/ genuinely immutable for caching.
//
// Up to maxIndexedRefs entries make refcount decisions cost one GET per content
// ID instead of a full mappings/ scan. More highly shared content is marked as
// overflow so cleanup can safely scan mappings without growing this object.
type ContentRefs struct {
	Version    int      `json:"version"`
	ContentID  string   `json:"contentId"`
	SourceKeys []string `json:"sourceKeys"`
	Overflow   bool     `json:"overflow,omitempty"`
	UpdatedAt  string   `json:"updatedAt"`
}

// RefsKey is the dest-bucket key of a content ID's reverse index.
func RefsKey(contentID string) string { return refsPrefix + contentID + ".json" }

// DeleteRefs removes a content ID's reverse index. No-op if absent. Called by
// cleanup when the content is GC'd; the index no longer lives inside the
// by-id/ tree, so it is not removed with it.
func DeleteRefs(ctx context.Context, client *s3.Client, bucket, contentID string) error {
	_, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(RefsKey(contentID)),
	})
	if err != nil && !IsNotFound(err) {
		return err
	}
	return nil
}

// ReadRefs returns a content ID's reverse index, or nil if it has none.
func ReadRefs(ctx context.Context, client *s3.Client, bucket, contentID string) (*ContentRefs, error) {
	refs, err := getJSONObject[ContentRefs](ctx, client, bucket, RefsKey(contentID))
	if err != nil {
		return nil, err
	}
	if refs == nil {
		return nil, nil
	}
	if refs.Version != 1 && refs.Version != 2 {
		return nil, fmt.Errorf("unsupported refs version %d for %s", refs.Version, contentID)
	}
	return refs, nil
}

// WriteRefs overwrites a content ID's reverse index with a bounded, sorted,
// de-duplicated set. Overflow tells readers to use the authoritative mappings.
func WriteRefs(ctx context.Context, client *s3.Client, bucket, contentID string, sourceKeys []string) error {
	seen := make(map[string]struct{}, min(len(sourceKeys), maxIndexedRefs+1))
	keys := make([]string, 0, min(len(sourceKeys), maxIndexedRefs))
	overflow := false
	for _, key := range sourceKeys {
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if len(keys) == maxIndexedRefs {
			overflow = true
			break
		}
		keys = append(keys, key)
	}
	slices.Sort(keys)
	version := 1
	if overflow {
		// Older v1-only readers must reject incomplete indexes instead of
		// mistaking the bounded sample for every live reference.
		version = 2
	}
	return putJSONObject(ctx, client, bucket, RefsKey(contentID), ContentRefs{
		Version: version, ContentID: contentID, SourceKeys: keys, Overflow: overflow, UpdatedAt: nowISO(),
	})
}

// AddRef records that sourceKey's mapping points at contentID.
//
// Call it *before* writing the mapping: the index must never be missing a live
// reference (cleanup would GC content that is still in use), while an extra
// reference is harmless because cleanup re-checks each one against the mapping
// it names and prunes the ones that no longer hold.
//
// seedFromScan controls what happens when no index exists yet. Pass true when
// the content pre-dates this write (a dedup hit): its existing references are
// recovered by scan first, because seeding the index with only the incoming key
// would hide mappings written before the index existed and cleanup would then
// GC content they still point at. Pass false for content transcoded in this
// pass, where no mapping can reference it yet — scanning there would put an
// O(N) whole-bucket read on the ingest path, which is the cost this index
// exists to avoid.
func AddRef(ctx context.Context, client *s3.Client, bucket, contentID, sourceKey string, seedFromScan bool) error {
	refs, err := ReadRefs(ctx, client, bucket, contentID)
	if err != nil {
		return err
	}
	var keys []string
	if refs != nil {
		// Once the bounded index overflows, cleanup deliberately falls back to a
		// mappings scan. Do not repeatedly read, copy, and rewrite an ever-growing
		// attacker-controlled list on each dedup hit.
		if refs.Overflow {
			return nil
		}
		if slices.Contains(refs.SourceKeys, sourceKey) {
			return nil
		}
		keys = refs.SourceKeys
	} else if seedFromScan {
		if keys, err = FindMappingsForContentID(ctx, client, bucket, contentID); err != nil {
			return err
		}
	}
	return WriteRefs(ctx, client, bucket, contentID, append(keys, sourceKey))
}

// RemoveRef drops sourceKey from a content ID's reverse index. No-op when the
// index is absent or does not name the key.
func RemoveRef(ctx context.Context, client *s3.Client, bucket, contentID, sourceKey string) error {
	refs, err := ReadRefs(ctx, client, bucket, contentID)
	if err != nil || refs == nil {
		return err
	}
	if refs.Overflow {
		// The stored keys are only a sample. Removing from it could incorrectly
		// make the index look complete; cleanup will rebuild it from mappings.
		return nil
	}
	remaining := slices.DeleteFunc(append([]string(nil), refs.SourceKeys...), func(k string) bool {
		return k == sourceKey
	})
	if len(remaining) == len(refs.SourceKeys) {
		return nil
	}
	return WriteRefs(ctx, client, bucket, contentID, remaining)
}

// ListRefs returns the source keys that reference contentID. When no index
// exists — content transcoded before the reverse index was introduced, or an
// index deleted out of band — it falls back to the O(N) mappings/ scan and,
// when persist is true, backfills the index so later passes are O(1).
func ListRefs(ctx context.Context, client *s3.Client, bucket, contentID string, persist bool) ([]string, error) {
	refs, err := ReadRefs(ctx, client, bucket, contentID)
	if err != nil {
		return nil, err
	}
	if refs != nil && !refs.Overflow {
		return refs.SourceKeys, nil
	}
	keys, err := FindMappingsForContentID(ctx, client, bucket, contentID)
	if err != nil {
		return nil, err
	}
	if persist {
		if err := WriteRefs(ctx, client, bucket, contentID, keys); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

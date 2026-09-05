package core

import (
	"context"
	"fmt"
	"slices"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const refsObjectName = "refs.json"

// ContentRefs is the reverse index for one content ID: the source keys whose
// mapping currently points at it. It lives at by-id/<contentID>/refs.json so
// that GC-ing the content directory disposes of the index with it.
//
// The index exists so that refcount decisions cost one GET per content ID
// instead of a full mappings/ scan (see FindMappingsForContentID).
type ContentRefs struct {
	Version    int      `json:"version"`
	ContentID  string   `json:"contentId"`
	SourceKeys []string `json:"sourceKeys"`
	UpdatedAt  string   `json:"updatedAt"`
}

// RefsKey is the dest-bucket key of a content ID's reverse index.
func RefsKey(contentID string) string { return ByIDPrefix(contentID) + refsObjectName }

// ReadRefs returns a content ID's reverse index, or nil if it has none.
func ReadRefs(ctx context.Context, client *s3.Client, bucket, contentID string) (*ContentRefs, error) {
	refs, err := getJSONObject[ContentRefs](ctx, client, bucket, RefsKey(contentID))
	if err != nil {
		return nil, err
	}
	if refs == nil {
		return nil, nil
	}
	if refs.Version != 1 {
		return nil, fmt.Errorf("unsupported refs version %d for %s", refs.Version, contentID)
	}
	return refs, nil
}

// WriteRefs overwrites a content ID's reverse index with sourceKeys, sorted and
// de-duplicated.
func WriteRefs(ctx context.Context, client *s3.Client, bucket, contentID string, sourceKeys []string) error {
	keys := append([]string(nil), sourceKeys...)
	slices.Sort(keys)
	keys = slices.Compact(keys)
	return putJSONObject(ctx, client, bucket, RefsKey(contentID), ContentRefs{
		Version: 1, ContentID: contentID, SourceKeys: keys, UpdatedAt: nowISO(),
	})
}

// AddRef records that sourceKey's mapping points at contentID.
//
// Call it *before* writing the mapping: the index must never be missing a live
// reference (cleanup would GC content that is still in use), while an extra
// reference is harmless because cleanup re-checks each one against the mapping
// it names and prunes the ones that no longer hold.
func AddRef(ctx context.Context, client *s3.Client, bucket, contentID, sourceKey string) error {
	refs, err := ReadRefs(ctx, client, bucket, contentID)
	if err != nil {
		return err
	}
	var keys []string
	if refs != nil {
		if slices.Contains(refs.SourceKeys, sourceKey) {
			return nil
		}
		keys = refs.SourceKeys
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
	if refs != nil {
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

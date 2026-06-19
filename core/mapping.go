package core

import (
	"context"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const (
	mappingPrefix = "mappings/"
	mappingSuffix = ".json"
)

// SourceMapping points a source key at its transcoded output (client contract).
type SourceMapping struct {
	SourceKey          string `json:"sourceKey"`
	SourceEtag         string `json:"sourceEtag"`
	SourceSize         int64  `json:"sourceSize"`
	SourceLastModified string `json:"sourceLastModified"`
	ContentID          string `json:"contentId"`
	HLSRoot            string `json:"hlsRoot"`
	EncodedAt          string `json:"encodedAt"`
	EncoderVersion     string `json:"encoderVersion"`
}

// MappingKey is the dest-bucket key for a source key's mapping.
func MappingKey(sourceKey string) string { return mappingPrefix + sourceKey + mappingSuffix }

func sourceKeyFromMappingKey(key string) (string, bool) {
	if !strings.HasPrefix(key, mappingPrefix) || !strings.HasSuffix(key, mappingSuffix) {
		return "", false
	}
	return key[len(mappingPrefix) : len(key)-len(mappingSuffix)], true
}

// ReadMapping returns the mapping for a source key, or nil if none exists.
func ReadMapping(ctx context.Context, client *s3.Client, bucket, sourceKey string) (*SourceMapping, error) {
	return getJSONObject[SourceMapping](ctx, client, bucket, MappingKey(sourceKey))
}

// WriteMapping writes (overwrites) a source key's mapping.
func WriteMapping(ctx context.Context, client *s3.Client, bucket string, m SourceMapping) error {
	return putJSONObject(ctx, client, bucket, MappingKey(m.SourceKey), m)
}

// IsCachedMapping reports whether an existing mapping already covers the current
// source object (ETag + size match).
func IsCachedMapping(m *SourceMapping, etag string, size int64) bool {
	return m != nil && m.SourceEtag == etag && m.SourceSize == size
}

// FindMappingsForContentID returns source keys whose mapping currently points at
// contentID. O(N) GETs over the mappings/ prefix.
func FindMappingsForContentID(ctx context.Context, client *s3.Client, bucket, contentID string) ([]string, error) {
	var result []string
	var token *string
	for {
		out, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket: aws.String(bucket), Prefix: aws.String(mappingPrefix), ContinuationToken: token,
		})
		if err != nil {
			return nil, err
		}
		for _, o := range out.Contents {
			if o.Key == nil {
				continue
			}
			sourceKey, ok := sourceKeyFromMappingKey(*o.Key)
			if !ok {
				continue
			}
			m, err := ReadMapping(ctx, client, bucket, sourceKey)
			if err != nil {
				return nil, err
			}
			if m != nil && m.ContentID == contentID {
				result = append(result, sourceKey)
			}
		}
		if out.IsTruncated != nil && *out.IsTruncated && out.NextContinuationToken != nil {
			token = out.NextContinuationToken
		} else {
			return result, nil
		}
	}
}

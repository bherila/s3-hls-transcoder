package core

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// MetadataSource captures the probed source characteristics.
type MetadataSource struct {
	Width           int     `json:"width"`
	Height          int     `json:"height"`
	DurationSeconds float64 `json:"durationSeconds"`
	BitrateKbps     *int    `json:"bitrateKbps,omitempty"`
}

// OutputMetadata is written to by-id/<contentID>/metadata.json.
type OutputMetadata struct {
	ContentID      string         `json:"contentId"`
	EncoderVersion string         `json:"encoderVersion"`
	EncodedAt      string         `json:"encodedAt"`
	Source         MetadataSource `json:"source"`
	Ladder         []LadderRung   `json:"ladder"`
}

// ReadMetadata returns a content ID's metadata, or nil if none exists.
func ReadMetadata(ctx context.Context, client *s3.Client, bucket, contentID string) (*OutputMetadata, error) {
	return getJSONObject[OutputMetadata](ctx, client, bucket, MetadataKey(contentID))
}

// WriteMetadata writes a content ID's metadata.json.
func WriteMetadata(ctx context.Context, client *s3.Client, bucket string, m OutputMetadata) error {
	return putJSONObject(ctx, client, bucket, MetadataKey(m.ContentID), m)
}

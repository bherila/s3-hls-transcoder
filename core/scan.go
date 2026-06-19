package core

import (
	"context"
	"iter"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// ScanSource lists the source bucket with pagination, yielding objects in
// S3 listing order. Stop early by returning false from the range body. A
// listing error is yielded once with a zero SourceObject, then iteration ends.
func ScanSource(ctx context.Context, client *s3.Client, bucket string, opts ScanOptions) iter.Seq2[SourceObject, error] {
	return func(yield func(SourceObject, error) bool) {
		filter := opts.Filter
		if filter == nil {
			filter = IsVideoKey
		}
		var token *string
		for {
			in := &s3.ListObjectsV2Input{Bucket: aws.String(bucket), ContinuationToken: token}
			if opts.Prefix != "" {
				in.Prefix = aws.String(opts.Prefix)
			}
			out, err := client.ListObjectsV2(ctx, in)
			if err != nil {
				yield(SourceObject{}, err)
				return
			}
			for _, o := range out.Contents {
				if o.Key == nil || o.ETag == nil || o.Size == nil || o.LastModified == nil {
					continue
				}
				key := *o.Key
				if strings.HasSuffix(key, "/") && *o.Size == 0 {
					continue
				}
				if !filter(key) {
					continue
				}
				obj := SourceObject{
					Key:          key,
					ETag:         strings.Trim(*o.ETag, `"`),
					Size:         *o.Size,
					LastModified: *o.LastModified,
				}
				if !yield(obj, nil) {
					return
				}
			}
			if out.IsTruncated != nil && *out.IsTruncated && out.NextContinuationToken != nil {
				token = out.NextContinuationToken
			} else {
				return
			}
		}
	}
}

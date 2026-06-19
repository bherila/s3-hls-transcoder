package core

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// TranscodedOutputExists reports whether a content ID's master playlist exists.
func TranscodedOutputExists(ctx context.Context, client *s3.Client, bucket, contentID string) (bool, error) {
	_, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(MasterPlaylistKey(contentID))})
	if err != nil {
		if IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// DeleteByIDDirectory removes everything under by-id/<contentID>/, returning the
// number of objects deleted.
func DeleteByIDDirectory(ctx context.Context, client *s3.Client, bucket, contentID string) (int, error) {
	prefix := ByIDPrefix(contentID)
	deleted := 0
	var token *string
	for {
		out, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket: aws.String(bucket), Prefix: aws.String(prefix), ContinuationToken: token,
		})
		if err != nil {
			return deleted, err
		}
		var ids []types.ObjectIdentifier
		for _, o := range out.Contents {
			if o.Key != nil {
				ids = append(ids, types.ObjectIdentifier{Key: o.Key})
			}
		}
		// DeleteObjects caps at 1000 keys per call.
		for i := 0; i < len(ids); i += 1000 {
			end := i + 1000
			if end > len(ids) {
				end = len(ids)
			}
			batch := ids[i:end]
			if _, err := client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
				Bucket: aws.String(bucket),
				Delete: &types.Delete{Objects: batch},
			}); err != nil {
				return deleted, err
			}
			deleted += len(batch)
		}
		if out.IsTruncated != nil && *out.IsTruncated && out.NextContinuationToken != nil {
			token = out.NextContinuationToken
		} else {
			return deleted, nil
		}
	}
}

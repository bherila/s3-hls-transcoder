package core

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// getJSONObject GETs and decodes a JSON object. A 404 returns (nil, nil).
func getJSONObject[T any](ctx context.Context, client *s3.Client, bucket, key string) (*T, error) {
	out, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	defer out.Body.Close()
	var v T
	if err := json.NewDecoder(out.Body).Decode(&v); err != nil {
		return nil, err
	}
	return &v, nil
}

// putJSONObject PUTs a value as pretty-printed JSON with the JSON content type.
func putJSONObject(ctx context.Context, client *s3.Client, bucket, key string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/json"),
	})
	return err
}

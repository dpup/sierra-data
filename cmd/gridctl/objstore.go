package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// objectStore is the slice of S3 gridctl uses: whole-file put and get. The
// real client is built only when a location is s3://, so local runs and
// tests never need AWS credentials (tests swap newObjectStore for a fake).
type objectStore interface {
	Put(ctx context.Context, bucket, key, path string) error
	Get(ctx context.Context, bucket, key, path string) error
}

// newObjectStore builds the S3 client from the default credential chain
// (the ECS task role in a one-off task). Replaced in tests.
var newObjectStore = func(ctx context.Context) (objectStore, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("gridctl: loading AWS configuration: %w", err)
	}
	return &s3Store{client: s3.NewFromConfig(cfg)}, nil
}

// location is a local path or an s3://bucket/key URL.
type location struct {
	raw         string
	bucket, key string // set for s3://
}

func (l location) isS3() bool     { return l.bucket != "" }
func (l location) String() string { return l.raw }

func parseLocation(raw string) (location, error) {
	if raw == "" {
		return location{}, errors.New("empty location")
	}
	rest, ok := strings.CutPrefix(raw, "s3://")
	if !ok {
		return location{raw: raw}, nil
	}
	bucket, key, _ := strings.Cut(rest, "/")
	if bucket == "" || key == "" || strings.HasSuffix(key, "/") {
		return location{}, fmt.Errorf("%q: want s3://bucket/key", raw)
	}
	return location{raw: raw, bucket: bucket, key: key}, nil
}

// withSuffix is the location of a sidecar (the manifest).
func (l location) withSuffix(sfx string) location {
	out := l
	out.raw += sfx
	if out.isS3() {
		out.key += sfx
	}
	return out
}

type s3Store struct{ client *s3.Client }

// Put uploads a file in one PutObject (the snapshot is ~150 MiB; the single
// request limit is 5 GiB), with a SHA-256 checksum S3 verifies on receipt.
// It also sends bucket-owner-full-control. The migration bucket now lives in
// the old account, beside the snapshot task (the Grid organization refuses
// writes from outside it), so the header is a no-op there; it is kept so an
// upload into a bucket another account owns still hands that owner the object.
func (s *s3Store) Put(ctx context.Context, bucket, key, path string) error {
	fh, err := os.Open(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		return err
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:            aws.String(bucket),
		Key:               aws.String(key),
		Body:              fh,
		ContentLength:     aws.Int64(st.Size()),
		ACL:               types.ObjectCannedACLBucketOwnerFullControl,
		ChecksumAlgorithm: types.ChecksumAlgorithmSha256,
	})
	if err != nil {
		return fmt.Errorf("gridctl: uploading s3://%s/%s: %w", bucket, key, err)
	}
	return nil
}

// Get downloads an object to a new file at path.
func (s *s3Store) Get(ctx context.Context, bucket, key, path string) error {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key),
		ChecksumMode: types.ChecksumModeEnabled})
	if err != nil {
		return fmt.Errorf("gridctl: downloading s3://%s/%s: %w", bucket, key, err)
	}
	defer out.Body.Close()
	fh, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(fh, out.Body); err != nil {
		fh.Close()
		os.Remove(path)
		return fmt.Errorf("gridctl: downloading s3://%s/%s: %w", bucket, key, err)
	}
	return fh.Close()
}

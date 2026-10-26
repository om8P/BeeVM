package store

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// S3Config says where the bucket is. Leave Endpoint empty for real AWS S3; set it (for example
// http://127.0.0.1:9000) for MinIO or any other S3-compatible server. Credentials are not part
// of the config: they come from the standard AWS chain (AWS_ACCESS_KEY_ID and
// AWS_SECRET_ACCESS_KEY, ~/.aws, or the instance role on EC2).
type S3Config struct {
	Bucket    string
	Region    string
	Endpoint  string
	PathStyle bool   // http://host/bucket/key instead of http://bucket.host/key; MinIO needs this
	Prefix    string // optional, put all keys under this "folder"
}

// S3 implements Blobs on the S3 API.
type S3 struct {
	c      *s3.Client
	bucket string
	prefix string
}

// NewS3 builds a client. It does not contact the server; call EnsureBucket for that.
func NewS3(ctx context.Context, cfg S3Config) (*S3, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("s3: bucket name is required")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("s3: load config: %w", err)
	}
	c := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.PathStyle
	})
	prefix := strings.Trim(cfg.Prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	return &S3{c: c, bucket: cfg.Bucket, prefix: prefix}, nil
}

func (s *S3) key(k string) string { return s.prefix + k }

// EnsureBucket checks that the bucket exists and creates it if it does not.
func (s *S3) EnsureBucket(ctx context.Context) error {
	_, err := s.c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &s.bucket})
	if err == nil {
		return nil
	}
	if !isNotFound(err) {
		return fmt.Errorf("s3: check bucket %q: %w", s.bucket, err)
	}
	_, err = s.c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &s.bucket})
	var owned *s3types.BucketAlreadyOwnedByYou
	if err != nil && !errors.As(err, &owned) {
		return fmt.Errorf("s3: create bucket %q: %w", s.bucket, err)
	}
	return nil
}

func (s *S3) Put(ctx context.Context, key string, body io.ReadSeeker, size int64) error {
	// S3 can verify the upload itself if we send the SHA-256 up front, so a corrupted
	// transfer is rejected by the server instead of being stored.
	h := sha256.New()
	if _, err := io.Copy(h, body); err != nil {
		return fmt.Errorf("s3: checksum: %w", err)
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("s3: rewind: %w", err)
	}
	_, err := s.c.PutObject(ctx, &s3.PutObjectInput{
		Bucket:         &s.bucket,
		Key:            aws.String(s.key(key)),
		Body:           body,
		ContentLength:  aws.Int64(size),
		ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(h.Sum(nil))),
	})
	if err != nil {
		return fmt.Errorf("s3: put %q: %w", key, err)
	}
	return nil
}

func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := s.c.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: aws.String(s.key(key))})
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("s3: get %q: %w", key, err)
	}
	return out.Body, nil
}

func (s *S3) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: aws.String(s.key(key))})
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("s3: head %q: %w", key, err)
}

func (s *S3) Delete(ctx context.Context, key string) error {
	// S3 answers 204 for a key that is not there, so no special case is needed.
	_, err := s.c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: aws.String(s.key(key))})
	if err != nil {
		return fmt.Errorf("s3: delete %q: %w", key, err)
	}
	return nil
}

// isNotFound recognises "no such key / bucket" in the several shapes the SDK reports it
// (HEAD requests have no body, so they only carry the HTTP status as a generic error code).
func isNotFound(err error) bool {
	var nsk *s3types.NoSuchKey
	var nf *s3types.NotFound
	var nsb *s3types.NoSuchBucket
	if errors.As(err, &nsk) || errors.As(err, &nf) || errors.As(err, &nsb) {
		return true
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NotFound", "NoSuchKey", "NoSuchBucket":
			return true
		}
	}
	return false
}

// Package objectstore is the control plane's S3-compatible object store
// (SeaweedFS) client: PutObject for registration-time ingest and PresignGet
// for agent fetch. Presign is a local signature computation — no network round
// trip — so ResolveArtifactSource stays O(1).
package objectstore

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// ObjectKey returns the content-addressed key layout used by registration-time
// ingest: artifacts/<name>/<version>/<sha256-hex>.
func ObjectKey(name, version, digest string) string {
	hex := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(digest)), "sha256:")
	return fmt.Sprintf("artifacts/%s/%s/%s", name, version, hex)
}

// ObjectURI builds s3://<bucket>/<ObjectKey(...)>.
func ObjectURI(bucket, name, version, digest string) string {
	return fmt.Sprintf("s3://%s/%s", bucket, ObjectKey(name, version, digest))
}

// DefaultPresignTTL is the lifetime of URLs handed to agents. The agent's
// download timeout is 10 minutes; TTL only constrains when the GET may start.
const DefaultPresignTTL = 5 * time.Minute

// Config holds SeaweedFS / S3 gateway connection settings.
type Config struct {
	Endpoint  string // e.g. http://127.0.0.1:8333
	Region    string // arbitrary for SeaweedFS; default "us-east-1"
	Bucket    string // default bucket for PutObject (ST-2 ingest)
	AccessKey string
	SecretKey string
}

// Store is the subset of S3 operations the control plane needs.
type Store interface {
	// PresignGet returns a time-limited HTTPS (or HTTP) GET URL for bucket/key.
	PresignGet(ctx context.Context, bucket, key string, ttl time.Duration) (url string, expiresAt time.Time, err error)
	// PutObject writes body to bucket/key. Used by registration-time ingest (ST-2).
	PutObject(ctx context.Context, bucket, key string, body io.Reader, size int64) error
	// Bucket returns the configured default bucket.
	Bucket() string
}

// S3 is a Store backed by aws-sdk-go-v2 against an S3-compatible endpoint.
type S3 struct {
	client  *s3.Client
	presign *s3.PresignClient
	bucket  string
}

// New builds an S3 client for a path-style SeaweedFS (or MinIO) gateway.
func New(cfg Config) (*S3, error) {
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("objectstore: endpoint is required")
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, fmt.Errorf("objectstore: access key and secret key are required")
	}
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	awsCfg := aws.Config{
		Region: region,
		Credentials: credentials.NewStaticCredentialsProvider(
			cfg.AccessKey, cfg.SecretKey, ""),
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(cfg.Endpoint)
		o.UsePathStyle = true
	})
	return &S3{
		client:  client,
		presign: s3.NewPresignClient(client),
		bucket:  cfg.Bucket,
	}, nil
}

// Bucket returns the configured default bucket (may be empty until ST-2).
func (s *S3) Bucket() string { return s.bucket }

// PresignGet signs a GetObject request locally and returns the URL.
func (s *S3) PresignGet(ctx context.Context, bucket, key string, ttl time.Duration) (string, time.Time, error) {
	if bucket == "" || key == "" {
		return "", time.Time{}, fmt.Errorf("objectstore: bucket and key are required")
	}
	if ttl <= 0 {
		ttl = DefaultPresignTTL
	}
	out, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("objectstore: presign get %s/%s: %w", bucket, key, err)
	}
	return out.URL, time.Now().Add(ttl), nil
}

// PutObject uploads body to bucket/key.
func (s *S3) PutObject(ctx context.Context, bucket, key string, body io.Reader, size int64) error {
	if bucket == "" || key == "" {
		return fmt.Errorf("objectstore: bucket and key are required")
	}
	input := &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   body,
	}
	if size >= 0 {
		input.ContentLength = aws.Int64(size)
	}
	if _, err := s.client.PutObject(ctx, input); err != nil {
		return fmt.Errorf("objectstore: put %s/%s: %w", bucket, key, err)
	}
	return nil
}

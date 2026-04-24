// Package blob wraps the S3-compatible object store used for uploaded
// documents. Production points at real S3; local dev hits the MinIO
// container in compose. All callers should use the Client interface so
// tests can substitute an in-memory fake.
package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/treetop/rag-svc/internal/config"
)

// Client is the narrow surface we need out of S3/MinIO. The interface
// keeps the rest of rag-svc testable without standing up a real S3.
type Client interface {
	Put(ctx context.Context, key string, body []byte, contentType string) error
	Exists(ctx context.Context, key string) (bool, error)
	Get(ctx context.Context, key string) ([]byte, string, error)
}

// New returns a real S3/MinIO-backed client bound to cfg.BlobBucket. The
// client pings the bucket (HeadBucket) and creates it if missing. In
// production IAM typically forbids CreateBucket — a 403 on create after
// HeadBucket failed-not-found is reported as an error; a 403 on create
// when HeadBucket succeeded is ignored.
func New(ctx context.Context, cfg config.Core) (Client, error) {
	if cfg.BlobEndpoint == "" {
		return nil, fmt.Errorf("blob: BLOB_ENDPOINT is required")
	}
	if cfg.BlobBucket == "" {
		return nil, fmt.Errorf("blob: BLOB_BUCKET is required")
	}
	region := "us-east-1"
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.BlobAccessKey, cfg.BlobSecretKey, ""),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("blob: aws config: %w", err)
	}
	s3c := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(cfg.BlobEndpoint)
		// Path-style is what MinIO expects; real S3 accepts it too.
		o.UsePathStyle = true
	})
	c := &s3Client{s3: s3c, bucket: cfg.BlobBucket}
	if err := c.ensureBucket(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

type s3Client struct {
	s3     *s3.Client
	bucket string
}

func (c *s3Client) ensureBucket(ctx context.Context) error {
	_, err := c.s3.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &c.bucket})
	if err == nil {
		return nil
	}
	// On NotFound / NoSuchBucket try to create. Any other error
	// propagates — misconfigured credentials should fail loud.
	if !isNotFound(err) {
		// If HeadBucket returns 403 it may mean "bucket exists but no
		// perms to probe" — treat as success so prod IAM works.
		if isForbidden(err) {
			return nil
		}
		return fmt.Errorf("blob: head bucket %q: %w", c.bucket, err)
	}
	_, err = c.s3.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &c.bucket})
	if err == nil {
		return nil
	}
	// In prod, CreateBucket may be forbidden — accept and assume the
	// bucket is provisioned out-of-band.
	if isForbidden(err) {
		return nil
	}
	// AlreadyOwnedByYou / BucketAlreadyExists — fine, someone else just
	// created it.
	var owned *s3types.BucketAlreadyOwnedByYou
	var existing *s3types.BucketAlreadyExists
	if errors.As(err, &owned) || errors.As(err, &existing) {
		return nil
	}
	return fmt.Errorf("blob: create bucket %q: %w", c.bucket, err)
}

func (c *s3Client) Put(ctx context.Context, key string, body []byte, contentType string) error {
	_, err := c.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      &c.bucket,
		Key:         &key,
		Body:        bytes.NewReader(body),
		ContentType: &contentType,
	})
	if err != nil {
		return fmt.Errorf("blob: put %s: %w", key, err)
	}
	return nil
}

func (c *s3Client) Exists(ctx context.Context, key string) (bool, error) {
	_, err := c.s3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &c.bucket, Key: &key})
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("blob: head %s: %w", key, err)
}

func (c *s3Client) Get(ctx context.Context, key string) ([]byte, string, error) {
	out, err := c.s3.GetObject(ctx, &s3.GetObjectInput{Bucket: &c.bucket, Key: &key})
	if err != nil {
		return nil, "", fmt.Errorf("blob: get %s: %w", key, err)
	}
	defer out.Body.Close()
	buf, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, "", err
	}
	ctype := ""
	if out.ContentType != nil {
		ctype = *out.ContentType
	}
	return buf, ctype, nil
}

// ---- error helpers ----

func isNotFound(err error) bool {
	var nsk *s3types.NoSuchKey
	var nsb *s3types.NoSuchBucket
	var nf *s3types.NotFound
	if errors.As(err, &nsk) || errors.As(err, &nsb) || errors.As(err, &nf) {
		return true
	}
	// HeadBucket/HeadObject return opaque responses; peek the status.
	return httpStatus(err) == 404
}

func isForbidden(err error) bool { return httpStatus(err) == 403 }

func httpStatus(err error) int {
	var respErr *smithyhttp.ResponseError
	if errors.As(err, &respErr) {
		if respErr.Response != nil {
			return respErr.Response.StatusCode
		}
	}
	return 0
}

// SanitizeKey normalizes a caller-supplied key: trims slashes, rejects
// obvious path traversal. Not a security boundary — bucket policy is —
// but keeps accidental `../` from slipping through.
func SanitizeKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", errors.New("blob: empty key")
	}
	if strings.Contains(key, "..") {
		return "", errors.New("blob: key contains ..")
	}
	u, err := url.Parse(key)
	if err != nil {
		return "", fmt.Errorf("blob: invalid key: %w", err)
	}
	return strings.TrimLeft(u.Path, "/"), nil
}

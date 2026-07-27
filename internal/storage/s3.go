package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/shridarpatil/whatomate/internal/config"
)

// ErrObjectNotFound normalizes provider-specific missing-object responses.
var ErrObjectNotFound = errors.New("object not found")

// ObjectStore is the storage boundary used by handlers for durable tenant
// media. Implementations must preserve the supplied object key verbatim.
type ObjectStore interface {
	Put(ctx context.Context, key string, data []byte, contentType string) error
	Get(ctx context.Context, key string) ([]byte, string, error)
	Delete(ctx context.Context, key string) error
	ListPrefix(ctx context.Context, prefix string) ([]ObjectInfo, error)
}

// ObjectInfo is the minimum provider-neutral metadata needed by privacy
// exports, retention jobs, and recovery checks.
type ObjectInfo struct {
	Key          string    `json:"key"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"last_modified"`
	ETag         string    `json:"etag,omitempty"`
}

// S3Client provides S3-compatible object storage operations.
type S3Client struct {
	client *s3.Client
	bucket string
}

// NewS3Client creates a new S3 client from the application's StorageConfig.
func NewS3Client(cfg *config.StorageConfig) (*S3Client, error) {
	if cfg.S3Bucket == "" || cfg.S3Region == "" {
		return nil, fmt.Errorf("s3_bucket and s3_region are required")
	}

	opts := s3.Options{
		Region:       cfg.S3Region,
		UsePathStyle: cfg.S3UsePathStyle,
	}

	if cfg.S3Endpoint != "" {
		opts.BaseEndpoint = aws.String(cfg.S3Endpoint)
	}

	if cfg.S3Key != "" && cfg.S3Secret != "" {
		opts.Credentials = credentials.NewStaticCredentialsProvider(cfg.S3Key, cfg.S3Secret, "")
	}

	client := s3.New(opts)
	return &S3Client{client: client, bucket: cfg.S3Bucket}, nil
}

// Put uploads an in-memory object.
func (s *S3Client) Put(ctx context.Context, key string, data []byte, contentType string) error {
	return s.Upload(ctx, key, bytes.NewReader(data), contentType)
}

// Upload uploads a file to S3 at the given key.
func (s *S3Client) Upload(ctx context.Context, key string, body io.Reader, contentType string) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        body,
		ContentType: aws.String(contentType),
	})
	return err
}

// Get downloads an object and returns its data and stored content type.
func (s *S3Client) Get(ctx context.Context, key string) ([]byte, string, error) {
	result, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) && (apiErr.ErrorCode() == "NoSuchKey" || apiErr.ErrorCode() == "NotFound") {
			return nil, "", fmt.Errorf("%w: %s", ErrObjectNotFound, key)
		}
		return nil, "", err
	}
	defer result.Body.Close() //nolint:errcheck

	data, err := io.ReadAll(result.Body)
	if err != nil {
		return nil, "", err
	}

	contentType := ""
	if result.ContentType != nil {
		contentType = *result.ContentType
	}
	return data, contentType, nil
}

// Delete removes one object. S3 deletion is idempotent, which lets privacy and
// retention jobs retry safely after partial failure.
func (s *S3Client) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	return err
}

// ListPrefix returns all objects under a tenant-scoped prefix.
func (s *S3Client) ListPrefix(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})

	var objects []ObjectInfo
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, item := range page.Contents {
			info := ObjectInfo{
				Key:  aws.ToString(item.Key),
				Size: aws.ToInt64(item.Size),
				ETag: aws.ToString(item.ETag),
			}
			if item.LastModified != nil {
				info.LastModified = *item.LastModified
			}
			objects = append(objects, info)
		}
	}
	return objects, nil
}

// GetPresignedURL returns a time-limited download URL for the given S3 key.
func (s *S3Client) GetPresignedURL(ctx context.Context, key string, expiry time.Duration) (string, error) {
	presigner := s3.NewPresignClient(s.client)
	req, err := presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(expiry))
	if err != nil {
		return "", err
	}
	return req.URL, nil
}

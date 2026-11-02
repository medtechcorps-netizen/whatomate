package storageprobe

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/storage"
)

var (
	ErrInvalidMetadataKey = errors.New("invalid object metadata key")
	// A HEAD 404 is an observation from the configured endpoint, not proof of
	// old-provider provenance or permission to remove an attachment reference.
	ErrObjectMetadataNotFound    = errors.New("object metadata endpoint returned not found")
	ErrObjectMetadataDenied      = errors.New("object metadata access denied")
	ErrObjectMetadataUnavailable = errors.New("object metadata unavailable")
)

// Reader exposes only a bounded metadata observation, with no write or content
// download operations. It is separate from the application's normal media store.
type Reader interface {
	Head(ctx context.Context, key string) (storage.ObjectInfo, error)
}

type Client struct {
	client *s3.Client
	bucket string
}

var _ Reader = (*Client)(nil)

// NewClient uses the same endpoint/region/key settings as media storage while
// keeping the metadata observer separate from content clients and their pools.
func NewClient(cfg *config.StorageConfig) (*Client, error) {
	if cfg == nil || cfg.S3Bucket == "" || cfg.S3Region == "" {
		return nil, errors.New("metadata bucket and region required")
	}
	opts := s3.Options{Region: cfg.S3Region, UsePathStyle: cfg.S3UsePathStyle}
	if cfg.S3Endpoint != "" {
		opts.BaseEndpoint = aws.String(cfg.S3Endpoint)
	}
	if cfg.S3Key != "" && cfg.S3Secret != "" {
		opts.Credentials = credentials.NewStaticCredentialsProvider(cfg.S3Key, cfg.S3Secret, "")
	}
	return &Client{client: s3.New(opts), bucket: cfg.S3Bucket}, nil
}

// Head observes metadata without downloading object content. It preserves the
// supplied key verbatim. Callers must establish tenant scope and exact key/bucket
// provenance separately; no result authorizes deletion. A 404 is intentionally
// distinct from ErrObjectNotFound used by existing content readers.
func (s *Client) Head(ctx context.Context, key string) (storage.ObjectInfo, error) {
	if key == "" || len(key) > 1024 || !utf8.ValidString(key) || strings.ContainsRune(key, 0) {
		return storage.ObjectInfo{}, ErrInvalidMetadataKey
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return storage.ObjectInfo{}, err
	}

	// Use the SDK's default transport (including system trust and proxy), but
	// reject every redirect and cap response headers for this isolated probe.
	// A dedicated transport is disposed after the request; normal media clients
	// and their retry/connection settings remain untouched.
	transport := awshttp.NewBuildableClient().GetTransport()
	transport.MaxResponseHeaderBytes = 16 * 1024
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	result, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}, func(options *s3.Options) {
		options.Retryer = aws.NopRetryer{}
		options.RetryMaxAttempts = 1
		options.HTTPClient = client
	})
	if err := ctx.Err(); err != nil {
		return storage.ObjectInfo{}, err
	}
	if err != nil {
		var response interface{ HTTPStatusCode() int }
		if errors.As(err, &response) {
			switch response.HTTPStatusCode() {
			case http.StatusNotFound:
				return storage.ObjectInfo{}, ErrObjectMetadataNotFound
			case http.StatusUnauthorized, http.StatusForbidden:
				return storage.ObjectInfo{}, ErrObjectMetadataDenied
			}
		}
		// Do not expose endpoint URLs, object keys, credential diagnostics or
		// arbitrary provider error text through inventory diagnostics.
		return storage.ObjectInfo{}, ErrObjectMetadataUnavailable
	}
	if result == nil || result.ContentLength == nil || *result.ContentLength < 0 || result.LastModified == nil {
		return storage.ObjectInfo{}, ErrObjectMetadataUnavailable
	}
	return storage.ObjectInfo{
		Key:          key,
		Size:         *result.ContentLength,
		LastModified: *result.LastModified,
		ETag:         aws.ToString(result.ETag),
	}, nil
}

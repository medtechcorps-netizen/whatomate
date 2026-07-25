package storage_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// NewS3Client validation: bucket and region required.

func TestNewS3Client_RequiresBucket(t *testing.T) {
	_, err := storage.NewS3Client(&config.StorageConfig{
		S3Region: "us-east-1",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "s3_bucket")
}

func TestNewS3Client_RequiresRegion(t *testing.T) {
	_, err := storage.NewS3Client(&config.StorageConfig{
		S3Bucket: "my-bucket",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "s3_region")
}

func TestNewS3Client_Succeeds_WithBucketAndRegion(t *testing.T) {
	c, err := storage.NewS3Client(&config.StorageConfig{
		S3Bucket: "b",
		S3Region: "us-east-1",
	})
	require.NoError(t, err)
	assert.NotNil(t, c)
}

func TestNewS3Client_AcceptsStaticCredentials(t *testing.T) {
	c, err := storage.NewS3Client(&config.StorageConfig{
		S3Bucket: "b",
		S3Region: "us-east-1",
		S3Key:    "AKIA...",
		S3Secret: "secret",
	})
	require.NoError(t, err)
	assert.NotNil(t, c)
}

func TestS3Client_UsesCustomEndpointForObjectRoundTrip(t *testing.T) {
	var stored []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/media-bucket/organizations/org-1/messages/image.jpg", r.URL.Path)
		switch r.Method {
		case http.MethodPut:
			var err error
			stored, err = io.ReadAll(r.Body)
			require.NoError(t, err)
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(stored)
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	client, err := storage.NewS3Client(&config.StorageConfig{
		S3Bucket:       "media-bucket",
		S3Region:       "sin",
		S3Key:          "test-key",
		S3Secret:       "test-secret",
		S3Endpoint:     server.URL,
		S3UsePathStyle: true,
	})
	require.NoError(t, err)

	key := "organizations/org-1/messages/image.jpg"
	require.NoError(t, client.Put(context.Background(), key, []byte("image-data"), "image/jpeg"))

	data, contentType, err := client.Get(context.Background(), key)
	require.NoError(t, err)
	assert.Equal(t, []byte("image-data"), data)
	assert.Equal(t, "image/jpeg", contentType)
}

func TestS3Client_GetMapsMissingObject(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`<Error><Code>NoSuchKey</Code><Message>missing</Message></Error>`))
	}))
	defer server.Close()

	client, err := storage.NewS3Client(&config.StorageConfig{
		S3Bucket:       "media-bucket",
		S3Region:       "auto",
		S3Key:          "test-key",
		S3Secret:       "test-secret",
		S3Endpoint:     server.URL,
		S3UsePathStyle: true,
	})
	require.NoError(t, err)

	_, _, err = client.Get(context.Background(), "missing.jpg")
	require.Error(t, err)
	assert.True(t, errors.Is(err, storage.ErrObjectNotFound))
}

package storage_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/storage"
	"github.com/stretchr/testify/require"
)

type countingReader struct {
	remaining int
	read      int
}

func (r *countingReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(p), r.remaining)
	r.remaining -= n
	r.read += n
	return n, nil
}

func TestBoundedReadConsumesOnlyLimitPlusOne(t *testing.T) {
	r := &countingReader{remaining: 1000000}
	data, err := storage.ReadLimited(context.Background(), r, 10)
	require.ErrorIs(t, err, storage.ErrObjectTooLarge)
	require.Nil(t, data)
	require.Equal(t, 11, r.read)
	data, err = storage.ReadLimited(context.Background(), strings.NewReader("exact"), 5)
	require.NoError(t, err)
	require.Equal(t, "exact", string(data))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r = &countingReader{remaining: 100}
	_, err = storage.ReadLimited(ctx, r, 10)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, r.read)
}

func TestS3LimitedReadRejectsAdvertisedAndChunkedOversize(t *testing.T) {
	for _, chunked := range []bool{false, true} {
		t.Run(map[bool]string{false: "advertised", true: "chunked"}[chunked], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if chunked {
					w.(http.Flusher).Flush()
				}
				_, _ = io.WriteString(w, strings.Repeat("x", 64))
			}))
			defer server.Close()
			client, err := storage.NewS3Client(&config.StorageConfig{
				S3Bucket: "bucket", S3Region: "us-east-1", S3Endpoint: server.URL,
				S3Key: "synthetic", S3Secret: "synthetic", S3UsePathStyle: true,
			})
			require.NoError(t, err)
			data, _, err := client.GetLimited(context.Background(), "organizations/test/media", 8)
			require.True(t, errors.Is(err, storage.ErrObjectTooLarge), "%v", err)
			require.Nil(t, data)
		})
	}
}

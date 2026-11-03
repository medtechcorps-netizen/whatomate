package storageprobe_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/storage"
	"github.com/shridarpatil/whatomate/internal/storageprobe"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func metadataClient(t *testing.T, endpoint string) *storageprobe.Client {
	t.Helper()
	client, err := storageprobe.NewClient(&config.StorageConfig{
		S3Bucket: "media-bucket", S3Region: "us-east-1",
		S3Key: "test-key", S3Secret: "test-secret", S3Endpoint: endpoint, S3UsePathStyle: true,
	})
	require.NoError(t, err)
	return client
}

func TestS3MetadataHeadPreservesKeyWithoutContentRead(t *testing.T) {
	key := "organizations/org-1/messages/a +%\u96ea.bin"
	modified := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.Equal(t, http.MethodHead, r.Method)
		assert.Equal(t, "/media-bucket/"+key, r.URL.Path)
		assert.Empty(t, r.URL.RawQuery)
		w.Header().Set("Content-Length", "1099511627776")
		w.Header().Set("Last-Modified", modified.Format(http.TimeFormat))
		w.Header().Set("ETag", `"opaque-provider-etag"`)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	info, err := metadataClient(t, server.URL).Head(context.Background(), key)
	require.NoError(t, err)
	assert.Equal(t, storage.ObjectInfo{Key: key, Size: 1099511627776, LastModified: modified, ETag: `"opaque-provider-etag"`}, info)
	assert.Equal(t, int32(1), calls.Load())
}

func TestS3MetadataHeadClassifiesFailuresWithoutRetryOrSecrets(t *testing.T) {
	for _, test := range []struct {
		code int
		want error
	}{
		{404, storageprobe.ErrObjectMetadataNotFound}, {401, storageprobe.ErrObjectMetadataDenied}, {403, storageprobe.ErrObjectMetadataDenied},
		{400, storageprobe.ErrObjectMetadataUnavailable}, {429, storageprobe.ErrObjectMetadataUnavailable}, {500, storageprobe.ErrObjectMetadataUnavailable}, {503, storageprobe.ErrObjectMetadataUnavailable},
	} {
		t.Run(http.StatusText(test.code), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Equal(t, http.MethodHead, r.Method)
				w.Header().Set("X-Amz-Request-Id", "private-provider-diagnostic")
				w.WriteHeader(test.code)
			}))
			defer server.Close()
			info, err := metadataClient(t, server.URL).Head(context.Background(), "private-object-key")
			require.ErrorIs(t, err, test.want)
			assert.NotErrorIs(t, err, storage.ErrObjectNotFound)
			assert.Empty(t, info)
			assert.NotContains(t, err.Error(), "private-object-key")
			assert.NotContains(t, err.Error(), "private-provider-diagnostic")
			assert.NotContains(t, err.Error(), server.URL)
			assert.Equal(t, int32(1), calls.Load())
		})
	}
}

func TestS3MetadataHeadNeverFollowsRedirects(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { destinationCalls.Add(1); w.WriteHeader(http.StatusOK) }))
	defer destination.Close()
	for _, code := range []int{301, 302, 303, 307, 308} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Equal(t, http.MethodHead, r.Method)
				w.Header().Set("Location", destination.URL+"/must-not-receive-credentials")
				w.WriteHeader(code)
			}))
			defer server.Close()
			info, err := metadataClient(t, server.URL).Head(context.Background(), "key")
			require.ErrorIs(t, err, storageprobe.ErrObjectMetadataUnavailable)
			assert.Empty(t, info)
			assert.Equal(t, int32(1), calls.Load())
		})
	}
	assert.Zero(t, destinationCalls.Load())
}

func TestS3MetadataHeadHonorsCancellationAndShortDeadline(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "deadline"}[deadline], func(t *testing.T) {
			entered := make(chan struct{})
			released := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				close(entered)
				<-r.Context().Done()
				close(released)
			}))
			defer server.Close()
			ctx, cancel := context.WithCancel(context.Background())
			if deadline {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
			}
			defer cancel()
			result := make(chan error, 1)
			client := metadataClient(t, server.URL)
			go func() {
				info, err := client.Head(ctx, "key")
				if info != (storage.ObjectInfo{}) {
					result <- errors.New("partial metadata published")
					return
				}
				result <- err
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("request did not arrive")
			}
			if !deadline {
				cancel()
			}
			select {
			case err := <-result:
				if deadline {
					require.ErrorIs(t, err, context.DeadlineExceeded)
				} else {
					require.ErrorIs(t, err, context.Canceled)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("probe did not stop")
			}
			select {
			case <-released:
			case <-time.After(3 * time.Second):
				t.Fatal("HTTP request not canceled")
			}
		})
	}
}

func TestS3MetadataHeadRejectsInvalidKeysAndAlreadyCanceledContext(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	client := metadataClient(t, server.URL)
	for _, key := range []string{"", strings.Repeat("x", 1025), "a\x00b", string([]byte{0xff})} {
		info, err := client.Head(context.Background(), key)
		require.ErrorIs(t, err, storageprobe.ErrInvalidMetadataKey)
		assert.Empty(t, info)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	info, err := client.Head(ctx, "valid-key")
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, info)
	assert.Zero(t, calls.Load())
}

func TestS3MetadataHeadHasOwnDeadline(t *testing.T) {
	released := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		close(released)
	}))
	defer server.Close()
	started := time.Now()
	info, err := metadataClient(t, server.URL).Head(context.Background(), "key")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Empty(t, info)
	assert.Less(t, time.Since(started), 8*time.Second)
	select {
	case <-released:
	case <-time.After(3 * time.Second):
		t.Fatal("timed-out HTTP request was not canceled")
	}
}

func TestS3MetadataHeadNetworkFailureIsUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.Close()
	info, err := metadataClient(t, server.URL).Head(context.Background(), "private-key")
	require.ErrorIs(t, err, storageprobe.ErrObjectMetadataUnavailable)
	assert.Empty(t, info)
	assert.NotContains(t, err.Error(), server.URL)
	assert.NotContains(t, err.Error(), "private-key")
}

func TestS3MetadataHeadRejectsOversizedOrIncompleteHeaders(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing-last-modified", true: "oversized"}[oversized], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Length", "10")
				if oversized {
					w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
					w.Header().Set("X-Amz-Meta-Large", strings.Repeat("x", 32*1024))
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			info, err := metadataClient(t, server.URL).Head(context.Background(), "key")
			require.ErrorIs(t, err, storageprobe.ErrObjectMetadataUnavailable)
			assert.Empty(t, info)
		})
	}
}

func TestMetadataClientRequiresBucketAndRegion(t *testing.T) {
	for _, cfg := range []*config.StorageConfig{nil, {}, {S3Bucket: "bucket"}, {S3Region: "us-east-1"}} {
		client, err := storageprobe.NewClient(cfg)
		require.Error(t, err)
		require.Nil(t, client)
	}
}

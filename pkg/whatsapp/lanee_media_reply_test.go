package whatsapp_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// laneEMediaPayloads records the JSON bodies sent to the messages endpoint.
func laneEMediaPayloads(t *testing.T) (*whatsapp.Client, *whatsapp.Account, func() []map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var payloads []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		payloads = append(payloads, body)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages": []map[string]string{{"id": "wamid.media"}},
		})
	}))
	t.Cleanup(server.Close)
	client := whatsapp.NewWithTimeout(testutil.NopLogger(), 5*time.Second)
	client.HTTPClient = &http.Client{Transport: &testServerTransport{serverURL: server.URL}}
	return client, testAccount(server.URL), func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), payloads...)
	}
}

func TestLaneEMediaMessagesCarryOptionalReplyContext(t *testing.T) {
	t.Parallel()
	client, account, sent := laneEMediaPayloads(t)
	ctx := testutil.TestContext(t)
	rcpt := whatsapp.Recipient{Phone: "1234567890"}

	sends := []func(context.Context, ...string) (string, error){
		func(ctx context.Context, reply ...string) (string, error) {
			return client.SendImageMessage(ctx, account, rcpt, "m1", "cap", reply...)
		},
		func(ctx context.Context, reply ...string) (string, error) {
			return client.SendVideoMessage(ctx, account, rcpt, "m2", "cap", reply...)
		},
		func(ctx context.Context, reply ...string) (string, error) {
			return client.SendAudioMessage(ctx, account, rcpt, "m3", reply...)
		},
		func(ctx context.Context, reply ...string) (string, error) {
			return client.SendDocumentMessage(ctx, account, rcpt, "m4", "a.pdf", "cap", reply...)
		},
	}
	for _, send := range sends {
		_, err := send(ctx, "wamid.quoted")
		require.NoError(t, err)
		_, err = send(ctx, "")
		require.NoError(t, err)
		_, err = send(ctx)
		require.NoError(t, err)
	}

	payloads := sent()
	require.Len(t, payloads, 12)
	for i, payload := range payloads {
		if i%3 == 0 {
			assert.Equal(t, map[string]any{"message_id": "wamid.quoted"}, payload["context"], "payload %d", i)
			continue
		}
		assert.NotContains(t, payload, "context", "payload %d", i)
	}
}

// A quote or line break in a file name or MIME type must not end the part
// header early, and Graph always gets a file name.
func TestLaneEUploadMediaSanitizesPartHeaders(t *testing.T) {
	t.Parallel()
	type seen struct{ name, contentType, injected, body string }
	var mu sync.Mutex
	var parts []seen
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reader, err := r.MultipartReader()
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if part.FormName() == "file" {
				data, readErr := io.ReadAll(part)
				assert.NoError(t, readErr)
				mu.Lock()
				parts = append(parts, seen{part.FileName(), part.Header.Get("Content-Type"), part.Header.Get("X-Injected"), string(data)})
				mu.Unlock()
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "media-ok"})
	}))
	t.Cleanup(server.Close)
	client := whatsapp.NewWithTimeout(testutil.NopLogger(), 5*time.Second)
	client.HTTPClient = &http.Client{Transport: &testServerTransport{serverURL: server.URL}}
	ctx := testutil.TestContext(t)

	_, err := client.UploadMedia(ctx, testAccount(server.URL), []byte("x"), "image/png\r\nX-Injected: 1", "scan \"a\".png\r\nX-Injected: 2")
	require.NoError(t, err)
	_, err = client.UploadMedia(ctx, testAccount(server.URL), []byte("x"), "audio/ogg", "  ")
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, parts, 2)
	assert.Equal(t, `scan "a".pngX-Injected: 2`, parts[0].name)
	assert.Empty(t, parts[0].injected)
	assert.Equal(t, "image/pngX-Injected: 1", parts[0].contentType)
	assert.Equal(t, "upload", parts[1].name)
	assert.Equal(t, "x", parts[0].body)
	assert.Equal(t, "x", parts[1].body)
}

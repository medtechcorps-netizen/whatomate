package handlers

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type templateMediaRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn templateMediaRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestDownloadTemplateHeaderMediaRejectsUnsafeURLsBeforeTransport(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	app := &App{HTTPClient: &http.Client{Transport: templateMediaRoundTripFunc(
		func(*http.Request) (*http.Response, error) {
			attempts.Add(1)
			return nil, errors.New("transport must not be reached")
		},
	)}}

	for _, rawURL := range []string{
		"http://media.example.com/header.png",
		"https://localhost/header.png",
		"https://127.0.0.1/header.png",
		"https://10.0.0.1/header.png",
		"https://169.254.169.254/latest/meta-data",
		"https://user:password@media.example.com/header.png",
	} {
		_, _, err := app.downloadTemplateHeaderMedia(context.Background(), rawURL)
		require.ErrorIs(t, err, errTemplateHeaderMediaInvalidURL, rawURL)
	}

	assert.Zero(t, attempts.Load())
}

func TestDownloadTemplateHeaderMediaUsesMappedHTTPSClient(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		assert.Equal(t, "/header.png", request.URL.Path)
		assert.Equal(t, "signature=synthetic", request.URL.RawQuery)
		response.Header().Set("Content-Type", "image/png")
		_, _ = response.Write([]byte("synthetic-image"))
	}))
	t.Cleanup(server.Close)

	client := testutil.NewHTTPSRewriteClient(t, map[string]*httptest.Server{
		"https://media.example.com": server,
	})
	originalRedirectPolicy := func(_ *http.Request, _ []*http.Request) error {
		return errors.New("original redirect policy")
	}
	client.CheckRedirect = originalRedirectPolicy
	app := &App{HTTPClient: client}

	data, mimeType, err := app.downloadTemplateHeaderMedia(
		context.Background(),
		"  https://media.example.com/header.png?signature=synthetic  ",
	)
	require.NoError(t, err)
	assert.Equal(t, []byte("synthetic-image"), data)
	assert.Equal(t, "image/png", mimeType)
	require.NotNil(t, client.CheckRedirect, "shared client redirect policy must not be mutated")
	assert.EqualError(t, client.CheckRedirect(nil, nil), "original redirect policy")
}

func TestDownloadTemplateHeaderMediaDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()

	var targetCalls atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		targetCalls.Add(1)
		response.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)

	source := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, "https://media-target.example.com/private", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(source.Close)

	app := &App{HTTPClient: testutil.NewHTTPSRewriteClient(t, map[string]*httptest.Server{
		"https://media-source.example.com": source,
		"https://media-target.example.com": target,
	})}

	_, _, err := app.downloadTemplateHeaderMedia(
		context.Background(),
		"https://media-source.example.com/header.png",
	)
	require.ErrorIs(t, err, errTemplateHeaderMediaUnexpectedStatus)
	assert.Zero(t, targetCalls.Load())
}

func TestDownloadTemplateHeaderMediaRejectsDeclaredAndStreamingOversizeBodies(t *testing.T) {
	t.Parallel()

	t.Run("declared content length", func(t *testing.T) {
		app := &App{HTTPClient: &http.Client{Transport: templateMediaRoundTripFunc(
			func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode:    http.StatusOK,
					ContentLength: templateHeaderMediaMaxBytes + 1,
					Header:        make(http.Header),
					Body:          io.NopCloser(bytes.NewReader(nil)),
				}, nil
			},
		)}}

		_, _, err := app.downloadTemplateHeaderMedia(context.Background(), "https://media.example.com/large")
		require.ErrorIs(t, err, errTemplateHeaderMediaTooLarge)
	})

	t.Run("streaming body", func(t *testing.T) {
		body := bytes.Repeat([]byte{'x'}, templateHeaderMediaMaxBytes+1)
		app := &App{HTTPClient: &http.Client{Transport: templateMediaRoundTripFunc(
			func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode:    http.StatusOK,
					ContentLength: -1,
					Header:        make(http.Header),
					Body:          io.NopCloser(bytes.NewReader(body)),
				}, nil
			},
		)}}

		_, _, err := app.downloadTemplateHeaderMedia(context.Background(), "https://media.example.com/stream")
		require.ErrorIs(t, err, errTemplateHeaderMediaTooLarge)
	})
}

func TestDownloadTemplateHeaderMediaFailsClosedWithoutSafeClient(t *testing.T) {
	t.Parallel()

	for _, app := range []*App{
		nil,
		{},
		{HTTPClient: &http.Client{}},
	} {
		_, _, err := app.downloadTemplateHeaderMedia(context.Background(), "https://media.example.com/header.png")
		require.ErrorIs(t, err, errTemplateHeaderMediaClientUnavailable)
	}
}

func TestReadTemplateHeaderMediaEnforcesMultipartLimit(t *testing.T) {
	t.Parallel()

	data, err := readTemplateHeaderMedia(bytes.NewReader(bytes.Repeat([]byte{'x'}, templateHeaderMediaMaxBytes)))
	require.NoError(t, err)
	assert.Len(t, data, templateHeaderMediaMaxBytes)

	_, err = readTemplateHeaderMedia(bytes.NewReader(bytes.Repeat([]byte{'x'}, templateHeaderMediaMaxBytes+1)))
	require.ErrorIs(t, err, errTemplateHeaderMediaTooLarge)
}

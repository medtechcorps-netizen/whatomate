package handlers

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type threadsOAuthEndpointRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn threadsOAuthEndpointRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestThreadsOAuthTokenExchangesUseOfficialGraphHost(t *testing.T) {
	var paths []string
	app := &App{HTTPClient: &http.Client{Transport: threadsOAuthEndpointRoundTripFunc(
		func(request *http.Request) (*http.Response, error) {
			assert.Equal(t, "graph.threads.net", request.URL.Host)
			paths = append(paths, request.URL.Path)
			payload := `{"access_token":"long-lived-token","expires_in":5184000}`
			if request.URL.Path == "/oauth/access_token" {
				payload = `{"access_token":"short-lived-token","user_id":"1283576863615439"}`
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(payload)),
				Header:     make(http.Header),
			}, nil
		},
	)}}

	snapshot := threadsIntegrationSnapshot{
		AppID:       "1331429782494489",
		AppSecret:   "test-app-secret",
		RedirectURI: "https://app.example.test/api/integrations/threads/callback",
	}
	shortToken, err := app.exchangeThreadsAuthorizationCode(context.Background(), snapshot, "authorization-code")
	require.NoError(t, err)
	_, err = app.exchangeThreadsLongLivedToken(context.Background(), snapshot.AppSecret, shortToken.AccessToken)
	require.NoError(t, err)
	assert.Equal(t, []string{"/oauth/access_token", "/access_token"}, paths)
}

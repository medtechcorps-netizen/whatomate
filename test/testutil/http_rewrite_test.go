package testutil

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewHTTPSRewriteClientRoutesOnlyMappedPublicOrigins(t *testing.T) {
	var receivedPath, receivedQuery string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		receivedPath = request.URL.Path
		receivedQuery = request.URL.RawQuery
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := NewHTTPSRewriteClient(t, map[string]*httptest.Server{
		"https://hooks.example.com": server,
	})
	response, err := client.Get("https://hooks.example.com/events/contact?id=synthetic")
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()

	assert.Equal(t, http.StatusNoContent, response.StatusCode)
	assert.Equal(t, "/events/contact", receivedPath)
	assert.Equal(t, "id=synthetic", receivedQuery)

	_, err = client.Get("https://unmapped.example.com/must-not-leave-the-test")
	require.ErrorContains(t, err, "is not mapped")
}

package handlers

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zerodha/logf"
)

const metaMessengerDebugTestInputToken = "synthetic-user-token-never-log"

func metaMessengerDebugTestLogger(buffer *bytes.Buffer) logf.Logger {
	return logf.New(logf.Opts{
		Writer: buffer, Level: logf.DebugLevel,
		EnableCaller: false, EnableColor: false,
	})
}

func assertMetaMessengerDebugCredentialsRedacted(
	t *testing.T,
	value string,
) {
	t.Helper()
	appAccessToken := metaLifecycleTestAppID + "|" + metaLifecycleTestAppSecret
	assert.NotContains(t, value, metaMessengerDebugTestInputToken)
	assert.NotContains(t, value, appAccessToken)
	assert.NotContains(t, value, metaLifecycleTestAppSecret)
}

func TestMetaMessengerTokenInspectionRedactsHostileProviderError(t *testing.T) {
	appAccessToken := metaLifecycleTestAppID + "|" + metaLifecycleTestAppSecret
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.Equal(t, http.MethodGet, request.Method)
		require.Equal(t, metaMessengerDebugTestInputToken, request.URL.Query().Get("input_token"))
		require.Equal(t, "Bearer "+appAccessToken, request.Header.Get("Authorization"))
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(writer,
			`{"error":{"message":%q,"type":"OAuthException","code":100,"error_subcode":33}}`,
			"provider echoed "+request.URL.String()+" "+appAccessToken+" "+metaLifecycleTestAppSecret,
		)
	}))
	defer server.Close()

	var logs bytes.Buffer
	app := newMetaLifecycleGraphApp(t, server)
	app.Log = metaMessengerDebugTestLogger(&logs)
	_, err := app.inspectMetaMessengerToken(t.Context(), metaMessengerDebugTestInputToken, true)
	require.Error(t, err)
	assert.ErrorContains(t, err, "HTTP 400")
	assert.ErrorContains(t, err, "code 100")
	assert.ErrorContains(t, err, "subcode 33")
	assert.ErrorContains(t, err, "type OAuthException")
	app.Log.Warn("Messenger token inspection rejected", "error", err)

	assertMetaMessengerDebugCredentialsRedacted(t, err.Error())
	assertMetaMessengerDebugCredentialsRedacted(t, logs.String())
}

func TestMetaMessengerTokenInspectionRedactsTransportURL(t *testing.T) {
	appAccessToken := metaLifecycleTestAppID + "|" + metaLifecycleTestAppSecret
	var logs bytes.Buffer
	dummy := httptest.NewServer(http.NotFoundHandler())
	defer dummy.Close()
	app := newMetaLifecycleGraphApp(t, dummy)
	app.Log = metaMessengerDebugTestLogger(&logs)
	app.HTTPClient = &http.Client{Transport: metaMessengerRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		require.Equal(t, http.MethodGet, request.Method)
		require.Equal(t, metaMessengerDebugTestInputToken, request.URL.Query().Get("input_token"))
		return nil, fmt.Errorf(
			"synthetic transport error for %s with %s and %s",
			request.URL.String(), appAccessToken, metaLifecycleTestAppSecret,
		)
	})}

	_, err := app.inspectMetaMessengerToken(t.Context(), metaMessengerDebugTestInputToken, true)
	require.EqualError(t, err, "meta provider request failed")
	app.Log.Warn("Messenger token inspection unavailable", "error", err)

	assertMetaMessengerDebugCredentialsRedacted(t, err.Error())
	assertMetaMessengerDebugCredentialsRedacted(t, logs.String())
}

func TestMetaMessengerTokenInspectionDoesNotReplayCredentialsAcrossRedirect(t *testing.T) {
	var redirectedCalls atomic.Int32
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirectedCalls.Add(1)
	}))
	defer redirectTarget.Close()

	appAccessToken := metaLifecycleTestAppID + "|" + metaLifecycleTestAppSecret
	source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.Equal(t, http.MethodGet, request.Method)
		require.Equal(t, metaMessengerDebugTestInputToken, request.URL.Query().Get("input_token"))
		require.Equal(t, "Bearer "+appAccessToken, request.Header.Get("Authorization"))
		writer.Header().Set(
			"Location",
			redirectTarget.URL+"/capture?"+request.URL.RawQuery,
		)
		writer.WriteHeader(http.StatusTemporaryRedirect)
		_, _ = writer.Write([]byte(
			"redirect " + metaMessengerDebugTestInputToken + " " + appAccessToken,
		))
	}))
	defer source.Close()

	app := newMetaLifecycleGraphApp(t, source)
	_, err := app.inspectMetaMessengerToken(t.Context(), metaMessengerDebugTestInputToken, true)
	require.Error(t, err)
	assert.Equal(t, int32(0), redirectedCalls.Load())
	var providerErr *metaMessengerProviderError
	require.True(t, errors.As(err, &providerErr))
	assert.Equal(t, http.StatusTemporaryRedirect, providerErr.StatusCode)
	assertMetaMessengerDebugCredentialsRedacted(t, err.Error())
}

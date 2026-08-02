package calling

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type callbackRoundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip callbackRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func TestValidateHTTPCallbackURLRequiresPublicHTTPS(t *testing.T) {
	t.Parallel()

	for _, validURL := range []string{
		"https://crm.example.test/ivr",
		"https://crm.example.test:8443/ivr?call_id=example",
	} {
		assert.NoError(t, ValidateHTTPCallbackURL(validURL), validURL)
	}

	for _, invalidURL := range []string{
		"",
		"http://crm.example.test/ivr",
		"https://localhost/ivr",
		"https://127.0.0.1/ivr",
		"https://10.10.0.1/ivr",
		"https://user:password@crm.example.test/ivr",
		"https://crm.example.test/ivr#secret",
		"https://{{callback_host}}/ivr",
	} {
		err := ValidateHTTPCallbackURL(invalidURL)
		require.Error(t, err, invalidURL)
		assert.Contains(t, err.Error(), "public HTTPS URL", invalidURL)
	}
}

func TestValidateHTTPCallbackTemplateURLRequiresTemplatedQueryValues(t *testing.T) {
	t.Parallel()

	require.NoError(t, ValidateHTTPCallbackTemplateURL("https://crm.example.test/ivr?call_id={{call_id}}"))
	require.ErrorContains(t, ValidateHTTPCallbackTemplateURL("https://crm.example.test/ivr?token=literal-secret"), "placeholders")
}

func TestValidateHTTPCallbackMethodAllowsOnlyEditorMethods(t *testing.T) {
	t.Parallel()

	for _, method := range []string{"GET", "post", " POST "} {
		assert.NoError(t, ValidateHTTPCallbackMethod(method))
	}
	for _, method := range []string{"", "PUT", "CONNECT", "TRACE"} {
		assert.Error(t, ValidateHTTPCallbackMethod(method))
	}
}

func TestExecuteHTTPCallbackUsesInjectedClientAndRefusesRedirects(t *testing.T) {
	t.Parallel()

	var requestCount atomic.Int32
	var inheritedRedirectCalls atomic.Int32
	client := &http.Client{
		Timeout: 2 * time.Minute,
		Transport: callbackRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			requestCount.Add(1)
			assert.Equal(t, "source.example.test", request.URL.Hostname())
			assert.Equal(t, "Bearer endpoint-secret", request.Header.Get("Authorization"))
			return &http.Response{
				StatusCode: http.StatusTemporaryRedirect,
				Status:     "307 Temporary Redirect",
				Header: http.Header{
					"Location": []string{"https://destination.example.test/receive"},
				},
				Body:    io.NopCloser(strings.NewReader("redirect refused")),
				Request: request,
			}, nil
		}),
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			inheritedRedirectCalls.Add(1)
			return nil
		},
	}

	result, err := executeHTTPCallback(
		client,
		"https://source.example.test/send",
		http.MethodPost,
		map[string]string{"Authorization": "Bearer endpoint-secret"},
		`{"event":"call"}`,
		3*time.Second,
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, http.StatusTemporaryRedirect, result.StatusCode)
	assert.Equal(t, int32(1), requestCount.Load(), "redirect destination must never be requested")
	assert.Zero(t, inheritedRedirectCalls.Load(), "callback must override a permissive redirect policy")
	assert.Equal(t, 2*time.Minute, client.Timeout, "per-callback timeout must not mutate the shared client")
}

func TestExecuteHTTPCallbackFailsClosedWithoutGuardedClient(t *testing.T) {
	t.Parallel()

	_, err := executeHTTPCallback(
		nil,
		"https://crm.example.test/ivr",
		http.MethodGet,
		nil,
		"",
		time.Second,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "client is not configured")
}

func TestExecuteHTTPCallbackRevalidatesLegacyURLBeforeTransport(t *testing.T) {
	t.Parallel()

	var requestCount atomic.Int32
	client := &http.Client{Transport: callbackRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestCount.Add(1)
		return nil, assert.AnError
	})}

	_, err := executeHTTPCallback(
		client,
		"https://127.0.0.1/admin",
		http.MethodGet,
		map[string]string{"Authorization": "Bearer must-not-leave"},
		"",
		time.Second,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "public HTTPS URL")
	assert.Zero(t, requestCount.Load(), "legacy unsafe URLs must fail before the injected transport")
}

func TestExecuteHTTPCallbackRejectsLegacyUnsafeMethodBeforeTransport(t *testing.T) {
	t.Parallel()

	var requestCount atomic.Int32
	client := &http.Client{Transport: callbackRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestCount.Add(1)
		return nil, assert.AnError
	})}

	_, err := executeHTTPCallback(
		client,
		"https://crm.example.test/ivr",
		http.MethodConnect,
		map[string]string{"Authorization": "Bearer must-not-leave"},
		"",
		time.Second,
	)
	require.ErrorContains(t, err, "GET or POST")
	assert.Zero(t, requestCount.Load())
}

func TestManagerHTTPCallbackUsesInjectedClient(t *testing.T) {
	t.Parallel()

	var requestCount atomic.Int32
	manager := &Manager{
		log: testutil.NopLogger(),
		httpClient: &http.Client{Transport: callbackRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			requestCount.Add(1)
			assert.Equal(t, "call-123", request.URL.Query().Get("call_id"))
			return &http.Response{
				StatusCode: http.StatusNoContent,
				Status:     "204 No Content",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    request,
			}, nil
		})},
	}
	node := &IVRNode{Config: map[string]any{
		"url":    "https://crm.example.test/ivr?call_id={{call_id}}",
		"method": http.MethodGet,
	}}
	context := &IVRContext{Variables: map[string]string{"call_id": "call-123"}}

	outcome := manager.executeHTTPCallback(&CallSession{ID: "call-123"}, node, context)

	assert.Equal(t, "http:2xx", outcome)
	assert.Equal(t, int32(1), requestCount.Load())
}

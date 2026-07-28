package qwen

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testRoundTripper func(*http.Request) (*http.Response, error)

func (fn testRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestGenerateSendsAuthenticatedTextCompletionPayload(t *testing.T) {
	var receivedBody string
	client := &http.Client{Transport: testRoundTripper(
		func(request *http.Request) (*http.Response, error) {
			assert.Equal(t, "https://qwen.example.test/v1/chat/completions", request.URL.String())
			assert.Equal(t, "Bearer super-secret", request.Header.Get("Authorization"))
			assert.Equal(t, "application/json", request.Header.Get("Content-Type"))
			body, err := io.ReadAll(request.Body)
			require.NoError(t, err)
			receivedBody = string(body)
			return qwenTestResponse(http.StatusOK, `{"choices":[{"message":{"content":"Hello"}}]}`), nil
		},
	)}

	result, err := Generate(context.Background(), client, Options{
		APIKey:      "super-secret",
		BaseURL:     "https://qwen.example.test/v1",
		Model:       "qwen-test",
		MaxTokens:   300,
		Temperature: 0.2,
		Messages: []Message{
			{Role: "system", Content: "plain text only"},
			{Role: "user", Content: "Hi"},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "Hello", result)
	assert.Contains(t, receivedBody, `"enable_thinking":false`)
	assert.Contains(t, receivedBody, `"max_tokens":300`)
	assert.Contains(t, receivedBody, `"model":"qwen-test"`)
}

func TestGenerateRejectsOversizedMalformedAndEmptyResponses(t *testing.T) {
	for _, testCase := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "oversized",
			body: strings.Repeat("x", maxResponseBytes+1),
			want: "size limit",
		},
		{
			name: "malformed",
			body: "{",
			want: "parse Qwen response failed",
		},
		{
			name: "no choices",
			body: `{"choices":[]}`,
			want: "no response from Qwen",
		},
		{
			name: "empty content",
			body: `{"choices":[{"message":{"content":"   "}}]}`,
			want: "empty response",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			client := &http.Client{Transport: testRoundTripper(
				func(_ *http.Request) (*http.Response, error) {
					return qwenTestResponse(http.StatusOK, testCase.body), nil
				},
			)}
			_, err := Generate(context.Background(), client, validQwenTestOptions())
			require.Error(t, err)
			assert.Contains(t, err.Error(), testCase.want)
		})
	}
}

func TestGenerateRefusesRedirectWithoutReplayingCredentials(t *testing.T) {
	var calls atomic.Int32
	client := &http.Client{Transport: testRoundTripper(
		func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			if request.URL.Host == "evil.example.test" {
				t.Fatalf("redirect target received a request")
			}
			response := qwenTestResponse(http.StatusFound, "redirect body")
			response.Header.Set("Location", "https://evil.example.test/steal")
			return response, nil
		},
	)}

	_, err := Generate(context.Background(), client, validQwenTestOptions())
	require.Error(t, err)
	assert.Equal(t, "qwen API returned status 302", err.Error())
	assert.EqualValues(t, 1, calls.Load())
}

func TestGenerateSanitizesTransportAndProviderErrors(t *testing.T) {
	const sensitive = "SECRET-PROVIDER-BODY-AND-PROMPT"
	transportClient := &http.Client{Transport: testRoundTripper(
		func(_ *http.Request) (*http.Response, error) {
			return nil, errors.New(sensitive)
		},
	)}
	_, err := Generate(context.Background(), transportClient, validQwenTestOptions())
	require.Error(t, err)
	assert.Equal(t, "qwen request failed", err.Error())
	assert.NotContains(t, err.Error(), sensitive)

	providerClient := &http.Client{Transport: testRoundTripper(
		func(_ *http.Request) (*http.Response, error) {
			return qwenTestResponse(
				http.StatusTooManyRequests,
				`{"error":{"message":"`+sensitive+`"}}`,
			), nil
		},
	)}
	_, err = Generate(context.Background(), providerClient, validQwenTestOptions())
	require.Error(t, err)
	assert.Equal(t, "qwen API returned status 429", err.Error())
	assert.NotContains(t, err.Error(), sensitive)
}

func validQwenTestOptions() Options {
	return Options{
		APIKey:    "test-key",
		BaseURL:   "https://qwen.example.test/v1",
		Model:     "qwen-test",
		MaxTokens: 300,
		Messages:  []Message{{Role: "user", Content: "hello"}},
	}
}

func qwenTestResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

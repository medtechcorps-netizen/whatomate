package handlers

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type qwenRoundTripFunc func(*http.Request) (*http.Response, error)

func (f qwenRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestGenerateQwenResponse(t *testing.T) {
	t.Parallel()

	var (
		requestURL    string
		authorization string
		payload       map[string]any
	)
	app := &App{
		Config: &config.Config{
			AI: config.AIConfig{
				QwenBaseURL: "https://workspace.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1/",
			},
		},
		HTTPClient: &http.Client{
			Transport: qwenRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				requestURL = request.URL.String()
				authorization = request.Header.Get("Authorization")
				require.NoError(t, json.NewDecoder(request.Body).Decode(&payload))

				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"Hello from Qwen"}}]}`)),
					Request:    request,
				}, nil
			}),
		},
	}
	settings := &models.ChatbotSettings{
		AI: models.AIConfig{
			APIKey:       "test-api-key",
			Model:        "qwen3.7-plus",
			MaxTokens:    500,
			Temperature:  0.2,
			SystemPrompt: "You are a helpful CRM assistant.",
		},
	}

	response, err := app.generateQwenResponse(settings, nil, "Hello", "")

	require.NoError(t, err)
	assert.Equal(t, "Hello from Qwen", response)
	assert.Equal(t, "https://workspace.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1/chat/completions", requestURL)
	assert.Equal(t, "Bearer test-api-key", authorization)
	assert.Equal(t, "qwen3.7-plus", payload["model"])
	assert.Equal(t, false, payload["enable_thinking"])
	assert.Equal(t, float64(500), payload["max_tokens"])
	assert.Equal(t, float64(0.2), payload["temperature"])
}

func TestGenerateQwenResponseUsesDefaultModelWithoutMutatingSettings(t *testing.T) {
	t.Parallel()

	var payload map[string]any
	app := &App{
		HTTPClient: &http.Client{
			Transport: qwenRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				require.NoError(t, json.NewDecoder(request.Body).Decode(&payload))
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"OK"}}]}`)),
					Request:    request,
				}, nil
			}),
		},
	}
	settings := &models.ChatbotSettings{
		AI: models.AIConfig{
			APIKey:    "test-api-key",
			MaxTokens: 500,
		},
	}

	_, err := app.generateQwenResponse(settings, nil, "Hello", "")

	require.NoError(t, err)
	assert.Equal(t, "qwen3.7-plus", payload["model"])
	assert.Empty(t, settings.AI.Model)
}

func TestGenerateQwenResponseRejectsOversizedProviderResponse(t *testing.T) {
	t.Parallel()

	app := &App{
		HTTPClient: &http.Client{
			Transport: qwenRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(strings.Repeat("x", (4<<20)+1))),
					Request:    request,
				}, nil
			}),
		},
	}
	settings := &models.ChatbotSettings{
		AI: models.AIConfig{
			APIKey:    "test-api-key",
			Model:     "qwen3.7-plus",
			MaxTokens: 500,
		},
	}

	_, err := app.generateQwenResponse(settings, nil, "Hello", "")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "response exceeded the size limit")
}

package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const chatbotTestEncryptionKey = "chatbot-outbound-test-encryption-key-32-bytes"

type chatbotRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn chatbotRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func chatbotContractTestApp(client *http.Client) *App {
	return &App{
		Config:     &config.Config{App: config.AppConfig{EncryptionKey: chatbotTestEncryptionKey}},
		HTTPClient: client,
		Log:        testutil.NopLogger(),
	}
}

func TestChatbotHeadersAreAllEncryptedMaskedAndReplaceable(t *testing.T) {
	app := chatbotContractTestApp(&http.Client{})
	stored, err := app.protectChatbotHeaders(map[string]string{
		"Authorization": "Bearer synthetic-token",
		"X-Trace-ID":    "synthetic-credential-in-custom-header",
	}, nil)
	require.NoError(t, err)

	for _, name := range []string{"Authorization", "X-Trace-ID"} {
		value, ok := stored[name].(string)
		require.True(t, ok)
		assert.True(t, appcrypto.IsEncrypted(value), name)
		assert.NotContains(t, value, "synthetic")
	}
	assert.Equal(t, map[string]string{
		"Authorization": outboundHeaderMask,
		"X-Trace-ID":    outboundHeaderMask,
	}, redactChatbotHeaders(stored))

	updated, err := app.protectChatbotHeaders(map[string]string{
		"authorization": outboundHeaderMask,
	}, stored)
	require.NoError(t, err)
	assert.Equal(t, stored["Authorization"], updated["authorization"])
	_, retainedDeletedHeader := updated["X-Trace-ID"]
	assert.False(t, retainedDeletedHeader, "omitted headers must be deleted")

	legacyUpdated, err := app.protectChatbotHeaders(map[string]string{
		"X-Legacy": outboundHeaderMask,
	}, models.JSONB{"x-legacy": "legacy-synthetic-secret"})
	require.NoError(t, err)
	legacyStored, ok := legacyUpdated["X-Legacy"].(string)
	require.True(t, ok)
	assert.True(t, appcrypto.IsEncrypted(legacyStored))

	noKeyApp := &App{Config: &config.Config{}}
	_, err = noKeyApp.protectChatbotHeaders(map[string]string{"X-Any": "synthetic"}, nil)
	assert.ErrorIs(t, err, errOutboundHeaderEncryptionUnavailable)
}

func TestChatbotFlowGraphProtectsAndRedactsAPINodeHeaders(t *testing.T) {
	app := chatbotContractTestApp(&http.Client{})
	graph := models.JSONB{
		"version":    2,
		"entry_node": "api",
		"nodes": []any{
			map[string]any{
				"id":   "api",
				"type": "api_call",
				"config": map[string]any{
					"url":     "https://api.example.test/v1/{{customer_id}}",
					"method":  "GET",
					"headers": map[string]any{"X-Custom": "synthetic-secret"},
				},
			},
		},
		"edges": []any{},
	}

	protected, err := app.protectChatbotFlowGraph(graph, nil)
	require.NoError(t, err)
	configs, err := chatbotFlowNodeConfigs(protected)
	require.NoError(t, err)
	configKey := chatbotFlowNodeConfigKey(string(ChatNodeAPICall), "api")
	storedHeaders, err := outboundHeaderStrings(configs[configKey]["headers"])
	require.NoError(t, err)
	assert.True(t, appcrypto.IsEncrypted(storedHeaders["X-Custom"]))
	assert.True(t, chatbotFlowCacheSafe(models.ChatbotFlow{Graph: protected}))

	redacted := redactChatbotFlowGraph(protected)
	redactedConfigs, err := chatbotFlowNodeConfigs(redacted)
	require.NoError(t, err)
	redactedHeaders, err := outboundHeaderStrings(redactedConfigs[configKey]["headers"])
	require.NoError(t, err)
	assert.Equal(t, outboundHeaderMask, redactedHeaders["X-Custom"])
	assert.NotContains(t, string(mustJSON(t, redacted)), "enc:")

	roundTripped, err := app.protectChatbotFlowGraph(redacted, protected)
	require.NoError(t, err)
	roundTrippedConfigs, err := chatbotFlowNodeConfigs(roundTripped)
	require.NoError(t, err)
	roundTrippedHeaders, err := outboundHeaderStrings(roundTrippedConfigs[configKey]["headers"])
	require.NoError(t, err)
	assert.Equal(t, storedHeaders["X-Custom"], roundTrippedHeaders["X-Custom"])

	plaintextGraph := models.JSONB{
		"nodes": []any{map[string]any{
			"id": "legacy", "type": "webhook",
			"config": map[string]any{
				"url":     "https://hooks.example.test/inbound",
				"headers": map[string]any{"X-Custom": "legacy-plaintext"},
			},
		}},
	}
	assert.False(t, chatbotFlowCacheSafe(models.ChatbotFlow{Graph: plaintextGraph}))
}

func TestChatbotOutboundURLValidationAtSave(t *testing.T) {
	app := chatbotContractTestApp(&http.Client{})
	for _, rawURL := range []string{
		"http://api.example.test/context",
		"https://127.0.0.1/context",
		"https://localhost/context",
		"https://user:pass@api.example.test/context",
	} {
		t.Run(rawURL, func(t *testing.T) {
			_, err := app.protectChatbotAPIConfig(models.JSONB{"url": rawURL}, nil)
			assert.Error(t, err)
		})
	}

	_, err := app.protectChatbotAPIConfig(models.JSONB{
		"url": "https://api.example.test/context/{{customer_id}}",
	}, nil)
	assert.NoError(t, err)
}

func TestChatbotExecutableConfigsRequireURLs(t *testing.T) {
	t.Parallel()

	assert.NoError(t, validateChatbotCompletionAction("none", nil))
	assert.NoError(t, validateChatbotCompletionAction("webhook", models.JSONB{
		"url": "https://hooks.example.test/completed",
	}))
	assert.ErrorContains(t, validateChatbotCompletionAction("webhook", models.JSONB{}), "URL is required")
	assert.ErrorContains(t, validateChatbotCompletionAction("unknown", models.JSONB{}), "none or webhook")

	assert.NoError(t, validateAIContextOutboundConfig(models.ContextTypeStatic, nil))
	assert.NoError(t, validateAIContextOutboundConfig(models.ContextTypeAPI, models.JSONB{
		"url": "https://api.example.test/context",
	}))
	assert.ErrorContains(t, validateAIContextOutboundConfig(models.ContextTypeAPI, models.JSONB{}), "URL is required")
	assert.ErrorContains(t, validateAIContextOutboundConfig(models.ContextType("unknown"), nil), "static or api")
}

func TestChatbotConfiguredMethodAndHeadersAreValidatedAtSave(t *testing.T) {
	app := chatbotContractTestApp(&http.Client{})
	for _, method := range []string{"GET", "POST", "PUT", "PATCH", "post"} {
		t.Run("allow_"+method, func(t *testing.T) {
			stored, err := app.protectChatbotAPIConfig(models.JSONB{
				"url":    "https://api.example.test/context",
				"method": method,
			}, nil)
			require.NoError(t, err)
			assert.Equal(t, strings.ToUpper(method), stored["method"])
		})
	}
	for _, method := range []string{"DELETE", "TRACE", "CONNECT", "POST\r\nX-Injected: yes"} {
		t.Run("reject_"+method, func(t *testing.T) {
			_, err := app.protectChatbotAPIConfig(models.JSONB{
				"url":    "https://api.example.test/context",
				"method": method,
			}, nil)
			assert.Error(t, err)
		})
	}

	_, err := app.protectChatbotAPIConfig(models.JSONB{
		"url": "https://api.example.test/context",
		"headers": map[string]any{
			"Bad Header": "synthetic",
		},
	}, nil)
	assert.Error(t, err)

	_, err = app.protectChatbotAPIConfig(models.JSONB{
		"url": "https://api.example.test/context",
		"headers": map[string]any{
			"X-Safe": "safe\r\nX-Injected: yes",
		},
	}, nil)
	assert.Error(t, err)

	_, err = app.protectChatbotAPIConfig(models.JSONB{
		"url": "https://api.example.test/context",
		"headers": map[string]any{
			"X-Duplicate": "one",
			"x-duplicate": "two",
		},
	}, nil)
	assert.Error(t, err)

	_, err = app.protectChatbotAPIConfig(models.JSONB{
		"url": "https://api.example.test/context",
		"headers": map[string]any{
			"X-Control": "invalid\x01value",
		},
	}, nil)
	assert.Error(t, err)
}

func TestExecuteConfiguredAPIDecryptsAtBoundaryAndRejectsRedirects(t *testing.T) {
	var requests []*http.Request
	client := &http.Client{Transport: chatbotRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests = append(requests, req.Clone(req.Context()))
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": []string{"https://redirect.example.test/target"}},
			Body:       io.NopCloser(strings.NewReader("redirect")),
			Request:    req,
		}, nil
	})}
	app := chatbotContractTestApp(client)
	storedHeaders, err := app.protectChatbotHeaders(map[string]string{
		"X-Custom": "Bearer {{token}}",
	}, nil)
	require.NoError(t, err)

	body, status, err := app.executeConfiguredAPI(models.JSONB{
		"url":     "https://api.example.test/v1/{{customer_id}}",
		"method":  "GET",
		"headers": storedHeaders,
	}, func(value string) string {
		return strings.NewReplacer(
			"{{customer_id}}", "customer-42",
			"{{token}}", "synthetic-runtime-token",
		).Replace(value)
	})
	require.NoError(t, err)
	assert.Equal(t, http.StatusFound, status)
	assert.Equal(t, "redirect", string(body))
	require.Len(t, requests, 1, "redirect destination must never be requested")
	assert.Equal(t, "https://api.example.test/v1/customer-42", requests[0].URL.String())
	assert.Equal(t, "Bearer synthetic-runtime-token", requests[0].Header.Get("X-Custom"))
}

func TestExecuteConfiguredAPIFailsClosedBeforeTransport(t *testing.T) {
	called := false
	client := &http.Client{Transport: chatbotRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		called = true
		return nil, nil
	})}
	app := chatbotContractTestApp(client)

	_, _, err := app.executeConfiguredAPI(models.JSONB{
		"url": "https://{{host}}/context",
	}, func(value string) string {
		return strings.ReplaceAll(value, "{{host}}", "127.0.0.1")
	})
	assert.Error(t, err)
	assert.False(t, called)

	app.HTTPClient = &http.Client{}
	_, _, err = app.executeConfiguredAPI(models.JSONB{
		"url": "https://api.example.test/context",
	}, func(value string) string { return value })
	assert.Error(t, err)

	app.HTTPClient = nil
	_, _, err = app.executeConfiguredAPI(models.JSONB{
		"url": "https://api.example.test/context",
	}, func(value string) string { return value })
	assert.Error(t, err)
}

func TestExecuteConfiguredAPIRevalidatesMethodAndRenderedHeaders(t *testing.T) {
	called := false
	app := chatbotContractTestApp(&http.Client{Transport: chatbotRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("must not be called")
	})})

	_, _, err := app.executeConfiguredAPI(models.JSONB{
		"url":    "https://api.example.test/context",
		"method": "TRACE",
	}, func(value string) string { return value })
	assert.Error(t, err)
	assert.False(t, called)

	storedHeaders, err := app.protectChatbotHeaders(map[string]string{
		"X-Safe": "{{dynamic_header}}",
	}, nil)
	require.NoError(t, err)
	_, _, err = app.executeConfiguredAPI(models.JSONB{
		"url":     "https://api.example.test/context",
		"method":  "POST",
		"headers": storedHeaders,
	}, func(value string) string {
		return strings.ReplaceAll(value, "{{dynamic_header}}", "safe\r\nX-Injected: yes")
	})
	assert.Error(t, err)
	assert.False(t, called)
}

func TestExecuteChatbotCompletionWebhookUsesProtectedConfig(t *testing.T) {
	var capturedURL, capturedHeader, capturedBody string
	client := &http.Client{Transport: chatbotRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		capturedURL = req.URL.String()
		capturedHeader = req.Header.Get("X-Custom")
		body, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		capturedBody = string(body)
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    req,
		}, nil
	})}
	app := chatbotContractTestApp(client)
	completionConfig, err := app.protectChatbotAPIConfig(models.JSONB{
		"url":    "https://hooks.example.test/complete/{{customer_id}}",
		"method": "POST",
		"headers": map[string]any{
			"X-Custom": "Bearer {{synthetic_token}}",
		},
		"body": `{"customer":"{{customer_id}}"}`,
	}, nil)
	require.NoError(t, err)
	flow := &models.ChatbotFlow{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OnCompleteAction: "webhook",
		CompletionConfig: completionConfig,
	}
	session := &models.ChatbotSession{
		BaseModel:   models.BaseModel{ID: uuid.New()},
		PhoneNumber: "60120000000",
		SessionData: models.JSONB{
			"customer_id":     "customer-42",
			"synthetic_token": "runtime-token",
		},
	}

	require.NoError(t, app.executeChatbotCompletionWebhook(flow, session))
	assert.Equal(t, "https://hooks.example.test/complete/customer-42", capturedURL)
	assert.Equal(t, "Bearer runtime-token", capturedHeader)
	assert.JSONEq(t, `{"customer":"customer-42"}`, capturedBody)
}

func TestExecuteChatbotCompletionWebhookFailsClosedWithoutLeakingSecrets(t *testing.T) {
	t.Run("transport error is sanitized", func(t *testing.T) {
		app := chatbotContractTestApp(&http.Client{Transport: chatbotRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return nil, errors.New("synthetic-secret-must-not-escape")
		})})
		flow := &models.ChatbotFlow{
			BaseModel:        models.BaseModel{ID: uuid.New()},
			OnCompleteAction: "webhook",
			CompletionConfig: models.JSONB{
				"url": "https://hooks.example.test/secret-path",
			},
		}
		err := app.executeChatbotCompletionWebhook(flow, &models.ChatbotSession{
			BaseModel: models.BaseModel{ID: uuid.New()},
		})
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "synthetic-secret")
		assert.NotContains(t, err.Error(), "secret-path")
	})

	t.Run("unsafe legacy destination never reaches transport", func(t *testing.T) {
		called := false
		app := chatbotContractTestApp(&http.Client{Transport: chatbotRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			called = true
			return nil, errors.New("must not be called")
		})})
		flow := &models.ChatbotFlow{
			BaseModel:        models.BaseModel{ID: uuid.New()},
			OnCompleteAction: "webhook",
			CompletionConfig: models.JSONB{
				"url": "https://127.0.0.1/private",
				"headers": map[string]any{
					"X-Legacy": "legacy-synthetic-secret",
				},
			},
		}
		err := app.executeChatbotCompletionWebhook(flow, &models.ChatbotSession{
			BaseModel: models.BaseModel{ID: uuid.New()},
		})
		require.Error(t, err)
		assert.False(t, called)
		assert.NotContains(t, err.Error(), "legacy-synthetic-secret")
	})
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	return encoded
}

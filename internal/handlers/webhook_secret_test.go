package handlers

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/shridarpatil/whatomate/internal/config"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type webhookRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn webhookRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestDecryptWebhookSecretSupportsEncryptedAndLegacyButFailsWithoutKey(t *testing.T) {
	t.Parallel()
	const encryptionKey = "webhook-encryption-test-key"
	encrypted, err := appcrypto.Encrypt("signing-secret", encryptionKey)
	require.NoError(t, err)

	app := &App{Config: &config.Config{App: config.AppConfig{EncryptionKey: encryptionKey}}}
	actual, err := app.decryptWebhookSecret(encrypted)
	require.NoError(t, err)
	assert.Equal(t, "signing-secret", actual)

	legacy, err := app.decryptWebhookSecret("legacy-plaintext")
	require.NoError(t, err)
	assert.Equal(t, "legacy-plaintext", legacy)

	app.Config.App.EncryptionKey = ""
	_, err = app.decryptWebhookSecret(encrypted)
	require.ErrorContains(t, err, "encryption key")
}

func TestConstantTimeWebhookTokenEqualRejectsEmptyAndMismatchedValues(t *testing.T) {
	t.Parallel()

	assert.True(t, constantTimeWebhookTokenEqual("synthetic-verify-token", "synthetic-verify-token"))
	assert.False(t, constantTimeWebhookTokenEqual("", ""))
	assert.False(t, constantTimeWebhookTokenEqual("synthetic-verify-token", ""))
	assert.False(t, constantTimeWebhookTokenEqual("synthetic-verify-token", "different-token"))
	assert.False(t, constantTimeWebhookTokenEqual("same-length-a", "same-length-b"))
}

func TestSendWebhookRequestDoesNotReplayCredentialsAcrossRedirect(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	client := &http.Client{Transport: webhookRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		assert.Equal(t, "https://hooks.example.com/source", r.URL.String())
		assert.Equal(t, "Bearer tenant-secret", r.Header.Get("Authorization"))
		assert.NotEmpty(t, r.Header.Get("X-Webhook-Signature"))
		return &http.Response{
			StatusCode: http.StatusTemporaryRedirect,
			Status:     "307 Temporary Redirect",
			Header: http.Header{
				"Location": []string{"https://destination.example.com/target"},
			},
			Body:    io.NopCloser(strings.NewReader("")),
			Request: r,
		}, nil
	})}

	const encryptionKey = "webhook-redirect-encryption-test-key"
	encrypted, err := appcrypto.Encrypt("signing-secret", encryptionKey)
	require.NoError(t, err)
	app := &App{
		Config:     &config.Config{App: config.AppConfig{EncryptionKey: encryptionKey}},
		HTTPClient: client,
	}
	err = app.sendWebhookRequest(context.Background(), models.Webhook{
		URL: "https://hooks.example.com/source",
		Headers: models.JSONB{
			"Authorization": "Bearer tenant-secret",
		},
		Secret: encrypted,
	}, []byte(`{"event":"test"}`))
	require.Error(t, err)
	var webhookErr *WebhookError
	require.ErrorAs(t, err, &webhookErr)
	assert.Equal(t, http.StatusTemporaryRedirect, webhookErr.StatusCode)
	assert.EqualValues(t, 1, requests.Load(), "redirect destination must never be requested")
}

func TestSendWebhookRequestFailsClosedWithoutGuardedClient(t *testing.T) {
	t.Parallel()

	app := &App{Config: &config.Config{App: config.AppConfig{EncryptionKey: "unused-test-key"}}}
	err := app.sendWebhookRequest(context.Background(), models.Webhook{
		URL: "https://api.example.test/webhook",
	}, []byte(`{"event":"test"}`))
	require.ErrorContains(t, err, "client is not configured")
}

package handlers

import (
	"errors"
	"testing"

	"github.com/shridarpatil/whatomate/internal/config"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOutboundHeadersEncryptRedactResolveAndPreserveMaskedUpdates(t *testing.T) {
	t.Parallel()

	const encryptionKey = "outbound-header-encryption-test-key"
	app := &App{Config: &config.Config{App: config.AppConfig{EncryptionKey: encryptionKey}}}
	stored, err := app.protectOutboundHeaders(map[string]string{
		"Authorization": "Bearer tenant-secret",
		"X-Tenant":      "clinic-42",
		"X-Remove":      "delete-me",
	}, nil)
	require.NoError(t, err)

	storedAuthorization, ok := stored["Authorization"].(string)
	require.True(t, ok)
	assert.True(t, appcrypto.IsEncrypted(storedAuthorization))
	assert.NotContains(t, storedAuthorization, "tenant-secret")
	storedTenant, ok := stored["X-Tenant"].(string)
	require.True(t, ok)
	assert.True(t, appcrypto.IsEncrypted(storedTenant))
	assert.NotContains(t, storedTenant, "clinic-42")

	redacted := redactOutboundHeaders(stored)
	assert.Equal(t, outboundHeaderMask, redacted["Authorization"])
	assert.Equal(t, outboundHeaderMask, redacted["X-Tenant"])
	assert.NotContains(t, redacted["Authorization"], "enc:")

	updated, err := app.protectOutboundHeaders(map[string]string{
		"authorization": outboundHeaderMask,
		"X-Tenant":      "clinic-99",
	}, stored)
	require.NoError(t, err)
	assert.Equal(t, storedAuthorization, updated["authorization"])
	updatedTenant, ok := updated["X-Tenant"].(string)
	require.True(t, ok)
	assert.True(t, appcrypto.IsEncrypted(updatedTenant))
	assert.NotContains(t, updatedTenant, "clinic-99")
	_, retainedRemovedHeader := updated["X-Remove"]
	assert.False(t, retainedRemovedHeader, "omitted headers must be deleted")

	resolved, err := app.resolveOutboundHeaders(updated)
	require.NoError(t, err)
	assert.Equal(t, "Bearer tenant-secret", resolved["authorization"])
	assert.Equal(t, "clinic-99", resolved["X-Tenant"])
}

func TestOutboundHeadersMigrateLegacyPlaintextOnMaskedUpdate(t *testing.T) {
	t.Parallel()

	app := &App{Config: &config.Config{App: config.AppConfig{EncryptionKey: "legacy-migration-key"}}}
	legacy := models.JSONB{"X-Credential": "legacy-plaintext"}
	updated, err := app.protectOutboundHeaders(map[string]string{"x-credential": outboundHeaderMask}, legacy)
	require.NoError(t, err)

	stored, ok := updated["x-credential"].(string)
	require.True(t, ok)
	assert.True(t, appcrypto.IsEncrypted(stored))
	resolved, err := app.resolveOutboundHeaders(updated)
	require.NoError(t, err)
	assert.Equal(t, "legacy-plaintext", resolved["x-credential"])
}

func TestOutboundHeadersFailClosedWithoutEncryptionKey(t *testing.T) {
	t.Parallel()

	app := &App{Config: &config.Config{}}
	_, err := app.protectOutboundHeaders(map[string]string{"X-API-Key": "new-secret"}, nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errOutboundHeaderEncryptionUnavailable))

	encrypted, err := appcrypto.Encrypt("stored-secret", "available-at-write-time")
	require.NoError(t, err)
	_, err = app.resolveOutboundHeaders(models.JSONB{"X-API-Key": encrypted})
	require.Error(t, err)
	assert.True(t, errors.Is(err, errOutboundHeaderEncryptionUnavailable))

	// Existing masked ciphertext may still round-trip unchanged even if the key
	// is temporarily unavailable; it is never exposed or overwritten.
	preserved, err := app.protectOutboundHeaders(
		map[string]string{"x-api-key": outboundHeaderMask},
		models.JSONB{"X-API-Key": encrypted},
	)
	require.NoError(t, err)
	assert.Equal(t, encrypted, preserved["x-api-key"])

	legacy, err := app.resolveOutboundHeaders(models.JSONB{"X-Auth": "legacy-secret"})
	require.NoError(t, err)
	assert.Equal(t, "legacy-secret", legacy["X-Auth"])
}

func TestOutboundHeadersRejectInvalidNamesValuesAndClientCiphertext(t *testing.T) {
	t.Parallel()

	const encryptionKey = "outbound-header-validation-test-key"
	app := &App{Config: &config.Config{App: config.AppConfig{EncryptionKey: encryptionKey}}}

	_, err := app.protectOutboundHeaders(map[string]string{"Bad Header": "value"}, nil)
	require.ErrorContains(t, err, "name")

	_, err = app.protectOutboundHeaders(map[string]string{"X-Custom": "line-one\r\nX-Injected: yes"}, nil)
	require.ErrorContains(t, err, "invalid value")

	ciphertext, err := appcrypto.Encrypt("synthetic", encryptionKey)
	require.NoError(t, err)
	_, err = app.protectOutboundHeaders(map[string]string{"X-Custom": ciphertext}, nil)
	require.ErrorContains(t, err, "plaintext or the mask")

	_, err = app.resolveOutboundHeaders(models.JSONB{"Bad Header": "legacy"})
	require.ErrorContains(t, err, "name")

	_, err = app.protectOutboundHeaders(map[string]string{
		"Authorization": "first",
		"authorization": "second",
	}, nil)
	require.ErrorContains(t, err, "duplicated case-insensitively")

	_, err = app.resolveOutboundHeaders(models.JSONB{
		"X-API-Key": "legacy-one",
		"x-api-key": "legacy-two",
	})
	require.ErrorContains(t, err, "duplicated case-insensitively")
}

func TestWebhookActionConfigHeadersResolveFromStoredNestedJSON(t *testing.T) {
	t.Parallel()

	app := &App{Config: &config.Config{App: config.AppConfig{EncryptionKey: "custom-action-header-key"}}}
	protected, err := app.protectWebhookActionConfig(map[string]any{
		"url":    "https://example.com/hook",
		"method": "POST",
		"headers": map[string]any{
			"Authorization": "Bearer action-secret",
			"X-Tenant":      "relive",
		},
	}, nil)
	require.NoError(t, err)

	resolved, err := app.resolveOutboundHeaders(protected["headers"])
	require.NoError(t, err)
	assert.Equal(t, "Bearer action-secret", resolved["Authorization"])
	assert.Equal(t, "relive", resolved["X-Tenant"])

	responseConfig := redactCustomActionConfig(protected)
	responseHeaders, err := outboundHeaderStrings(responseConfig["headers"])
	require.NoError(t, err)
	assert.Equal(t, outboundHeaderMask, responseHeaders["Authorization"])
	assert.Equal(t, outboundHeaderMask, responseHeaders["X-Tenant"])
}

func TestExecuteWebhookActionFailsClosedWithoutGuardedClient(t *testing.T) {
	t.Parallel()

	app := &App{Config: &config.Config{App: config.AppConfig{EncryptionKey: "unused-test-key"}}}
	_, err := app.executeWebhookAction(models.CustomAction{
		ActionType: models.ActionTypeWebhook,
		Config: models.JSONB{
			"url":     "https://api.example.test/hook",
			"method":  "POST",
			"headers": models.JSONB{},
		},
	}, map[string]any{})
	require.ErrorContains(t, err, "client is not configured")
}

func TestValidateWebhookURLRequiresHTTPSAndRejectsUserInfo(t *testing.T) {
	t.Parallel()

	require.NoError(t, validateWebhookURL("https://api.example.com/hooks"))
	require.ErrorContains(t, validateWebhookURL("http://api.example.com/hooks"), "https")
	require.ErrorContains(t, validateWebhookURL("https://user:pass@api.example.com/hooks"), "user credentials")
	require.ErrorContains(t, validateWebhookURL("https://api.example.com/hooks?token=literal-secret"), "placeholders")
	require.NoError(t, validateWebhookURL("https://api.example.com/hooks?contact_id={{contact.id}}"))
	require.ErrorContains(t, validateWebhookURL("https://api.example.com/hooks#fragment"), "fragment")
}

func TestValidateEventWebhookURLRejectsAllQueries(t *testing.T) {
	t.Parallel()

	require.NoError(t, validateEventWebhookURL("https://hooks.example.com/events/tenant-42"))
	require.ErrorContains(t, validateEventWebhookURL("https://hooks.example.com/events?tenant={{tenant.id}}"), "must not contain query")
	require.ErrorContains(t, validateEventWebhookURL("https://hooks.example.com/events?token=literal"), "must not contain query")
}

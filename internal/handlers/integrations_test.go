package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"gorm.io/gorm"
)

const integrationTestEncryptionKey = "integration-test-encryption-key-long-enough"

func newIntegrationHandlerTestApp(t *testing.T, encryptionKey string) *App {
	t.Helper()
	return &App{
		Config: &config.Config{
			App: config.AppConfig{EncryptionKey: encryptionKey},
			WhatsApp: config.WhatsAppConfig{
				BaseURL:            "https://graph.facebook.com",
				APIVersion:         "v21.0",
				WebhookVerifyToken: "integration-test-platform-webhook-token",
			},
		},
		DB:  testutil.SetupTestDB(t),
		Log: testutil.NopLogger(),
	}
}

func integrationTestUser(t *testing.T, app *App, orgID uuid.UUID, keys ...string) *models.User {
	t.Helper()
	role := testutil.CreateTestRoleWithKeys(t, app.DB, orgID, "integration-admin", keys)
	return testutil.CreateTestUser(t, app.DB, orgID, testutil.WithRoleID(&role.ID))
}

func TestIntegrationCenterMetaUsesRuntimeSettingsAndNeverReturnsSecret(t *testing.T) {
	app := newIntegrationHandlerTestApp(t, integrationTestEncryptionKey)
	org := testutil.CreateTestOrganization(t, app.DB)
	admin := integrationTestUser(
		t,
		app,
		org.ID,
		models.ResourceSettingsIntegrations+":"+models.ActionRead,
		models.ResourceSettingsIntegrations+":"+models.ActionWrite,
	)
	secret := "META-INTEGRATION-SECRET-DO-NOT-RETURN"
	verifyToken := "META-WEBHOOK-VERIFY-TOKEN-DO-NOT-RETURN"

	req := testutil.NewJSONRequest(t, map[string]any{
		"enabled": true,
		"config": map[string]any{
			"app_id":    "123456789",
			"config_id": "987654321",
		},
		"credentials": map[string]any{
			"app_secret":           secret,
			"webhook_verify_token": verifyToken,
		},
	})
	testutil.SetAuthContext(req, org.ID, admin.ID)
	testutil.SetPathParam(req, "provider", integrationProviderMeta)
	require.NoError(t, app.UpdateIntegration(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
	assert.NotContains(t, string(testutil.GetResponseBody(req)), secret)
	assert.NotContains(t, string(testutil.GetResponseBody(req)), verifyToken)
	var saved IntegrationResponse
	testutil.ParseEnvelopeResponse(t, req, &saved)
	assert.True(t, saved.Credentials["webhook_verify_token"].Configured)
	assert.Equal(t, "workspace", saved.Credentials["webhook_verify_token"].Source)
	assert.Equal(t, "workspace", saved.Config["management_mode"])
	assert.Equal(t, "/api/webhook?workspace="+org.ID.String(), saved.Config["webhook_callback_path"])

	var storedOrg models.Organization
	require.NoError(t, app.DB.First(&storedOrg, "id = ?", org.ID).Error)
	storedSecret, _ := storedOrg.Settings["meta_app_secret_encrypted"].(string)
	storedVerifyToken, _ := storedOrg.Settings[metaWebhookVerifyTokenSetting].(string)
	assert.True(t, appcrypto.IsEncrypted(storedSecret))
	assert.True(t, appcrypto.IsEncrypted(storedVerifyToken))
	assert.NotEqual(t, secret, storedSecret)
	assert.NotEqual(t, verifyToken, storedVerifyToken)

	appID, resolvedSecret, configID, err := app.resolveMetaAppCreds(org.ID)
	require.NoError(t, err)
	assert.Equal(t, "123456789", appID)
	assert.Equal(t, "987654321", configID)
	assert.Equal(t, secret, resolvedSecret)

	getReq := testutil.NewGETRequest(t)
	testutil.SetAuthContext(getReq, org.ID, admin.ID)
	require.NoError(t, app.GetIntegrations(getReq))
	assert.NotContains(t, string(testutil.GetResponseBody(getReq)), secret)
	assert.NotContains(t, string(testutil.GetResponseBody(getReq)), storedSecret)
	assert.NotContains(t, string(testutil.GetResponseBody(getReq)), verifyToken)
	assert.NotContains(t, string(testutil.GetResponseBody(getReq)), storedVerifyToken)

	var row models.ProviderIntegration
	require.NoError(t, app.DB.Where("organization_id = ? AND provider = ?", org.ID, integrationProviderMeta).First(&row).Error)
	var auditRows []models.AuditLog
	require.NoError(t, app.DB.Where("organization_id = ? AND resource_id = ?", org.ID, row.ID).Find(&auditRows).Error)
	require.NotEmpty(t, auditRows)
	auditJSON, err := json.Marshal(auditRows)
	require.NoError(t, err)
	assert.NotContains(t, string(auditJSON), secret)
	assert.NotContains(t, string(auditJSON), storedSecret)
	assert.NotContains(t, string(auditJSON), verifyToken)
	assert.NotContains(t, string(auditJSON), storedVerifyToken)

	configReq := testutil.NewGETRequest(t)
	testutil.SetAuthContext(configReq, org.ID, admin.ID)
	require.NoError(t, app.GetEmbeddedSignupConfig(configReq))
	var publicConfig struct {
		WhatsAppAppID    string `json:"whatsapp_app_id"`
		WhatsAppConfigID string `json:"whatsapp_config_id"`
		HasAppSecret     bool   `json:"has_app_secret"`
	}
	testutil.ParseEnvelopeResponse(t, configReq, &publicConfig)
	assert.Equal(t, "123456789", publicConfig.WhatsAppAppID)
	assert.Equal(t, "987654321", publicConfig.WhatsAppConfigID)
	assert.True(t, publicConfig.HasAppSecret)
}

func TestIntegrationCenterBlankSecretPreservesAndDeleteDisablesMeta(t *testing.T) {
	app := newIntegrationHandlerTestApp(t, integrationTestEncryptionKey)
	org := testutil.CreateTestOrganization(t, app.DB)
	admin := integrationTestUser(t, app, org.ID, "settings.integrations:read", "settings.integrations:write")

	create := testutil.NewJSONRequest(t, map[string]any{
		"enabled": true,
		"config":  map[string]any{"app_id": "12345678", "config_id": "87654321"},
		"credentials": map[string]any{
			"app_secret":           "original-meta-secret",
			"webhook_verify_token": "original-webhook-verify-token",
		},
	})
	testutil.SetAuthContext(create, org.ID, admin.ID)
	testutil.SetPathParam(create, "provider", integrationProviderMeta)
	require.NoError(t, app.UpdateIntegration(create))

	var before models.Organization
	require.NoError(t, app.DB.First(&before, "id = ?", org.ID).Error)
	beforeCiphertext, _ := before.Settings["meta_app_secret_encrypted"].(string)

	blank := testutil.NewJSONRequest(t, map[string]any{
		"credentials": map[string]any{"app_secret": "   "},
	})
	testutil.SetAuthContext(blank, org.ID, admin.ID)
	testutil.SetPathParam(blank, "provider", integrationProviderMeta)
	require.NoError(t, app.UpdateIntegration(blank))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(blank))
	var afterBlank models.Organization
	require.NoError(t, app.DB.First(&afterBlank, "id = ?", org.ID).Error)
	afterCiphertext, _ := afterBlank.Settings["meta_app_secret_encrypted"].(string)
	assert.Equal(t, beforeCiphertext, afterCiphertext)

	clearReq := testutil.NewRequest(t)
	testutil.SetAuthContext(clearReq, org.ID, admin.ID)
	testutil.SetPathParam(clearReq, "provider", integrationProviderMeta)
	require.NoError(t, app.DeleteIntegrationCredentials(clearReq))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(clearReq))
	var row models.ProviderIntegration
	require.NoError(t, app.DB.Where("organization_id = ? AND provider = ?", org.ID, integrationProviderMeta).First(&row).Error)
	assert.False(t, row.Enabled)
	var afterClear models.Organization
	require.NoError(t, app.DB.First(&afterClear, "id = ?", org.ID).Error)
	_, exists := afterClear.Settings["meta_app_secret_encrypted"]
	assert.False(t, exists)
	_, exists = afterClear.Settings[metaWebhookVerifyTokenSetting]
	assert.False(t, exists)
	_, _, _, err := app.resolveMetaAppCreds(org.ID)
	assert.ErrorIs(t, err, errMetaIntegrationDisabled)
}

func TestIntegrationCenterFailsClosedWithoutEncryptionKey(t *testing.T) {
	app := newIntegrationHandlerTestApp(t, "")
	org := testutil.CreateTestOrganization(t, app.DB)
	admin := integrationTestUser(t, app, org.ID, "settings.integrations:write")
	req := testutil.NewJSONRequest(t, map[string]any{
		"enabled":     true,
		"config":      map[string]any{"model": "qwen3.7-plus"},
		"credentials": map[string]any{"api_key": "must-never-be-plaintext"},
	})
	testutil.SetAuthContext(req, org.ID, admin.ID)
	testutil.SetPathParam(req, "provider", integrationProviderQwen)
	require.NoError(t, app.UpdateIntegration(req))
	testutil.AssertErrorResponse(t, req, fasthttp.StatusServiceUnavailable, "server-side encryption is not configured")

	var integrationCount, copilotCount int64
	require.NoError(t, app.DB.Model(&models.ProviderIntegration{}).Where("organization_id = ?", org.ID).Count(&integrationCount).Error)
	require.NoError(t, app.DB.Model(&models.CopilotSettings{}).Where("organization_id = ?", org.ID).Count(&copilotCount).Error)
	assert.Zero(t, integrationCount)
	assert.Zero(t, copilotCount)
}

func TestIntegrationCenterEncryptedCredentialsWithoutServerKeyAreDegraded(t *testing.T) {
	app := newIntegrationHandlerTestApp(t, "")
	org := testutil.CreateTestOrganization(t, app.DB)
	admin := integrationTestUser(t, app, org.ID, "settings.integrations:read", "settings.integrations:write")
	metaSecret, err := appcrypto.Encrypt("stored-meta-secret", integrationTestEncryptionKey)
	require.NoError(t, err)
	qwenKey, err := appcrypto.Encrypt("stored-qwen-key", integrationTestEncryptionKey)
	require.NoError(t, err)

	org.Settings = models.JSONB{
		"meta_app_id":                 "123456789",
		"meta_config_id":              "987654321",
		"meta_app_secret_encrypted":   metaSecret,
		metaWebhookVerifyTokenSetting: metaSecret,
	}
	require.NoError(t, app.DB.Model(org).Update("settings", org.Settings).Error)
	require.NoError(t, app.DB.Create(&models.CopilotSettings{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  org.ID,
		Provider:        models.AIProviderQwen,
		Model:           "qwen3.7-plus",
		APIKeyEncrypted: qwenKey,
		MaxTokens:       700,
		Temperature:     0.3,
		IsEnabled:       true,
	}).Error)
	for _, provider := range []string{integrationProviderMeta, integrationProviderQwen} {
		require.NoError(t, app.DB.Create(&models.ProviderIntegration{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: org.ID,
			Provider:       provider,
			Enabled:        true,
			Config:         models.JSONB{},
			CredentialData: models.JSONB{},
		}).Error)
	}

	get := testutil.NewGETRequest(t)
	testutil.SetAuthContext(get, org.ID, admin.ID)
	require.NoError(t, app.GetIntegrations(get))
	var catalog struct {
		Integrations []IntegrationResponse `json:"integrations"`
	}
	testutil.ParseEnvelopeResponse(t, get, &catalog)
	for _, provider := range []string{integrationProviderMeta, integrationProviderQwen} {
		var found *IntegrationResponse
		for index := range catalog.Integrations {
			if catalog.Integrations[index].Provider == provider {
				found = &catalog.Integrations[index]
				break
			}
		}
		require.NotNil(t, found)
		assert.True(t, found.Configured, provider)
		assert.Equal(t, integrationStatusDegraded, found.Status, provider)
		assert.NotEmpty(t, found.Message, provider)
		if provider == integrationProviderMeta {
			assert.False(t, found.OAuth.Available)
		}

		connect := testutil.NewRequest(t)
		testutil.SetAuthContext(connect, org.ID, admin.ID)
		testutil.SetPathParam(connect, "provider", provider)
		require.NoError(t, app.ConnectIntegration(connect))
		testutil.AssertErrorResponse(t, connect, fasthttp.StatusServiceUnavailable, "stored integration credential is unavailable")
	}
}

func TestIntegrationCenterPlatformMetaSecretDoesNotRequireWorkspaceEncryptionKey(t *testing.T) {
	app := &App{Config: &config.Config{
		WhatsApp: config.WhatsAppConfig{
			AppID:              "platform-app-id",
			ConfigID:           "platform-config-id",
			AppSecret:          "platform-secret",
			WebhookVerifyToken: "platform-webhook-token",
		},
	}}
	row := &models.ProviderIntegration{Provider: integrationProviderMeta, Enabled: true}
	response := app.composeIntegrationResponse(integrationProviderMeta, &integrationSources{
		organization: models.Organization{Settings: models.JSONB{}},
		rows:         map[string]*models.ProviderIntegration{integrationProviderMeta: row},
		connections:  map[string]integrationConnectionResponse{},
	})
	assert.True(t, response.Configured)
	assert.True(t, response.credentialUsable)
	assert.True(t, response.OAuth.Available)
	assert.Equal(t, integrationStatusConfigured, response.Status)
	assert.Equal(t, "platform", response.Credentials["webhook_verify_token"].Source)
	assert.Equal(t, "/api/webhook", response.Config["webhook_callback_path"])
}

func TestIntegrationCenterRejectsWorkspaceWebhookTokenForPlatformMetaApp(t *testing.T) {
	app := newIntegrationHandlerTestApp(t, integrationTestEncryptionKey)
	app.Config.WhatsApp.AppID = "platform-app-id"
	app.Config.WhatsApp.ConfigID = "platform-config-id"
	app.Config.WhatsApp.AppSecret = "platform-app-secret"
	app.Config.WhatsApp.WebhookVerifyToken = "platform-webhook-verify-token"
	org := testutil.CreateTestOrganization(t, app.DB)
	admin := integrationTestUser(t, app, org.ID, "settings.integrations:write")

	req := testutil.NewJSONRequest(t, map[string]any{
		"credentials": map[string]any{
			"webhook_verify_token": "duplicate-workspace-webhook-token",
		},
	})
	testutil.SetAuthContext(req, org.ID, admin.ID)
	testutil.SetPathParam(req, "provider", integrationProviderMeta)
	require.NoError(t, app.UpdateIntegration(req))
	testutil.AssertErrorResponse(t, req, fasthttp.StatusBadRequest, "deployment-managed webhook verify token")
	assert.NotContains(t, string(testutil.GetResponseBody(req)), "duplicate-workspace-webhook-token")
	assert.NotContains(t, string(testutil.GetResponseBody(req)), "platform-webhook-verify-token")

	var persisted models.Organization
	require.NoError(t, app.DB.First(&persisted, "id = ?", org.ID).Error)
	_, exists := persisted.Settings[metaWebhookVerifyTokenSetting]
	assert.False(t, exists)
}

func TestIntegrationCenterPublishesExactProviderScopeContracts(t *testing.T) {
	app := &App{Config: &config.Config{WhatsApp: config.WhatsAppConfig{APIVersion: "v21.0"}}}
	sources := &integrationSources{
		organization: models.Organization{Settings: models.JSONB{}},
		rows:         map[string]*models.ProviderIntegration{},
		connections:  map[string]integrationConnectionResponse{},
	}

	tests := []struct {
		provider string
		scopes   []string
	}{
		{
			provider: integrationProviderMeta,
			scopes: []string{
				"business_management",
				"whatsapp_business_management",
				"whatsapp_business_messaging",
			},
		},
		{
			provider: integrationProviderThreads,
			scopes: []string{
				"threads_basic",
				"threads_read_replies",
				"threads_manage_replies",
				"threads_content_publish",
				"threads_manage_mentions",
			},
		},
		{
			provider: integrationProviderTikTok,
			scopes: []string{
				"message.list.read",
				"message.list.send",
				"message.list.manage",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.provider, func(t *testing.T) {
			response := app.composeIntegrationResponse(test.provider, sources)
			assert.Equal(t, test.scopes, response.RequiredScopes)
			if test.provider == integrationProviderThreads || test.provider == integrationProviderTikTok {
				assert.False(t, response.Enabled)
				assert.False(t, response.OAuth.Available)
				assert.False(t, response.TestSupported)
			}
		})
	}
}

func TestIntegrationCenterQwenDeleteIsAuthoritativeOverLegacyFallback(t *testing.T) {
	app := newIntegrationHandlerTestApp(t, integrationTestEncryptionKey)
	org := testutil.CreateTestOrganization(t, app.DB)
	admin := integrationTestUser(t, app, org.ID, "settings.integrations:read", "settings.integrations:write")
	legacyKey, err := appcrypto.Encrypt("legacy-qwen-secret", integrationTestEncryptionKey)
	require.NoError(t, err)
	require.NoError(t, app.DB.Create(&models.ChatbotSettings{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		IsEnabled:      true,
		AI: models.AIConfig{
			Enabled:  true,
			Provider: models.AIProviderQwen,
			APIKey:   legacyKey,
			Model:    "qwen3.7-plus",
		},
	}).Error)

	update := testutil.NewJSONRequest(t, map[string]any{
		"enabled": true,
		"config": map[string]any{
			"model":       "qwen3.7-plus",
			"max_tokens":  700,
			"temperature": 0.3,
		},
		"credentials": map[string]any{"api_key": "new-qwen-secret"},
	})
	testutil.SetAuthContext(update, org.ID, admin.ID)
	testutil.SetPathParam(update, "provider", integrationProviderQwen)
	require.NoError(t, app.UpdateIntegration(update))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(update))

	clearReq := testutil.NewRequest(t)
	testutil.SetAuthContext(clearReq, org.ID, admin.ID)
	testutil.SetPathParam(clearReq, "provider", integrationProviderQwen)
	require.NoError(t, app.DeleteIntegrationCredentials(clearReq))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(clearReq))

	var dedicated models.CopilotSettings
	require.NoError(t, app.DB.Where("organization_id = ? AND whats_app_account = ''", org.ID).First(&dedicated).Error)
	assert.False(t, dedicated.IsEnabled)
	assert.Empty(t, dedicated.APIKeyEncrypted)
	_, err = app.resolveCopilotQwenSettings(org.ID, "")
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

func TestIntegrationCenterUnavailableProvidersCannotBeEnabled(t *testing.T) {
	for _, testCase := range []struct {
		provider    string
		config      map[string]any
		credentials map[string]any
		status      string
	}{
		{
			provider: integrationProviderThreads,
			config: map[string]any{
				"app_id":            "threads-app-123",
				"redirect_uri":      "https://app.example.test/oauth/threads",
				"app_review_status": "pending",
			},
			credentials: map[string]any{"app_secret": "threads-secret"},
			status:      integrationStatusAdapterMissing,
		},
		{
			provider: integrationProviderTikTok,
			config: map[string]any{
				"client_id":       "tiktok-client-123",
				"redirect_uri":    "https://app.example.test/oauth/tiktok",
				"approval_status": "pending",
			},
			credentials: map[string]any{"client_secret": "tiktok-secret"},
			status:      integrationStatusApprovalNeeded,
		},
	} {
		t.Run(testCase.provider, func(t *testing.T) {
			app := newIntegrationHandlerTestApp(t, integrationTestEncryptionKey)
			org := testutil.CreateTestOrganization(t, app.DB)
			admin := integrationTestUser(t, app, org.ID, "settings.integrations:read", "settings.integrations:write")

			draft := testutil.NewJSONRequest(t, map[string]any{
				"config":      testCase.config,
				"credentials": testCase.credentials,
			})
			testutil.SetAuthContext(draft, org.ID, admin.ID)
			testutil.SetPathParam(draft, "provider", testCase.provider)
			require.NoError(t, app.UpdateIntegration(draft))
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(draft))

			enable := testutil.NewJSONRequest(t, map[string]any{"enabled": true})
			testutil.SetAuthContext(enable, org.ID, admin.ID)
			testutil.SetPathParam(enable, "provider", testCase.provider)
			require.NoError(t, app.UpdateIntegration(enable))
			testutil.AssertErrorResponse(t, enable, fasthttp.StatusConflict, "cannot be enabled")

			var row models.ProviderIntegration
			require.NoError(t, app.DB.Where("organization_id = ? AND provider = ?", org.ID, testCase.provider).First(&row).Error)
			assert.False(t, row.Enabled)
			for _, value := range row.CredentialData {
				text, _ := value.(string)
				assert.True(t, appcrypto.IsEncrypted(text))
			}

			response, err := app.integrationResponse(org.ID, testCase.provider)
			require.NoError(t, err)
			assert.False(t, response.Enabled)
			assert.False(t, response.TestSupported)
			assert.False(t, response.OAuth.Available)
			assert.Equal(t, testCase.status, response.Status)
		})
	}
}

func TestIntegrationCenterStrictValidationAndPermissions(t *testing.T) {
	app := newIntegrationHandlerTestApp(t, integrationTestEncryptionKey)
	org := testutil.CreateTestOrganization(t, app.DB)
	reader := integrationTestUser(t, app, org.ID, "settings.integrations:read")

	forbidden := testutil.NewJSONRequest(t, map[string]any{"enabled": false})
	testutil.SetAuthContext(forbidden, org.ID, reader.ID)
	testutil.SetPathParam(forbidden, "provider", integrationProviderMeta)
	require.NoError(t, app.UpdateIntegration(forbidden))
	testutil.AssertErrorResponse(t, forbidden, fasthttp.StatusForbidden, "Insufficient permissions")

	admin := integrationTestUser(t, app, org.ID, "settings.integrations:read", "settings.integrations:write")
	unknownTop := testutil.NewJSONRequest(t, map[string]any{"enabled": false, "access_token": "must-not-be-accepted"})
	testutil.SetAuthContext(unknownTop, org.ID, admin.ID)
	testutil.SetPathParam(unknownTop, "provider", integrationProviderMeta)
	require.NoError(t, app.UpdateIntegration(unknownTop))
	testutil.AssertErrorResponse(t, unknownTop, fasthttp.StatusBadRequest, "Invalid request body")

	unknownClear := testutil.NewJSONRequest(t, map[string]any{"clear_credentials": []string{"access_token"}})
	testutil.SetAuthContext(unknownClear, org.ID, admin.ID)
	testutil.SetPathParam(unknownClear, "provider", integrationProviderMeta)
	require.NoError(t, app.UpdateIntegration(unknownClear))
	testutil.AssertErrorResponse(t, unknownClear, fasthttp.StatusBadRequest, "unsupported field")

	inReview := testutil.NewJSONRequest(t, map[string]any{
		"config": map[string]any{"approval_status": "in_review"},
	})
	testutil.SetAuthContext(inReview, org.ID, admin.ID)
	testutil.SetPathParam(inReview, "provider", integrationProviderTikTok)
	require.NoError(t, app.UpdateIntegration(inReview))
	testutil.AssertErrorResponse(t, inReview, fasthttp.StatusBadRequest, "not_submitted, pending, approved, or rejected")
}

func TestIntegrationCenterDoesNotLeakAnotherOrganization(t *testing.T) {
	app := newIntegrationHandlerTestApp(t, integrationTestEncryptionKey)
	orgA := testutil.CreateTestOrganization(t, app.DB)
	orgB := testutil.CreateTestOrganization(t, app.DB)
	adminA := integrationTestUser(t, app, orgA.ID, "settings.integrations:read")
	secretB, err := appcrypto.Encrypt("other-tenant-secret", integrationTestEncryptionKey)
	require.NoError(t, err)
	require.NoError(t, app.DB.Create(&models.ProviderIntegration{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgB.ID,
		Provider:       integrationProviderTikTok,
		Config: models.JSONB{
			"client_id":       "other-tenant-client-id",
			"redirect_uri":    "https://other.example.test/callback",
			"approval_status": "pending",
		},
		CredentialData: models.JSONB{"client_secret": secretB},
	}).Error)

	req := testutil.NewGETRequest(t)
	testutil.SetAuthContext(req, orgA.ID, adminA.ID)
	testutil.SetHeader(req, "X-Organization-ID", orgB.ID.String())
	require.NoError(t, app.GetIntegrations(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
	body := string(testutil.GetResponseBody(req))
	assert.NotContains(t, body, "other-tenant-client-id")
	assert.NotContains(t, body, "other-tenant-secret")
	assert.NotContains(t, body, secretB)
}

func TestIntegrationCenterSystemRoleAssignmentIsAdminOnly(t *testing.T) {
	adminPermissions := models.SystemRolePermissions()["admin"]
	managerPermissions := models.SystemRolePermissions()["manager"]
	agentPermissions := models.SystemRolePermissions()["agent"]
	assert.Contains(t, adminPermissions, "settings.integrations:read")
	assert.Contains(t, adminPermissions, "settings.integrations:write")
	assert.NotContains(t, managerPermissions, "settings.integrations:read")
	assert.NotContains(t, managerPermissions, "settings.integrations:write")
	assert.NotContains(t, agentPermissions, "settings.integrations:read")
	assert.NotContains(t, agentPermissions, "settings.integrations:write")
}

func TestIntegrationCenterResponseShapeNeverContainsCredentialFields(t *testing.T) {
	value := IntegrationResponse{
		Provider:    integrationProviderMeta,
		Credentials: map[string]integrationCredentialResponse{"app_secret": {Configured: true}},
	}
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	text := string(encoded)
	assert.Contains(t, text, `"configured":true`)
	for _, forbidden := range []string{"access_token", "refresh_token", "client_secret\":\"", "app_secret\":\""} {
		assert.False(t, strings.Contains(text, forbidden), text)
	}
}

func TestQwenPublicBaseURLRedactsURLCredentialsAndQuery(t *testing.T) {
	app := &App{Config: &config.Config{AI: config.AIConfig{
		QwenBaseURL: "https://tenant-user:tenant-password@qwen.example.test/v1?api_key=hidden#fragment",
	}}}
	assert.Equal(t, "https://qwen.example.test/v1", app.qwenPublicBaseURL())
}

func TestQwenEndpointRegionValidationUsesOfficialAllowlist(t *testing.T) {
	t.Parallel()

	for _, region := range []string{"platform", "singapore", "us", "china_beijing"} {
		config, err := validateIntegrationConfig(integrationProviderQwen, models.JSONB{
			"endpoint_region": region,
		})
		require.NoError(t, err)
		assert.Equal(t, region, config["endpoint_region"])
	}

	_, err := validateIntegrationConfig(integrationProviderQwen, models.JSONB{
		"endpoint_region": "https://tenant.example.test/v1",
	})
	require.EqualError(
		t,
		err,
		"config.endpoint_region must be platform, singapore, us, or china_beijing",
	)
}

func TestIntegrationRedirectURIRejectsValuesOver2048Characters(t *testing.T) {
	_, err := validateIntegrationConfig(integrationProviderThreads, models.JSONB{
		"redirect_uri": "https://example.test/callback/" + strings.Repeat("a", 2049),
	})
	require.EqualError(t, err, "config.redirect_uri must be an HTTPS URL without credentials or a fragment")
}

func TestAccountCredentialEncryptionFailsClosedWithoutServerKey(t *testing.T) {
	app := &App{Config: &config.Config{}}
	account := &models.WhatsAppAccount{AccessToken: "plaintext-access-token"}
	err := app.encryptAccountSecrets(account)
	require.ErrorIs(t, err, errAccountEncryptionUnavailable)
	assert.Equal(t, "plaintext-access-token", account.AccessToken)
}

func TestAccountMutationDoesNotPersistNewSecretsWithoutServerKey(t *testing.T) {
	app := newIntegrationHandlerTestApp(t, "")
	org := testutil.CreateTestOrganization(t, app.DB)
	writer := integrationTestUser(t, app, org.ID, "accounts:read", "accounts:write")

	create := testutil.NewJSONRequest(t, map[string]any{
		"name":         "no-key-create",
		"phone_id":     "no-key-phone",
		"business_id":  "no-key-business",
		"access_token": "must-not-be-persisted",
	})
	testutil.SetAuthContext(create, org.ID, writer.ID)
	require.NoError(t, app.CreateAccount(create))
	testutil.AssertErrorResponse(t, create, fasthttp.StatusServiceUnavailable, "credential storage is unavailable")
	var created int64
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).
		Where("organization_id = ? AND phone_id = ?", org.ID, "no-key-phone").Count(&created).Error)
	assert.Zero(t, created)

	existing := &models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "no-key-existing",
		PhoneID:        "no-key-existing-phone",
		BusinessID:     "no-key-existing-business",
		AccessToken:    "legacy-plaintext-access-token",
		APIVersion:     "v21.0",
		Status:         "active",
	}
	require.NoError(t, app.DB.Create(existing).Error)
	update := testutil.NewJSONRequest(t, map[string]any{"name": "must-not-be-saved"})
	testutil.SetAuthContext(update, org.ID, writer.ID)
	testutil.SetPathParam(update, "id", existing.ID.String())
	require.NoError(t, app.UpdateAccount(update))
	testutil.AssertErrorResponse(t, update, fasthttp.StatusServiceUnavailable, "credential storage is unavailable")
	var persisted models.WhatsAppAccount
	require.NoError(t, app.DB.First(&persisted, "id = ?", existing.ID).Error)
	assert.Equal(t, "no-key-existing", persisted.Name)
	assert.Equal(t, "legacy-plaintext-access-token", persisted.AccessToken)
}

func TestEffectiveMetaCredentialsPreferManagedCenterAndKeepLegacyFallback(t *testing.T) {
	app := newIntegrationHandlerTestApp(t, integrationTestEncryptionKey)
	org := testutil.CreateTestOrganization(t, app.DB)
	account := &models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "legacy-meta-account",
		PhoneID:        "legacy-meta-phone",
		BusinessID:     "legacy-meta-waba",
		AccessToken:    "legacy-access-token",
		AppID:          "legacy-app-id",
		AppSecret:      "legacy-app-secret",
		APIVersion:     "v21.0",
		Status:         "active",
	}
	require.NoError(t, app.DB.Create(account).Error)

	appID, appSecret, _, err := app.resolveEffectiveMetaAppCreds(account)
	require.NoError(t, err)
	assert.Equal(t, "legacy-app-id", appID)
	assert.Equal(t, "legacy-app-secret", appSecret)

	centralSecret, err := appcrypto.Encrypt("central-app-secret", integrationTestEncryptionKey)
	require.NoError(t, err)
	org.Settings = models.JSONB{
		"meta_app_id":               "central-app-id",
		"meta_config_id":            "central-config-id",
		"meta_app_secret_encrypted": centralSecret,
	}
	require.NoError(t, app.DB.Model(org).Update("settings", org.Settings).Error)
	row := &models.ProviderIntegration{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Provider:       integrationProviderMeta,
		Enabled:        true,
		Config:         models.JSONB{},
		CredentialData: models.JSONB{},
	}
	require.NoError(t, app.DB.Create(row).Error)

	appID, appSecret, _, err = app.resolveEffectiveMetaAppCreds(account)
	require.NoError(t, err)
	assert.Equal(t, "central-app-id", appID)
	assert.Equal(t, "central-app-secret", appSecret)

	app.Config.App.EncryptionKey = "wrong-encryption-key"
	_, _, _, err = app.resolveEffectiveMetaAppCreds(account)
	require.Error(t, err, "a managed corrupt/undecryptable secret must not fall back to the legacy account")
	app.Config.App.EncryptionKey = integrationTestEncryptionKey

	require.NoError(t, app.DB.Model(row).Update("enabled", false).Error)
	_, _, _, err = app.resolveEffectiveMetaAppCreds(account)
	require.ErrorIs(t, err, errMetaIntegrationDisabled)
}

func TestEmbeddedSignupAccountDoesNotCopyCentralAppCredentials(t *testing.T) {
	metaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":                   "embedded-phone-id",
			"verified_name":        "Embedded Test",
			"display_phone_number": "+60123456789",
		})
	}))
	defer metaServer.Close()

	app := newIntegrationHandlerTestApp(t, integrationTestEncryptionKey)
	app.WhatsApp = whatsapp.NewWithBaseURL(app.Log, metaServer.URL)
	org := testutil.CreateTestOrganization(t, app.DB)
	account, _, existing, _, err := app.createOrUpdateAccount(
		context.Background(),
		org.ID,
		"embedded-phone-id",
		"embedded-waba-id",
		"Embedded Account",
		"embedded-access-token",
		nil,
	)
	require.NoError(t, err)
	assert.False(t, existing)
	assert.Empty(t, account.AppID)
	assert.Empty(t, account.AppSecret)

	legacySecret, err := appcrypto.Encrypt("legacy-app-secret", integrationTestEncryptionKey)
	require.NoError(t, err)
	account.AppID = "legacy-app-id"
	account.AppSecret = legacySecret
	require.NoError(t, app.encryptAccountSecrets(account))
	require.NoError(t, app.DB.Save(account).Error)

	updated, _, existing, _, err := app.createOrUpdateAccount(
		context.Background(),
		org.ID,
		account.PhoneID,
		account.BusinessID,
		account.Name,
		"replacement-access-token",
		nil,
	)
	require.NoError(t, err)
	assert.True(t, existing)
	assert.Equal(t, "legacy-app-id", updated.AppID)
	assert.Equal(t, legacySecret, updated.AppSecret)
}

func TestLegacyAccountWebhookVerifyTokenNeverReturnsAndNewWritesAreRejected(t *testing.T) {
	app := newIntegrationHandlerTestApp(t, integrationTestEncryptionKey)
	org := testutil.CreateTestOrganization(t, app.DB)
	reader := integrationTestUser(t, app, org.ID, "accounts:read")
	writer := integrationTestUser(t, app, org.ID, "accounts:read", "accounts:write")
	verifyToken := "legacy-webhook-verify-token-must-never-return"
	accessToken, err := appcrypto.Encrypt("test-access-token", integrationTestEncryptionKey)
	require.NoError(t, err)
	account := models.WhatsAppAccount{
		BaseModel:          models.BaseModel{ID: uuid.New()},
		OrganizationID:     org.ID,
		Name:               "legacy-token-account",
		PhoneID:            "legacy-token-phone-id",
		BusinessID:         "legacy-token-business-id",
		AccessToken:        accessToken,
		WebhookVerifyToken: verifyToken,
		APIVersion:         "v21.0",
		Status:             "active",
	}
	require.NoError(t, app.DB.Create(&account).Error)

	readerGet := testutil.NewGETRequest(t)
	testutil.SetAuthContext(readerGet, org.ID, reader.ID)
	testutil.SetPathParam(readerGet, "id", account.ID.String())
	require.NoError(t, app.GetAccount(readerGet))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(readerGet))
	readerGetBody := string(testutil.GetResponseBody(readerGet))
	assert.NotContains(t, readerGetBody, verifyToken)
	assert.NotContains(t, readerGetBody, `"webhook_verify_token"`)
	assert.NotContains(t, readerGetBody, `"has_webhook_verify_token"`)

	readerList := testutil.NewGETRequest(t)
	testutil.SetAuthContext(readerList, org.ID, reader.ID)
	require.NoError(t, app.ListAccounts(readerList))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(readerList))
	readerListBody := string(testutil.GetResponseBody(readerList))
	assert.NotContains(t, readerListBody, verifyToken)
	assert.NotContains(t, readerListBody, `"webhook_verify_token"`)
	assert.NotContains(t, readerListBody, `"has_webhook_verify_token"`)

	writerList := testutil.NewGETRequest(t)
	testutil.SetAuthContext(writerList, org.ID, writer.ID)
	require.NoError(t, app.ListAccounts(writerList))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(writerList))
	writerListBody := string(testutil.GetResponseBody(writerList))
	assert.NotContains(t, writerListBody, verifyToken)
	assert.NotContains(t, writerListBody, `"webhook_verify_token"`)
	assert.NotContains(t, writerListBody, `"has_webhook_verify_token"`)

	writerGet := testutil.NewGETRequest(t)
	testutil.SetAuthContext(writerGet, org.ID, writer.ID)
	testutil.SetPathParam(writerGet, "id", account.ID.String())
	require.NoError(t, app.GetAccount(writerGet))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(writerGet))
	writerGetBody := string(testutil.GetResponseBody(writerGet))
	assert.NotContains(t, writerGetBody, verifyToken)
	assert.NotContains(t, writerGetBody, `"webhook_verify_token"`)
	assert.NotContains(t, writerGetBody, `"has_webhook_verify_token"`)

	create := testutil.NewJSONRequest(t, map[string]any{
		"name":                 "writer-created-account",
		"phone_id":             "writer-created-phone-id",
		"business_id":          "writer-created-business-id",
		"access_token":         "writer-created-access-token",
		"webhook_verify_token": "writer-created-verify-token",
	})
	testutil.SetAuthContext(create, org.ID, writer.ID)
	require.NoError(t, app.CreateAccount(create))
	testutil.AssertErrorResponse(t, create, fasthttp.StatusBadRequest, "managed in Settings > Integrations")

	var rejectedCreates int64
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).
		Where("organization_id = ? AND phone_id = ?", org.ID, "writer-created-phone-id").
		Count(&rejectedCreates).Error)
	assert.Zero(t, rejectedCreates)

	update := testutil.NewJSONRequest(t, map[string]any{
		"name":                 "must-not-change",
		"webhook_verify_token": "replacement-verify-token",
	})
	testutil.SetAuthContext(update, org.ID, writer.ID)
	testutil.SetPathParam(update, "id", account.ID.String())
	require.NoError(t, app.UpdateAccount(update))
	testutil.AssertErrorResponse(t, update, fasthttp.StatusBadRequest, "managed in Settings > Integrations")

	var persisted models.WhatsAppAccount
	require.NoError(t, app.DB.First(&persisted, "id = ?", account.ID).Error)
	assert.Equal(t, "legacy-token-account", persisted.Name)
	assert.Equal(t, verifyToken, persisted.WebhookVerifyToken,
		"the transition must preserve existing account-column tokens without rotating them")
}

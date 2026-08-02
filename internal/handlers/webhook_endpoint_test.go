package handlers_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

// signWebhook returns the X-Hub-Signature-256 header value for the given body
// and app secret, as Meta would compute it.
func signWebhook(body []byte, appSecret string) string {
	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// newAppForWebhook builds an App with config for webhook verification.
func newAppForWebhook(t *testing.T, globalVerifyToken string) *handlers.App {
	t.Helper()
	app := newTestApp(t)
	app.Config = &config.Config{
		App:      config.AppConfig{EncryptionKey: "webhook-test-encryption-key-long-enough"},
		JWT:      config.JWTConfig{Secret: testutil.TestJWTSecret, AccessExpiryMins: 15, RefreshExpiryDays: 7},
		WhatsApp: config.WhatsAppConfig{WebhookVerifyToken: globalVerifyToken, BaseURL: "https://graph.facebook.com", APIVersion: "v18.0"},
	}
	return app
}

func setManagedMetaIntegration(t *testing.T, app *handlers.App, org *models.Organization, secret string, enabled bool) {
	t.Helper()
	encrypted, err := appcrypto.Encrypt(secret, app.Config.App.EncryptionKey)
	require.NoError(t, err)
	org.Settings = models.JSONB{
		"meta_app_id":               "managed-meta-app-id",
		"meta_config_id":            "managed-meta-config-id",
		"meta_app_secret_encrypted": encrypted,
	}
	require.NoError(t, app.DB.Model(org).Update("settings", org.Settings).Error)
	require.NoError(t, app.DB.Create(&models.ProviderIntegration{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Provider:       "meta",
		Enabled:        enabled,
		Config:         models.JSONB{},
		CredentialData: models.JSONB{},
	}).Error)
}

func setManagedMetaWebhookVerifyToken(t *testing.T, app *handlers.App, orgID uuid.UUID, token string) {
	t.Helper()
	encrypted, err := appcrypto.Encrypt(token, app.Config.App.EncryptionKey)
	require.NoError(t, err)

	var organization models.Organization
	require.NoError(t, app.DB.First(&organization, "id = ?", orgID).Error)
	if organization.Settings == nil {
		organization.Settings = models.JSONB{}
	}
	organization.Settings["meta_webhook_verify_token_encrypted"] = encrypted
	require.NoError(t, app.DB.Model(&organization).Update("settings", organization.Settings).Error)
}

// --- WebhookVerify (GET challenge) ---

func TestApp_WebhookVerify_GlobalTokenSucceeds(t *testing.T) {
	app := newAppForWebhook(t, "shared-secret")

	req := testutil.NewGETRequest(t)
	testutil.SetQueryParam(req, "hub.mode", "subscribe")
	testutil.SetQueryParam(req, "hub.verify_token", "shared-secret")
	testutil.SetQueryParam(req, "hub.challenge", "challenge-xyz")

	require.NoError(t, app.WebhookVerify(req))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
	assert.Equal(t, "challenge-xyz", string(testutil.GetResponseBody(req)),
		"verify must echo the hub.challenge value")
}

func TestApp_WebhookVerify_AccountTokenSucceeds(t *testing.T) {
	app := newAppForWebhook(t, "")
	org := testutil.CreateTestOrganization(t, app.DB)
	acc := &models.WhatsAppAccount{
		BaseModel:          models.BaseModel{ID: uuid.New()},
		OrganizationID:     org.ID,
		Name:               "wbk-acc",
		PhoneID:            "phone-1",
		BusinessID:         "biz-1",
		AccessToken:        "tok",
		WebhookVerifyToken: "per-account-token",
		APIVersion:         "v18.0",
		Status:             "active",
	}
	require.NoError(t, app.DB.Create(acc).Error)

	req := testutil.NewGETRequest(t)
	testutil.SetQueryParam(req, "hub.mode", "subscribe")
	testutil.SetQueryParam(req, "hub.verify_token", "per-account-token")
	testutil.SetQueryParam(req, "hub.challenge", "ch-2")

	require.NoError(t, app.WebhookVerify(req))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
	assert.Equal(t, "ch-2", string(testutil.GetResponseBody(req)))
}

func TestApp_WebhookVerify_WorkspaceCentralTokenIsAuthoritative(t *testing.T) {
	app := newAppForWebhook(t, "")
	org := testutil.CreateTestOrganization(t, app.DB)
	setManagedMetaIntegration(t, app, org, "workspace-app-secret", true)
	setManagedMetaWebhookVerifyToken(t, app, org.ID, "workspace-central-verify-token")
	require.NoError(t, app.DB.Create(&models.WhatsAppAccount{
		BaseModel:          models.BaseModel{ID: uuid.New()},
		OrganizationID:     org.ID,
		Name:               "central-token-account-" + uuid.NewString()[:8],
		PhoneID:            "central-token-phone-id",
		BusinessID:         "central-token-business-id",
		AccessToken:        "encrypted-access-token-placeholder",
		WebhookVerifyToken: "legacy-account-verify-token",
		APIVersion:         "v18.0",
		Status:             "active",
	}).Error)

	central := testutil.NewGETRequest(t)
	testutil.SetQueryParam(central, "workspace", org.ID.String())
	testutil.SetQueryParam(central, "hub.mode", "subscribe")
	testutil.SetQueryParam(central, "hub.verify_token", "workspace-central-verify-token")
	testutil.SetQueryParam(central, "hub.challenge", "central-challenge")
	require.NoError(t, app.WebhookVerify(central))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(central))
	assert.Equal(t, "central-challenge", string(testutil.GetResponseBody(central)))
	assert.NotContains(t, string(testutil.GetResponseBody(central)), "workspace-central-verify-token")

	legacy := testutil.NewGETRequest(t)
	testutil.SetQueryParam(legacy, "workspace", org.ID.String())
	testutil.SetQueryParam(legacy, "hub.mode", "subscribe")
	testutil.SetQueryParam(legacy, "hub.verify_token", "legacy-account-verify-token")
	testutil.SetQueryParam(legacy, "hub.challenge", "must-not-echo")
	require.NoError(t, app.WebhookVerify(legacy))
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(legacy),
		"a central workspace token must disable the per-account legacy fallback")
}

func TestApp_WebhookVerify_WorkspaceSelectorEnforcesTenantBoundary(t *testing.T) {
	app := newAppForWebhook(t, "")
	orgA := testutil.CreateTestOrganization(t, app.DB)
	orgB := testutil.CreateTestOrganization(t, app.DB)
	setManagedMetaIntegration(t, app, orgA, "workspace-app-secret-a", true)
	setManagedMetaIntegration(t, app, orgB, "workspace-app-secret-b", true)
	setManagedMetaWebhookVerifyToken(t, app, orgA.ID, "workspace-token-a")
	setManagedMetaWebhookVerifyToken(t, app, orgB.ID, "workspace-token-b")

	crossTenant := testutil.NewGETRequest(t)
	testutil.SetQueryParam(crossTenant, "workspace", orgB.ID.String())
	testutil.SetQueryParam(crossTenant, "hub.mode", "subscribe")
	testutil.SetQueryParam(crossTenant, "hub.verify_token", "workspace-token-a")
	testutil.SetQueryParam(crossTenant, "hub.challenge", "must-not-echo")
	require.NoError(t, app.WebhookVerify(crossTenant))
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(crossTenant))
	assert.NotContains(t, string(testutil.GetResponseBody(crossTenant)), "workspace-token-a")
	assert.NotContains(t, string(testutil.GetResponseBody(crossTenant)), "workspace-token-b")
}

func TestApp_WebhookVerify_WrongModeRejected(t *testing.T) {
	app := newAppForWebhook(t, "shared-secret")

	req := testutil.NewGETRequest(t)
	testutil.SetQueryParam(req, "hub.mode", "ping")
	testutil.SetQueryParam(req, "hub.verify_token", "shared-secret")
	testutil.SetQueryParam(req, "hub.challenge", "x")

	require.NoError(t, app.WebhookVerify(req))
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))
}

func TestApp_WebhookVerify_UnknownTokenRejected(t *testing.T) {
	app := newAppForWebhook(t, "shared-secret")

	req := testutil.NewGETRequest(t)
	testutil.SetQueryParam(req, "hub.mode", "subscribe")
	testutil.SetQueryParam(req, "hub.verify_token", "wrong-secret")
	testutil.SetQueryParam(req, "hub.challenge", "x")

	require.NoError(t, app.WebhookVerify(req))
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))
}

func TestApp_WebhookVerify_EmptyTokenWithEmptyConfigRejected(t *testing.T) {
	// If config token is empty, an empty incoming token must NOT match (otherwise an
	// attacker could send no token at all and pass verification).
	app := newAppForWebhook(t, "")

	// Production code falls back to `WHERE webhook_verify_token = ?` against the
	// accounts table. If a leftover row from another test in the suite happens to
	// have an empty webhook_verify_token, the empty incoming token would match it
	// and the test would be meaningless. Skip in that case rather than mutating
	// shared state — other tests later in the run depend on these rows.
	var leftoverEmpty int64
	require.NoError(t, app.DB.Model(&models.WhatsAppAccount{}).
		Where("webhook_verify_token = ?", "").Count(&leftoverEmpty).Error)
	if leftoverEmpty > 0 {
		t.Skipf("test data has %d account(s) with empty webhook_verify_token; this branch is only meaningful when none exist", leftoverEmpty)
	}

	req := testutil.NewGETRequest(t)
	testutil.SetQueryParam(req, "hub.mode", "subscribe")
	testutil.SetQueryParam(req, "hub.verify_token", "")
	testutil.SetQueryParam(req, "hub.challenge", "x")

	require.NoError(t, app.WebhookVerify(req))
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))
}

// --- WebhookHandler signature verification ---

// makeMessagesPayload returns a minimal but valid messages-event webhook payload
// for the given phone number ID (no actual messages, just metadata).
func makeMessagesPayload(phoneNumberID string) []byte {
	body := map[string]any{
		"object": "whatsapp_business_account",
		"entry": []map[string]any{{
			"id": "WABA-123",
			"changes": []map[string]any{{
				"field": "messages",
				"value": map[string]any{
					"messaging_product": "whatsapp",
					"metadata": map[string]any{
						"display_phone_number": "+1234567890",
						"phone_number_id":      phoneNumberID,
					},
				},
			}},
		}},
	}
	b, _ := json.Marshal(body)
	return b
}

func makeMultiAccountMessagesPayload(phoneNumberIDs ...string) []byte {
	entries := make([]map[string]any, 0, len(phoneNumberIDs))
	for index, phoneNumberID := range phoneNumberIDs {
		entries = append(entries, map[string]any{
			"id": "WABA-" + string(rune('A'+index)),
			"changes": []map[string]any{{
				"field": "messages",
				"value": map[string]any{
					"messaging_product": "whatsapp",
					"metadata": map[string]any{
						"display_phone_number": "+1234567890",
						"phone_number_id":      phoneNumberID,
					},
				},
			}},
		})
	}
	body, _ := json.Marshal(map[string]any{
		"object": "whatsapp_business_account",
		"entry":  entries,
	})
	return body
}

func TestApp_WebhookHandler_NoSignatureNoAppSecret_Rejected(t *testing.T) {
	// No request may be processed when Meta's signature and an app secret are
	// unavailable.
	app := newAppForWebhook(t, "")
	org := testutil.CreateTestOrganization(t, app.DB)
	acc := &models.WhatsAppAccount{
		BaseModel:          models.BaseModel{ID: uuid.New()},
		OrganizationID:     org.ID,
		Name:               "wbk-h-1",
		PhoneID:            "phone-A",
		BusinessID:         "biz-A",
		AccessToken:        "tok",
		WebhookVerifyToken: "vt",
		APIVersion:         "v18.0",
		Status:             "active",
	}
	require.NoError(t, app.DB.Create(acc).Error)

	body := makeMessagesPayload("phone-A")
	req := testutil.NewRequest(t)
	req.RequestCtx.Request.Header.SetMethod("POST")
	req.RequestCtx.Request.Header.SetContentType("application/json")
	req.RequestCtx.Request.SetBody(body)

	require.NoError(t, app.WebhookHandler(req))
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))
}

func TestApp_WebhookHandler_ValidSignature_Accepted(t *testing.T) {
	app := newAppForWebhook(t, "")
	org := testutil.CreateTestOrganization(t, app.DB)
	appSecret := "shhh-app-secret-32-bytes-long-xx"
	acc := &models.WhatsAppAccount{
		BaseModel:          models.BaseModel{ID: uuid.New()},
		OrganizationID:     org.ID,
		Name:               "wbk-h-2",
		PhoneID:            "phone-B",
		BusinessID:         "biz-B",
		AccessToken:        "tok",
		AppSecret:          appSecret,
		WebhookVerifyToken: "vt",
		APIVersion:         "v18.0",
		Status:             "active",
	}
	require.NoError(t, app.DB.Create(acc).Error)

	body := makeMessagesPayload("phone-B")
	sig := signWebhook(body, appSecret)

	req := testutil.NewRequest(t)
	req.RequestCtx.Request.Header.SetMethod("POST")
	req.RequestCtx.Request.Header.SetContentType("application/json")
	req.RequestCtx.Request.Header.Set("X-Hub-Signature-256", sig)
	req.RequestCtx.Request.SetBody(body)

	require.NoError(t, app.WebhookHandler(req))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
}

func TestApp_WebhookHandler_ManagedSecretOverridesLegacyAndUpdatesImmediately(t *testing.T) {
	app := newAppForWebhook(t, "")
	org := testutil.CreateTestOrganization(t, app.DB)
	legacySecret := "legacy-account-secret"
	firstCentralSecret := "first-central-secret"
	secondCentralSecret := "second-central-secret"
	acc := &models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: org.ID,
		Name:           "managed-webhook-account",
		PhoneID:        "managed-webhook-phone",
		BusinessID:     "managed-webhook-waba",
		AccessToken:    "token",
		AppSecret:      legacySecret,
		APIVersion:     "v18.0",
		Status:         "active",
	}
	require.NoError(t, app.DB.Create(acc).Error)
	setManagedMetaIntegration(t, app, org, firstCentralSecret, true)
	body := makeMessagesPayload(acc.PhoneID)

	send := func(secret string) int {
		req := testutil.NewRequest(t)
		req.RequestCtx.Request.Header.SetMethod("POST")
		req.RequestCtx.Request.Header.SetContentType("application/json")
		req.RequestCtx.Request.Header.Set("X-Hub-Signature-256", signWebhook(body, secret))
		req.RequestCtx.Request.SetBody(body)
		require.NoError(t, app.WebhookHandler(req))
		return testutil.GetResponseStatusCode(req)
	}

	assert.Equal(t, fasthttp.StatusOK, send(firstCentralSecret))
	assert.Equal(t, fasthttp.StatusForbidden, send(legacySecret), "managed workspaces must not fall back to the account secret")

	encrypted, err := appcrypto.Encrypt(secondCentralSecret, app.Config.App.EncryptionKey)
	require.NoError(t, err)
	org.Settings["meta_app_secret_encrypted"] = encrypted
	require.NoError(t, app.DB.Model(org).Update("settings", org.Settings).Error)

	assert.Equal(t, fasthttp.StatusOK, send(secondCentralSecret), "central secret changes must take effect without account-cache invalidation")
	assert.Equal(t, fasthttp.StatusForbidden, send(firstCentralSecret))
}

func TestApp_WebhookHandler_DisabledOrUndecryptableManagedSecretNeverFallsBack(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		disableRow bool
		removeKey  bool
	}{
		{name: "disabled", disableRow: true},
		{name: "missing encryption key", removeKey: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			app := newAppForWebhook(t, "")
			org := testutil.CreateTestOrganization(t, app.DB)
			legacySecret := "legacy-secret-must-not-authorize"
			acc := &models.WhatsAppAccount{
				BaseModel:      models.BaseModel{ID: uuid.New()},
				OrganizationID: org.ID,
				Name:           "fail-closed-" + uuid.NewString(),
				PhoneID:        "fail-closed-phone-" + uuid.NewString(),
				BusinessID:     "fail-closed-waba-" + uuid.NewString(),
				AccessToken:    "token",
				AppSecret:      legacySecret,
				APIVersion:     "v18.0",
				Status:         "active",
			}
			require.NoError(t, app.DB.Create(acc).Error)
			setManagedMetaIntegration(t, app, org, "managed-secret", !testCase.disableRow)
			if testCase.removeKey {
				app.Config.App.EncryptionKey = ""
			}

			body := makeMessagesPayload(acc.PhoneID)
			req := testutil.NewRequest(t)
			req.RequestCtx.Request.Header.SetMethod("POST")
			req.RequestCtx.Request.Header.SetContentType("application/json")
			req.RequestCtx.Request.Header.Set("X-Hub-Signature-256", signWebhook(body, legacySecret))
			req.RequestCtx.Request.SetBody(body)
			require.NoError(t, app.WebhookHandler(req))
			assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))
		})
	}
}

func TestApp_WebhookHandler_WABAOnlyUsesManagedOrganizationSecret(t *testing.T) {
	app := newAppForWebhook(t, "")
	org := testutil.CreateTestOrganization(t, app.DB)
	centralSecret := "central-waba-secret"
	wabaID := "managed-waba-only"
	for index := 0; index < 2; index++ {
		require.NoError(t, app.DB.Create(&models.WhatsAppAccount{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: org.ID,
			Name:           "managed-waba-account-" + string(rune('a'+index)),
			PhoneID:        "managed-waba-phone-" + string(rune('a'+index)),
			BusinessID:     wabaID,
			AccessToken:    "token",
			AppSecret:      "stale-legacy-secret-" + string(rune('a'+index)),
			APIVersion:     "v18.0",
			Status:         "active",
		}).Error)
	}
	setManagedMetaIntegration(t, app, org, centralSecret, true)
	body, err := json.Marshal(map[string]any{
		"object": "whatsapp_business_account",
		"entry": []map[string]any{{
			"id": wabaID,
			"changes": []map[string]any{{
				"field": "message_template_status_update",
				"value": map[string]any{
					"event":                     "APPROVED",
					"message_template_name":     "central_template",
					"message_template_language": "en_US",
				},
			}},
		}},
	})
	require.NoError(t, err)
	req := testutil.NewRequest(t)
	req.RequestCtx.Request.Header.SetMethod("POST")
	req.RequestCtx.Request.Header.SetContentType("application/json")
	req.RequestCtx.Request.Header.Set("X-Hub-Signature-256", signWebhook(body, centralSecret))
	req.RequestCtx.Request.SetBody(body)

	require.NoError(t, app.WebhookHandler(req))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
}

func TestApp_WebhookHandler_InvalidSignature_Rejected(t *testing.T) {
	app := newAppForWebhook(t, "")
	org := testutil.CreateTestOrganization(t, app.DB)
	acc := &models.WhatsAppAccount{
		BaseModel:          models.BaseModel{ID: uuid.New()},
		OrganizationID:     org.ID,
		Name:               "wbk-h-3",
		PhoneID:            "phone-C",
		BusinessID:         "biz-C",
		AccessToken:        "tok",
		AppSecret:          "real-secret",
		WebhookVerifyToken: "vt",
		APIVersion:         "v18.0",
		Status:             "active",
	}
	require.NoError(t, app.DB.Create(acc).Error)

	body := makeMessagesPayload("phone-C")
	sig := signWebhook(body, "WRONG-SECRET")

	req := testutil.NewRequest(t)
	req.RequestCtx.Request.Header.SetMethod("POST")
	req.RequestCtx.Request.Header.SetContentType("application/json")
	req.RequestCtx.Request.Header.Set("X-Hub-Signature-256", sig)
	req.RequestCtx.Request.SetBody(body)

	require.NoError(t, app.WebhookHandler(req))
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req),
		"invalid signature must be rejected before any processing")
}

func TestApp_WebhookHandler_OneValidAccountCannotAuthorizeAnother(t *testing.T) {
	app := newAppForWebhook(t, "")
	orgA := testutil.CreateTestOrganization(t, app.DB)
	orgB := testutil.CreateTestOrganization(t, app.DB)
	secretA := "tenant-a-meta-app-secret"
	secretB := "tenant-b-meta-app-secret"
	for _, account := range []models.WhatsAppAccount{
		{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: orgA.ID,
			Name:           "mixed-a",
			PhoneID:        "mixed-phone-a",
			BusinessID:     "mixed-waba-a",
			AccessToken:    "token-a",
			AppSecret:      secretA,
			APIVersion:     "v18.0",
			Status:         "active",
		},
		{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: orgB.ID,
			Name:           "mixed-b",
			PhoneID:        "mixed-phone-b",
			BusinessID:     "mixed-waba-b",
			AccessToken:    "token-b",
			AppSecret:      secretB,
			APIVersion:     "v18.0",
			Status:         "active",
		},
	} {
		account := account
		require.NoError(t, app.DB.Create(&account).Error)
	}

	body := makeMultiAccountMessagesPayload("mixed-phone-a", "mixed-phone-b")
	req := testutil.NewRequest(t)
	req.RequestCtx.Request.Header.SetMethod("POST")
	req.RequestCtx.Request.Header.SetContentType("application/json")
	req.RequestCtx.Request.Header.Set(
		"X-Hub-Signature-256",
		signWebhook(body, secretA),
	)
	req.RequestCtx.Request.SetBody(body)

	require.NoError(t, app.WebhookHandler(req))
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req),
		"one tenant's app secret must not authorize changes for another tenant")
}

func TestApp_WebhookHandler_MalformedJSONRejected(t *testing.T) {
	app := newAppForWebhook(t, "")

	req := testutil.NewRequest(t)
	req.RequestCtx.Request.Header.SetMethod("POST")
	req.RequestCtx.Request.Header.SetContentType("application/json")
	req.RequestCtx.Request.SetBody([]byte("{not json"))

	require.NoError(t, app.WebhookHandler(req))
	assert.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(req))
}

func TestApp_WebhookHandler_BadlyFormattedSignatureRejected(t *testing.T) {
	app := newAppForWebhook(t, "")
	org := testutil.CreateTestOrganization(t, app.DB)
	acc := &models.WhatsAppAccount{
		BaseModel:          models.BaseModel{ID: uuid.New()},
		OrganizationID:     org.ID,
		Name:               "wbk-h-4",
		PhoneID:            "phone-D",
		BusinessID:         "biz-D",
		AccessToken:        "tok",
		AppSecret:          "real-secret",
		WebhookVerifyToken: "vt",
		APIVersion:         "v18.0",
		Status:             "active",
	}
	require.NoError(t, app.DB.Create(acc).Error)

	body := makeMessagesPayload("phone-D")

	req := testutil.NewRequest(t)
	req.RequestCtx.Request.Header.SetMethod("POST")
	req.RequestCtx.Request.Header.SetContentType("application/json")
	// Wrong prefix (not "sha256=")
	req.RequestCtx.Request.Header.Set("X-Hub-Signature-256", "md5=deadbeef")
	req.RequestCtx.Request.SetBody(body)

	require.NoError(t, app.WebhookHandler(req))
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req))
}

func TestApp_WebhookHandler_EmptyEntryRequiresGlobalSignature(t *testing.T) {
	// A payload with no account hint can still be verified using the global
	// Meta app secret.
	app := newAppForWebhook(t, "")
	app.Config.WhatsApp.AppSecret = "global-meta-secret"

	body, _ := json.Marshal(map[string]any{
		"object": "whatsapp_business_account",
		"entry":  []map[string]any{},
	})

	req := testutil.NewRequest(t)
	req.RequestCtx.Request.Header.SetMethod("POST")
	req.RequestCtx.Request.Header.SetContentType("application/json")
	req.RequestCtx.Request.Header.Set("X-Hub-Signature-256", signWebhook(body, app.Config.WhatsApp.AppSecret))
	req.RequestCtx.Request.SetBody(body)

	require.NoError(t, app.WebhookHandler(req))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
}

package handlers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func TestEffectiveMetaCredentialsPreserveLegacyAppWhenPlatformDefaultsAreAdded(t *testing.T) {
	const (
		platformAppID    = "123456789012345"
		platformSecret   = "synthetic-platform-secret"
		platformConfigID = "234567890123456"
		legacyAppID      = "345678901234567"
		legacySecret     = "synthetic-legacy-secret"
		workspaceAppID   = "456789012345678"
		workspaceSecret  = "synthetic-workspace-secret"
		workspaceConfig  = "567890123456789"
	)
	for _, testCase := range []struct {
		name              string
		legacyID          string
		legacySecret      string
		encryptLegacy     bool
		missingKey        bool
		wrongKey          bool
		managed           bool
		disabled          bool
		workspaceOverride string
		wantID            string
		wantSecret        string
		wantConfig        string
		wantError         bool
	}{
		{name: "unrelated legacy app", legacyID: legacyAppID, legacySecret: legacySecret, wantID: legacyAppID, wantSecret: legacySecret},
		{name: "encrypted unrelated legacy app", legacyID: legacyAppID, legacySecret: legacySecret, encryptLegacy: true, wantID: legacyAppID, wantSecret: legacySecret},
		{name: "same app rotates centrally", legacyID: platformAppID, legacySecret: legacySecret, wantID: platformAppID, wantSecret: platformSecret, wantConfig: platformConfigID},
		{name: "same app ignores stale unreadable legacy secret", legacyID: platformAppID, legacySecret: legacySecret, encryptLegacy: true, wrongKey: true, wantID: platformAppID, wantSecret: platformSecret, wantConfig: platformConfigID},
		{name: "secret only does not borrow platform ID", legacySecret: legacySecret, wantSecret: legacySecret},
		{name: "ID only does not borrow platform secret", legacyID: legacyAppID, wantID: legacyAppID},
		{name: "new account uses shared defaults", wantID: platformAppID, wantSecret: platformSecret, wantConfig: platformConfigID},
		{name: "encrypted legacy secret without key fails closed", legacyID: legacyAppID, legacySecret: legacySecret, encryptLegacy: true, missingKey: true, wantError: true},
		{name: "encrypted legacy secret with wrong key fails closed", legacyID: legacyAppID, legacySecret: legacySecret, encryptLegacy: true, wrongKey: true, wantError: true},
		{name: "managed platform configuration remains authoritative", legacyID: legacyAppID, legacySecret: legacySecret, managed: true, wantID: platformAppID, wantSecret: platformSecret, wantConfig: platformConfigID},
		{name: "managed disable remains authoritative", legacyID: legacyAppID, legacySecret: legacySecret, managed: true, disabled: true, wantError: true},
		{name: "explicit workspace override remains authoritative", legacyID: legacyAppID, legacySecret: legacySecret, workspaceOverride: "complete", wantID: workspaceAppID, wantSecret: workspaceSecret, wantConfig: workspaceConfig},
		{name: "explicit workspace ID retains existing fallback", legacyID: legacyAppID, legacySecret: legacySecret, workspaceOverride: "app_id", wantID: workspaceAppID, wantSecret: platformSecret, wantConfig: platformConfigID},
		{name: "explicit workspace config retains existing fallback", legacyID: legacyAppID, legacySecret: legacySecret, workspaceOverride: "config_id", wantID: platformAppID, wantSecret: platformSecret, wantConfig: workspaceConfig},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			app := newIntegrationHandlerTestApp(t, integrationTestEncryptionKey)
			app.Config.WhatsApp.AppID = platformAppID
			app.Config.WhatsApp.AppSecret = platformSecret
			app.Config.WhatsApp.ConfigID = platformConfigID
			org := testutil.CreateTestOrganization(t, app.DB)
			if testCase.workspaceOverride != "" {
				org.Settings = models.JSONB{}
				if testCase.workspaceOverride != "config_id" {
					org.Settings["meta_app_id"] = workspaceAppID
				}
				if testCase.workspaceOverride != "app_id" {
					org.Settings["meta_config_id"] = workspaceConfig
				}
				if testCase.workspaceOverride == "complete" {
					ciphertext, err := appcrypto.Encrypt(workspaceSecret, integrationTestEncryptionKey)
					require.NoError(t, err)
					org.Settings[metaAppSecretSetting] = ciphertext
				}
				require.NoError(t, app.DB.Model(org).Update("settings", org.Settings).Error)
			}
			if testCase.managed {
				require.NoError(t, app.DB.Create(&models.ProviderIntegration{
					BaseModel:      models.BaseModel{ID: uuid.New()},
					OrganizationID: org.ID,
					Provider:       integrationProviderMeta,
					Enabled:        !testCase.disabled,
				}).Error)
			}
			account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
			account.AppID = testCase.legacyID
			account.AppSecret = testCase.legacySecret
			if testCase.encryptLegacy {
				var err error
				account.AppSecret, err = appcrypto.Encrypt(account.AppSecret, integrationTestEncryptionKey)
				require.NoError(t, err)
			}
			require.NoError(t, app.DB.Save(account).Error)
			if testCase.missingKey {
				app.Config.App.EncryptionKey = ""
			}
			if testCase.wrongKey {
				app.Config.App.EncryptionKey = "synthetic-wrong-encryption-key"
			}

			appID, secret, configID, err := app.resolveEffectiveMetaAppCreds(account)
			if testCase.wantError {
				require.Error(t, err)
				if testCase.disabled {
					assert.ErrorIs(t, err, errMetaIntegrationDisabled)
				}
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, testCase.wantID, appID)
			assert.Equal(t, testCase.wantSecret, secret)
			assert.Equal(t, testCase.wantConfig, configID)

			var stored models.WhatsAppAccount
			require.NoError(t, app.DB.First(&stored, "id = ?", account.ID).Error)
			assert.Equal(t, account.AppID, stored.AppID)
			assert.Equal(t, account.AppSecret, stored.AppSecret)
			var providerCount int64
			require.NoError(t, app.DB.Model(&models.ProviderIntegration{}).
				Where("organization_id = ?", org.ID).Count(&providerCount).Error)
			if testCase.managed {
				assert.EqualValues(t, 1, providerCount)
			} else {
				assert.Zero(t, providerCount, "resolving credentials must not adopt a legacy integration")
			}
		})
	}
}

func TestWebhookHandlerPlatformDefaultsPreserveLegacyMetaSigner(t *testing.T) {
	const (
		platformAppID  = "123456789012345"
		platformSecret = "synthetic-platform-webhook-signer"
		legacyAppID    = "345678901234567"
		legacySecret   = "synthetic-legacy-webhook-signer"
	)
	for _, testCase := range []struct {
		name               string
		legacyID           string
		legacySecret       string
		wantLegacyStatus   int
		wantPlatformStatus int
	}{
		{name: "unrelated legacy app", legacyID: legacyAppID, legacySecret: legacySecret, wantLegacyStatus: fasthttp.StatusOK, wantPlatformStatus: fasthttp.StatusForbidden},
		{name: "secret only legacy app", legacySecret: legacySecret, wantLegacyStatus: fasthttp.StatusOK, wantPlatformStatus: fasthttp.StatusForbidden},
		{name: "ID only legacy app", legacyID: legacyAppID, wantLegacyStatus: fasthttp.StatusForbidden, wantPlatformStatus: fasthttp.StatusForbidden},
		{name: "same app central rotation", legacyID: platformAppID, legacySecret: legacySecret, wantLegacyStatus: fasthttp.StatusForbidden, wantPlatformStatus: fasthttp.StatusOK},
		{name: "new platform account", wantLegacyStatus: fasthttp.StatusForbidden, wantPlatformStatus: fasthttp.StatusOK},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			app := webhookTestApp(t)
			app.Config.WhatsApp.AppID = platformAppID
			app.Config.WhatsApp.AppSecret = platformSecret
			app.Config.WhatsApp.ConfigID = "234567890123456"
			org := testutil.CreateTestOrganization(t, app.DB)
			account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
			account.AppID = testCase.legacyID
			if testCase.legacySecret != "" {
				var err error
				account.AppSecret, err = appcrypto.Encrypt(testCase.legacySecret, app.Config.App.EncryptionKey)
				require.NoError(t, err)
			}
			require.NoError(t, app.DB.Save(account).Error)
			body, err := json.Marshal(map[string]any{
				"object": "whatsapp_business_account",
				"entry": []any{map[string]any{
					"id": account.BusinessID,
					"changes": []any{map[string]any{
						"field": "messages",
						"value": map[string]any{
							"messaging_product": "whatsapp",
							"metadata":          map[string]string{"phone_number_id": account.PhoneID},
						},
					}},
				}},
			})
			require.NoError(t, err)
			for _, signer := range []struct {
				secret     string
				wantStatus int
			}{
				{secret: legacySecret, wantStatus: testCase.wantLegacyStatus},
				{secret: platformSecret, wantStatus: testCase.wantPlatformStatus},
			} {
				mac := hmac.New(sha256.New, []byte(signer.secret))
				_, err := mac.Write(body)
				require.NoError(t, err)
				request := testutil.NewRequest(t)
				request.RequestCtx.Request.Header.SetMethod("POST")
				request.RequestCtx.Request.Header.SetContentType("application/json")
				request.RequestCtx.Request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
				request.RequestCtx.Request.SetBody(body)
				require.NoError(t, app.WebhookHandler(request))
				assert.Equal(t, signer.wantStatus, testutil.GetResponseStatusCode(request))
			}
		})
	}
}

func TestMetaPlatformDefaultsVerifyEveryPayloadSigningAuthority(t *testing.T) {
	const (
		platformSecret = "synthetic-platform-batch-signer"
		legacySecret   = "synthetic-legacy-batch-signer"
	)
	app := webhookTestApp(t)
	app.Config.WhatsApp.AppID = "123456789012345"
	app.Config.WhatsApp.AppSecret = platformSecret
	app.Config.WhatsApp.ConfigID = "234567890123456"
	legacyOrg := testutil.CreateTestOrganization(t, app.DB)
	legacyAccount := testutil.CreateTestWhatsAppAccount(t, app.DB, legacyOrg.ID)
	legacyAccount.AppID = "345678901234567"
	var err error
	legacyAccount.AppSecret, err = appcrypto.Encrypt(legacySecret, app.Config.App.EncryptionKey)
	require.NoError(t, err)
	require.NoError(t, app.DB.Save(legacyAccount).Error)
	platformOrg := testutil.CreateTestOrganization(t, app.DB)
	platformAccount := testutil.CreateTestWhatsAppAccount(t, app.DB, platformOrg.ID)

	for _, testCase := range []struct {
		name         string
		accounts     []*models.WhatsAppAccount
		includePhone bool
		legacyOK     bool
		platformOK   bool
	}{
		{name: "legacy WABA only", accounts: []*models.WhatsAppAccount{legacyAccount}, legacyOK: true},
		{name: "platform WABA only", accounts: []*models.WhatsAppAccount{platformAccount}, platformOK: true},
		{name: "mixed WABA authorities", accounts: []*models.WhatsAppAccount{legacyAccount, platformAccount}},
		{name: "mixed WABA authorities reversed", accounts: []*models.WhatsAppAccount{platformAccount, legacyAccount}},
		{name: "mixed phone authorities", accounts: []*models.WhatsAppAccount{legacyAccount, platformAccount}, includePhone: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			entries := make([]any, 0, len(testCase.accounts))
			for _, account := range testCase.accounts {
				field := "message_template_status_update"
				value := map[string]any{"event": "APPROVED", "message_template_id": 123456789}
				if testCase.includePhone {
					field = "messages"
					value = map[string]any{
						"messaging_product": "whatsapp",
						"metadata":          map[string]string{"phone_number_id": account.PhoneID},
					}
				}
				entries = append(entries, map[string]any{
					"id":      account.BusinessID,
					"changes": []any{map[string]any{"field": field, "value": value}},
				})
			}
			body, err := json.Marshal(map[string]any{"object": "whatsapp_business_account", "entry": entries})
			require.NoError(t, err)
			var payload WebhookPayload
			require.NoError(t, json.Unmarshal(body, &payload))
			for _, signer := range []struct {
				name   string
				secret string
				wantOK bool
			}{
				{name: "legacy", secret: legacySecret, wantOK: testCase.legacyOK},
				{name: "platform", secret: platformSecret, wantOK: testCase.platformOK},
			} {
				mac := hmac.New(sha256.New, []byte(signer.secret))
				_, err := mac.Write(body)
				require.NoError(t, err)
				signature := []byte("sha256=" + hex.EncodeToString(mac.Sum(nil)))
				assert.Equal(t, signer.wantOK, app.verifyMetaWebhookPayload(body, signature, &payload), signer.name)
			}
		})
	}
}

func TestMetaPlatformDefaultsPreserveLegacyUploadAppIdentity(t *testing.T) {
	const legacySecret = "synthetic-legacy-upload-app-secret"
	for _, testCase := range []struct {
		name     string
		legacyID string
		wantErr  bool
	}{
		{name: "legacy upload uses its existing app ID", legacyID: "345678901234567"},
		{name: "secret only cannot borrow the platform upload app ID", wantErr: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			app := newIntegrationHandlerTestApp(t, integrationTestEncryptionKey)
			app.Config.WhatsApp.AppID = "123456789012345"
			app.Config.WhatsApp.AppSecret = "synthetic-platform-upload-secret"
			app.Config.WhatsApp.ConfigID = "234567890123456"
			org := testutil.CreateTestOrganization(t, app.DB)
			account := testutil.CreateTestWhatsAppAccount(t, app.DB, org.ID)
			account.AppID = testCase.legacyID
			var err error
			account.AppSecret, err = appcrypto.Encrypt(legacySecret, integrationTestEncryptionKey)
			require.NoError(t, err)
			require.NoError(t, app.DB.Save(account).Error)
			original := *account

			clientAccount, err := app.toWhatsAppAccountWithMetaApp(account)
			if testCase.wantErr {
				require.ErrorIs(t, err, errMetaAppIDNotConfigured)
				assert.Nil(t, clientAccount)
			} else {
				require.NoError(t, err)
				require.NotNil(t, clientAccount)
				assert.Equal(t, account.AppID, clientAccount.AppID)
				assert.Equal(t, account.AccessToken, clientAccount.AccessToken)
				assert.Equal(t, account.PhoneID, clientAccount.PhoneID)
				assert.Equal(t, account.BusinessID, clientAccount.BusinessID)
			}
			assert.Equal(t, original, *account, "building upload credentials must not mutate the caller's account")
			var stored models.WhatsAppAccount
			require.NoError(t, app.DB.First(&stored, "id = ?", account.ID).Error)
			assert.Equal(t, original.AppID, stored.AppID)
			assert.Equal(t, original.AppSecret, stored.AppSecret)
			assert.Equal(t, original.AccessToken, stored.AccessToken)
		})
	}
}

package handlers_test

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/config"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func TestApp_CreateOrganization_EmbeddedSignupUsesPlatformDefaultsWithoutCopyingTenantConnections(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name               string
		platformConfigured bool
	}{
		{name: "configured_platform", platformConfigured: true},
		{name: "missing_platform_configuration"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			app := newTestApp(t)
			app.Config.WhatsApp = config.WhatsAppConfig{APIVersion: "v21.0"}
			const platformSecret = "synthetic-platform-secret-must-not-be-returned"
			const platformVerifyToken = "synthetic-platform-webhook-token-must-not-be-returned"
			if testCase.platformConfigured {
				app.Config.WhatsApp.AppID = "123456789012345"
				app.Config.WhatsApp.ConfigID = "234567890123456"
				app.Config.WhatsApp.AppSecret = platformSecret
				app.Config.WhatsApp.WebhookVerifyToken = platformVerifyToken
			}

			reseller := testutil.CreateTestReseller(t, app.DB)
			sourceOrg := testutil.CreateTestOrganizationForReseller(t, app.DB, reseller.ID)
			const sourceSecret = "synthetic-other-workspace-secret-must-stay-isolated"
			encryptedSourceSecret, err := appcrypto.Encrypt(sourceSecret, app.Config.App.EncryptionKey)
			require.NoError(t, err)
			sourceOrg.Settings = models.JSONB{
				"meta_app_id":               "345678901234567",
				"meta_config_id":            "456789012345678",
				"meta_app_secret_encrypted": encryptedSourceSecret,
			}
			require.NoError(t, app.DB.Save(sourceOrg).Error)
			require.NoError(t, app.DB.Create(&models.ProviderIntegration{
				BaseModel:      models.BaseModel{ID: uuid.New()},
				OrganizationID: sourceOrg.ID,
				Provider:       "meta",
				Enabled:        true,
			}).Error)
			sourceAccount := testutil.CreateTestWhatsAppAccount(t, app.DB, sourceOrg.ID)
			require.NoError(t, app.DB.Create(&models.ChannelAccount{
				BaseModel:         models.BaseModel{ID: uuid.New()},
				OrganizationID:    sourceOrg.ID,
				Channel:           models.ChannelWhatsApp,
				Provider:          channelapi.LegacyMetaProvider,
				Name:              "Existing workspace WhatsApp",
				ExternalAccountID: sourceAccount.PhoneID,
				Status:            models.ChannelAccountStatusActive,
				Capabilities:      models.JSONB{},
				Config:            models.JSONB{},
				Metadata:          models.JSONB{},
			}).Error)
			allPermissions := testutil.GetOrCreateTestPermissions(t, app.DB)
			role := testutil.CreateTestRole(t, app.DB, sourceOrg.ID, "admin", allPermissions)
			user := testutil.CreateTestUser(t, app.DB, sourceOrg.ID, testutil.WithRoleID(&role.ID))
			require.NoError(t, app.DB.Create(&models.ResellerMember{
				BaseModel:  models.BaseModel{ID: uuid.New()},
				ResellerID: reseller.ID,
				UserID:     user.ID,
				Role:       models.ResellerRoleAdmin,
				IsActive:   true,
			}).Error)

			createRequest := testutil.NewJSONRequest(t, map[string]any{
				"name":        "New Meta onboarding workspace",
				"reseller_id": reseller.ID,
			})
			testutil.SetAuthContext(createRequest, sourceOrg.ID, user.ID)
			require.NoError(t, app.CreateOrganization(createRequest))
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(createRequest))
			var created struct {
				Data handlers.OrganizationResponse `json:"data"`
			}
			require.NoError(t, json.Unmarshal(testutil.GetResponseBody(createRequest), &created))
			require.NotEqual(t, uuid.Nil, created.Data.ID)

			// Use the creator's real, newly seeded membership in the new workspace.
			// Do not bypass permissions with a super-admin fixture.
			configRequest := testutil.NewGETRequest(t)
			testutil.SetAuthContext(configRequest, created.Data.ID, user.ID)
			testutil.SetHeader(configRequest, "X-Organization-ID", created.Data.ID.String())
			require.NoError(t, app.GetEmbeddedSignupConfig(configRequest))
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(configRequest))
			var publicConfig struct {
				Data map[string]any `json:"data"`
			}
			body := testutil.GetResponseBody(configRequest)
			require.NoError(t, json.Unmarshal(body, &publicConfig))
			expected := map[string]any{
				"organization_id":      created.Data.ID.String(),
				"whatsapp_api_version": "v21.0",
				"has_app_secret":       testCase.platformConfigured,
			}
			if testCase.platformConfigured {
				expected["whatsapp_app_id"] = app.Config.WhatsApp.AppID
				expected["whatsapp_config_id"] = app.Config.WhatsApp.ConfigID
			}
			assert.Equal(t, expected, publicConfig.Data, "only the selected workspace and public platform configuration may be returned")
			for _, secret := range []string{platformSecret, platformVerifyToken, sourceSecret, encryptedSourceSecret, sourceAccount.AccessToken} {
				assert.NotContains(t, string(body), secret)
			}

			var newOrg models.Organization
			require.NoError(t, app.DB.First(&newOrg, "id = ?", created.Data.ID).Error)
			for _, key := range []string{"meta_app_id", "meta_config_id", "meta_app_secret_encrypted", "meta_webhook_verify_token_encrypted"} {
				assert.NotContains(t, newOrg.Settings, key, "Meta configuration must not be copied into tenant settings")
			}
			for _, record := range []any{&models.ProviderIntegration{}, &models.ChannelAccount{}, &models.WhatsAppAccount{}} {
				var count int64
				require.NoError(t, app.DB.Model(record).Where("organization_id = ?", newOrg.ID).Count(&count).Error)
				assert.Zero(t, count, "signup readiness must not create or inherit %T records", record)
			}
			var unchangedSource models.Organization
			require.NoError(t, app.DB.First(&unchangedSource, "id = ?", sourceOrg.ID).Error)
			assert.Equal(t, sourceOrg.Settings, unchangedSource.Settings)
		})
	}
}

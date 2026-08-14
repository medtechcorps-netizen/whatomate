package handlers

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/config"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/metaregistry"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

const metaRegistryTestEncryptionKey = "synthetic-meta-registry-encryption-key-32-bytes"
const metaRegistryTestServiceSecret = "synthetic-meta-registry-service-secret-at-least-32-bytes"

type metaRegistryFixture struct {
	account models.ChannelAccount
	oauth   models.ChannelCredential
	webhook models.ChannelCredential
	userID  uuid.UUID
}

func TestMetaRegistryServiceAuthReplayUsesFullClockSkewWindow(t *testing.T) {
	redisClient := testutil.SetupTestRedis(t)
	if redisClient == nil {
		t.Skip("TEST_REDIS_URL is required for the real Redis replay test")
	}
	nonce := "replay-" + uuid.NewString()
	replayKey := "meta-registry:nonce:" + nonce
	t.Cleanup(func() { _ = redisClient.Del(t.Context(), replayKey).Err() })
	app := &App{
		Redis: redisClient, Log: testutil.NopLogger(),
		Config: &config.Config{MetaRegistry: config.MetaRegistryConfig{
			ServiceSecret: metaRegistryTestServiceSecret,
			// Deliberately unsafe direct construction proves the handler itself
			// clamps retention even when config loading was bypassed.
			ReplayWindowSeconds: 120,
		}},
	}
	makeRequest := func() *fastglue.Request {
		request := testutil.NewJSONRequest(t, map[string]any{
			"channel": "messenger", "external_account_id": "page-replay",
		})
		request.RequestCtx.Request.Header.SetMethod(fasthttp.MethodPost)
		now := time.Now().UTC()
		raw := request.RequestCtx.PostBody()
		request.RequestCtx.Request.Header.Set(metaregistry.TimestampHeader, strconv.FormatInt(now.Unix(), 10))
		request.RequestCtx.Request.Header.Set(metaregistry.NonceHeader, nonce)
		request.RequestCtx.Request.Header.Set(metaregistry.SignatureHeader, metaregistry.SignRequest(
			metaRegistryTestServiceSecret, fasthttp.MethodPost, metaregistry.ResolvePath, now, nonce, raw,
		))
		return request
	}

	_, _, accepted := app.authenticateMetaRegistryRequest(makeRequest(), metaregistry.ResolvePath)
	require.True(t, accepted)
	ttl, err := redisClient.TTL(t.Context(), replayKey).Result()
	require.NoError(t, err)
	require.GreaterOrEqual(t, ttl, metaregistry.ReplayWindowFloor-5*time.Second)

	replay := makeRequest()
	_, _, accepted = app.authenticateMetaRegistryRequest(replay, metaregistry.ResolvePath)
	require.False(t, accepted)
	require.Equal(t, fasthttp.StatusUnauthorized, testutil.GetResponseStatusCode(replay))
}

func TestLoadMetaRegistryBindingIsTenantScopedEncryptedAndVersioned(t *testing.T) {
	db := testutil.SetupTestDB(t)
	orgA := testutil.CreateTestOrganization(t, db)
	orgB := testutil.CreateTestOrganization(t, db)
	fixtureA := createMetaRegistryFixture(t, db, orgA.ID, models.ChannelMessenger, "page-a")
	fixtureB := createMetaRegistryFixture(t, db, orgB.ID, models.ChannelMessenger, "page-b")

	scopedA := metaRegistryTestApp(db, orgA.ID)
	now := time.Now().UTC()
	binding, err := scopedA.loadMetaRegistryBinding(metaregistry.ResolveRequest{
		Channel: models.ChannelMessenger, ExternalAccountID: fixtureA.account.ExternalAccountID,
	}, now)
	require.NoError(t, err)
	assert.Equal(t, orgA.ID, binding.OrganizationID)
	assert.Equal(t, fixtureA.oauth.ID, binding.CredentialID)
	assert.Equal(t, fixtureA.oauth.Version, binding.CredentialVersion)
	assert.Equal(t, fixtureA.webhook.ID, binding.WebhookCredentialID)
	assert.Equal(t, "provider-token-page-a", binding.AccessToken)
	assert.Equal(t, "inbound-page-a", binding.InboundSecret)
	assert.Equal(t, "outbound-page-a", binding.OutboundSecret)

	_, err = scopedA.loadMetaRegistryBinding(metaregistry.ResolveRequest{
		Channel: models.ChannelMessenger, ExternalAccountID: fixtureB.account.ExternalAccountID,
	}, now)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

func TestMetaRegistryRevocationUsesTwoCredentialCASAndRedactedAudit(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	fixture := createMetaRegistryFixture(t, db, org.ID, models.ChannelInstagram, "ig-1")
	scoped := metaRegistryTestApp(db, org.ID)

	request := metaregistry.MutationRequest{
		ChannelAccountID: fixture.account.ID,
		CredentialID:     fixture.oauth.ID, CredentialVersion: fixture.oauth.Version,
		WebhookCredentialID: fixture.webhook.ID, WebhookCredentialVersion: fixture.webhook.Version,
		CheckedAt: time.Now().UTC(), Reason: "provider_deauthorization",
	}
	applied, err := scoped.applyMetaRegistryMutation(request, metaregistry.OwnershipRevoked)
	require.NoError(t, err)
	assert.True(t, applied)

	var account models.ChannelAccount
	require.NoError(t, db.Where("id = ? AND organization_id = ?", fixture.account.ID, org.ID).First(&account).Error)
	assert.Equal(t, models.ChannelAccountStatusDisconnected, account.Status)
	assert.Equal(t, metaregistry.OwnershipRevoked, stringConfigValue(account.Metadata, "meta_ownership_state"))
	var credentials []models.ChannelCredential
	require.NoError(t, db.Where("channel_account_id = ?", account.ID).Find(&credentials).Error)
	require.Len(t, credentials, 2)
	for _, credential := range credentials {
		assert.Equal(t, models.ChannelCredentialStatusRevoked, credential.Status)
	}

	var log models.AuditLog
	require.NoError(t, db.Where("organization_id = ? AND resource_type = ?", org.ID, "meta_channel_registry").First(&log).Error)
	encoded, err := json.Marshal(log.Changes)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "provider-token")
	assert.NotContains(t, string(encoded), "inbound-")
	assert.NotContains(t, string(encoded), "outbound-")
	assert.NotContains(t, string(encoded), "enc:")

	// A delayed revalidation carrying the old lease cannot resurrect or mutate
	// the revoked credential tuple.
	applied, err = scoped.applyMetaRegistryMutation(request, metaregistry.OwnershipVerified)
	require.ErrorIs(t, err, metaregistry.ErrNotFound)
	assert.False(t, applied)
}

func TestMetaRegistryBindingFailsClosedWithoutPlatformAppOrScopes(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	fixture := createMetaRegistryFixture(t, db, org.ID, models.ChannelMessenger, "page-scope")
	scoped := metaRegistryTestApp(db, org.ID)
	now := time.Now().UTC()

	metadata := cloneJSONB(fixture.account.Metadata)
	metadata["meta_webhook_app"] = "clinic_specific_app"
	require.NoError(t, db.Model(&models.ChannelAccount{}).Where("id = ?", fixture.account.ID).Update("metadata", metadata).Error)
	_, err := scoped.loadMetaRegistryBinding(metaregistry.ResolveRequest{
		Channel: models.ChannelMessenger, ExternalAccountID: fixture.account.ExternalAccountID,
	}, now)
	require.ErrorIs(t, err, metaregistry.ErrNotFound)

	metadata["meta_webhook_app"] = "messenger"
	metadata["meta_granted_scopes"] = []string{"pages_messaging"}
	require.NoError(t, db.Model(&models.ChannelAccount{}).Where("id = ?", fixture.account.ID).Update("metadata", metadata).Error)
	_, err = scoped.loadMetaRegistryBinding(metaregistry.ResolveRequest{
		Channel: models.ChannelMessenger, ExternalAccountID: fixture.account.ExternalAccountID,
	}, now)
	require.ErrorIs(t, err, metaregistry.ErrNotFound)
}

func TestProvisionMetaRegistryBindingCreatesEncryptedAuditedTenantRecord(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, org.ID)
	root := &App{
		DB: db, Log: testutil.NopLogger(),
		Config: &config.Config{App: config.AppConfig{EncryptionKey: metaRegistryTestEncryptionKey}},
	}
	var result metaRegistryProvisionResult
	err := root.WithCommittedTenantApp(org.ID, func(scoped *App) error {
		var provisionErr error
		result, provisionErr = scoped.provisionMetaRegistryBinding(metaRegistryProvisionInput{
			OrganizationID: org.ID, UserID: user.ID, Channel: models.ChannelMessenger,
			Name: "Synthetic Clinic Messenger", ExternalAccountID: "page-provisioned",
			WebhookApp: "messenger", MetaBusinessID: "business-1", AuthorizingMetaUserID: "user-1",
			GrantedScopes: []string{"pages_manage_metadata", "pages_read_engagement", "pages_messaging"},
			AccessToken:   "plaintext-provider-token", OwnershipCheckedAt: time.Now().UTC(),
			ReReplyBaseURL: "https://app.example.test", RelayBaseURL: "https://app.example.test/meta-relay",
		})
		return provisionErr
	})
	require.NoError(t, err)
	assert.Equal(t, org.ID, result.Account.OrganizationID)
	assert.Equal(t, models.ChannelAccountStatusActive, result.Account.Status)
	assert.False(t, boolConfigValue(result.Account.Config, "outbound_enabled"), "outbound still requires an explicit post-health approval")

	var credentials []models.ChannelCredential
	require.NoError(t, db.Where("channel_account_id = ?", result.Account.ID).Find(&credentials).Error)
	require.Len(t, credentials, 2)
	encoded, err := json.Marshal(credentials)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "plaintext-provider-token")
	for _, credential := range credentials {
		for _, value := range credential.CredentialBlob {
			ciphertext, _ := value.(string)
			assert.True(t, appcrypto.IsEncrypted(ciphertext))
		}
	}
	var log models.AuditLog
	require.NoError(t, db.Where("organization_id = ? AND resource_type = ?", org.ID, "meta_channel_registry").First(&log).Error)
	auditJSON, err := json.Marshal(log.Changes)
	require.NoError(t, err)
	assert.NotContains(t, string(auditJSON), "plaintext-provider-token")
	assert.Contains(t, string(auditJSON), "********")
}

func TestMetaRegistryRevalidationIsMonotonicAndCanRecoverDegradedAccount(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	fixture := createMetaRegistryFixture(t, db, org.ID, models.ChannelMessenger, "page-revalidate")
	scoped := metaRegistryTestApp(db, org.ID)
	base, err := time.Parse(time.RFC3339Nano, stringConfigValue(fixture.account.Metadata, "meta_ownership_checked_at"))
	require.NoError(t, err)
	request := metaregistry.MutationRequest{
		ChannelAccountID: fixture.account.ID,
		CredentialID:     fixture.oauth.ID, CredentialVersion: fixture.oauth.Version,
		WebhookCredentialID: fixture.webhook.ID, WebhookCredentialVersion: fixture.webhook.Version,
		CheckedAt: base.Add(2 * time.Second),
	}
	applied, err := scoped.applyMetaRegistryMutation(request, metaregistry.OwnershipStale)
	require.NoError(t, err)
	require.True(t, applied)

	request.CheckedAt = base.Add(2 * time.Second)
	applied, err = scoped.applyMetaRegistryMutation(request, metaregistry.OwnershipVerified)
	require.ErrorIs(t, err, metaregistry.ErrNotFound)
	require.False(t, applied)

	request.CheckedAt = base.Add(time.Second)
	applied, err = scoped.applyMetaRegistryMutation(request, metaregistry.OwnershipVerified)
	require.ErrorIs(t, err, metaregistry.ErrNotFound)
	require.False(t, applied)

	request.CheckedAt = base.Add(3 * time.Second)
	applied, err = scoped.applyMetaRegistryMutation(request, metaregistry.OwnershipVerified)
	require.NoError(t, err)
	require.True(t, applied)
	var account models.ChannelAccount
	require.NoError(t, db.Where("id = ?", fixture.account.ID).First(&account).Error)
	require.Equal(t, models.ChannelAccountStatusActive, account.Status)
	require.Equal(t, metaregistry.OwnershipVerified, stringConfigValue(account.Metadata, "meta_ownership_state"))
}

func TestGenericChannelAPIProtectsManagedMetaRoutingButAllowsSafeProfileToggle(t *testing.T) {
	fixture := newChannelAccountConcurrencyFixture(t, false)
	fixture.Account.Config["meta_registry_managed"] = true
	fixture.Account.Config["meta_management_mode"] = metaregistry.ManagementModePlatformOAuth
	fixture.Account.Metadata["meta_ownership_state"] = metaregistry.OwnershipVerified
	require.NoError(t, fixture.App.DB.Model(&models.ChannelAccount{}).
		Where("id = ?", fixture.Account.ID).Updates(map[string]any{
		"config": fixture.Account.Config, "metadata": fixture.Account.Metadata,
	}).Error)

	unsafeUpdate := testutil.NewJSONRequest(t, map[string]any{
		"config": map[string]any{"relay_url": "https://attacker.example.test/collect"},
	})
	unsafeUpdate.RequestCtx.Request.Header.SetMethod(fasthttp.MethodPut)
	testutil.SetFullAuthContext(unsafeUpdate, fixture.Organization.ID, fixture.User.ID, fixture.User.RoleID, true)
	testutil.SetPathParam(unsafeUpdate, "id", fixture.Account.ID.String())
	require.NoError(t, fixture.App.UpdateChannelAccount(unsafeUpdate))
	testutil.AssertErrorResponse(t, unsafeUpdate, fasthttp.StatusConflict, "managed by Meta onboarding")

	name := "Safe renamed connection"
	safeUpdate := testutil.NewJSONRequest(t, map[string]any{"name": name})
	safeUpdate.RequestCtx.Request.Header.SetMethod(fasthttp.MethodPut)
	testutil.SetFullAuthContext(safeUpdate, fixture.Organization.ID, fixture.User.ID, fixture.User.RoleID, true)
	testutil.SetPathParam(safeUpdate, "id", fixture.Account.ID.String())
	require.NoError(t, fixture.App.UpdateChannelAccount(safeUpdate))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(safeUpdate))

	blockedApproval := testutil.NewJSONRequest(t, map[string]any{"outbound_enabled": true})
	blockedApproval.RequestCtx.Request.Header.SetMethod(fasthttp.MethodPut)
	testutil.SetFullAuthContext(blockedApproval, fixture.Organization.ID, fixture.User.ID, fixture.User.RoleID, true)
	testutil.SetPathParam(blockedApproval, "id", fixture.Account.ID.String())
	require.NoError(t, fixture.App.UpdateChannelAccount(blockedApproval))
	testutil.AssertErrorResponse(t, blockedApproval, fasthttp.StatusBadRequest, "successful Meta relay health test")

	healthCheckedAt := time.Now().UTC()
	require.NoError(t, fixture.App.DB.Model(&models.ChannelAccount{}).
		Where("id = ?", fixture.Account.ID).Updates(map[string]any{
		"status": models.ChannelAccountStatusActive, "last_health_check_at": healthCheckedAt,
		"last_error": "", "last_error_at": nil,
	}).Error)
	approved := testutil.NewJSONRequest(t, map[string]any{
		"outbound_enabled": true, "ai_reply_enabled": true,
	})
	approved.RequestCtx.Request.Header.SetMethod(fasthttp.MethodPut)
	testutil.SetFullAuthContext(approved, fixture.Organization.ID, fixture.User.ID, fixture.User.RoleID, true)
	testutil.SetPathParam(approved, "id", fixture.Account.ID.String())
	require.NoError(t, fixture.App.UpdateChannelAccount(approved))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(approved))

	deleteRequest := testutil.NewRequest(t)
	testutil.SetFullAuthContext(deleteRequest, fixture.Organization.ID, fixture.User.ID, fixture.User.RoleID, true)
	testutil.SetPathParam(deleteRequest, "id", fixture.Account.ID.String())
	require.NoError(t, fixture.App.DeleteChannelAccount(deleteRequest))
	testutil.AssertErrorResponse(t, deleteRequest, fasthttp.StatusConflict, "managed by Meta onboarding")

	var stored models.ChannelAccount
	require.NoError(t, fixture.App.DB.Where("id = ?", fixture.Account.ID).First(&stored).Error)
	require.Equal(t, name, stored.Name)
	require.Equal(t, "https://relay-original.example.com/meta", stringConfigValue(stored.Config, "relay_url"))
	require.True(t, boolConfigValue(stored.Config, "outbound_enabled"))
	require.True(t, boolConfigValue(stored.Config, "ai_reply_enabled"))
}

func TestGenericChannelUpdateRejectsReservedMetaMarkerEvenWhenFalse(t *testing.T) {
	fixture := newChannelAccountConcurrencyFixture(t, false)
	request := testutil.NewJSONRequest(t, map[string]any{
		"config": map[string]any{
			"relay_url":             "https://relay-original.example.com/meta",
			"meta_registry_managed": false,
		},
	})
	request.RequestCtx.Request.Header.SetMethod(fasthttp.MethodPut)
	testutil.SetFullAuthContext(request, fixture.Organization.ID, fixture.User.ID, fixture.User.RoleID, true)
	testutil.SetPathParam(request, "id", fixture.Account.ID.String())
	require.NoError(t, fixture.App.UpdateChannelAccount(request))
	testutil.AssertErrorResponse(t, request, fasthttp.StatusBadRequest, "control markers")

	var stored models.ChannelAccount
	require.NoError(t, fixture.App.DB.Where("id = ?", fixture.Account.ID).First(&stored).Error)
	_, exists := stored.Config["meta_registry_managed"]
	require.False(t, exists)
}

func createMetaRegistryFixture(t *testing.T, db *gorm.DB, orgID uuid.UUID, channel models.Channel, externalID string) metaRegistryFixture {
	t.Helper()
	user := testutil.CreateTestUser(t, db, orgID)
	now := time.Now().UTC()
	mode := ""
	webhookApp := "messenger"
	scopes := []string{"pages_manage_metadata", "pages_read_engagement", "pages_messaging"}
	if channel == models.ChannelInstagram {
		mode = "instagram_login"
		webhookApp = "instagram_login"
		scopes = []string{"instagram_business_basic", "instagram_business_manage_messages"}
	}
	account := models.ChannelAccount{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: orgID,
		Channel: channel, Provider: channelapi.RelayProvider, Name: "registry-" + externalID,
		ExternalAccountID: externalID, Status: models.ChannelAccountStatusActive,
		Capabilities: models.JSONB{"text": true},
		Config: models.JSONB{
			"meta_registry_managed": true, "meta_management_mode": metaregistry.ManagementModePlatformOAuth,
			"instagram_api_mode":  mode,
			"rereply_webhook_url": "https://app.example.test/api/webhooks/channels/" + uuid.NewString(),
		},
		Metadata: models.JSONB{
			"meta_ownership_state":      metaregistry.OwnershipVerified,
			"meta_ownership_checked_at": now.Format(time.RFC3339Nano),
			"meta_webhook_app":          webhookApp,
			"meta_business_id":          "business-" + externalID,
			"meta_authorizing_user_id":  "user-" + externalID,
			"meta_granted_scopes":       scopes,
		},
		CreatedByID: &user.ID, UpdatedByID: &user.ID,
	}
	require.NoError(t, db.Create(&account).Error)
	encrypt := func(value string) string {
		ciphertext, err := appcrypto.Encrypt(value, metaRegistryTestEncryptionKey)
		require.NoError(t, err)
		return ciphertext
	}
	oauth := models.ChannelCredential{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: orgID, ChannelAccountID: account.ID,
		Kind: models.ChannelCredentialKindOAuth, Version: 2,
		CredentialBlob: models.JSONB{"access_token": encrypt("provider-token-" + externalID)},
		Status:         models.ChannelCredentialStatusActive, KeyVersion: "app:v1", Metadata: models.JSONB{},
	}
	webhook := models.ChannelCredential{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: orgID, ChannelAccountID: account.ID,
		Kind: models.ChannelCredentialKindWebhook, Version: 3,
		CredentialBlob: models.JSONB{
			"inbound_secret":  encrypt("inbound-" + externalID),
			"outbound_secret": encrypt("outbound-" + externalID),
		},
		Status: models.ChannelCredentialStatusActive, KeyVersion: "app:v1", Metadata: models.JSONB{},
	}
	require.NoError(t, db.Create(&oauth).Error)
	require.NoError(t, db.Create(&webhook).Error)
	return metaRegistryFixture{account: account, oauth: oauth, webhook: webhook, userID: user.ID}
}

func metaRegistryTestApp(db *gorm.DB, organizationID uuid.UUID) *App {
	return &App{
		DB: db, Log: testutil.NopLogger(), tenantOrgID: organizationID,
		Config: &config.Config{
			App:          config.AppConfig{EncryptionKey: metaRegistryTestEncryptionKey},
			MetaRegistry: config.MetaRegistryConfig{LeaseSeconds: 30, OwnershipMaxAgeMins: 24 * 60},
		},
	}
}

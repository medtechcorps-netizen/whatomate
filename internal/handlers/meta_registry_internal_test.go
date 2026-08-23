package handlers

import (
	"encoding/json"
	"fmt"
	"slices"
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
		Purpose: metaregistry.ResolvePurposeInbound,
	}, now)
	require.NoError(t, err)
	assert.Equal(t, orgA.ID, binding.OrganizationID)
	assert.Equal(t, fixtureA.oauth.ID, binding.CredentialID)
	assert.Equal(t, fixtureA.oauth.Version, binding.CredentialVersion)
	assert.Equal(t, fixtureA.webhook.ID, binding.WebhookCredentialID)
	assert.Equal(t, "123", binding.PlatformAppID)
	assert.Equal(t, "provider-token-page-a", binding.AccessToken)
	assert.Equal(t, "inbound-page-a", binding.InboundSecret)
	assert.Equal(t, "outbound-page-a", binding.OutboundSecret)

	_, err = scopedA.loadMetaRegistryBinding(metaregistry.ResolveRequest{
		Channel: models.ChannelMessenger, ExternalAccountID: fixtureB.account.ExternalAccountID,
		Purpose: metaregistry.ResolvePurposeInbound,
	}, now)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

func TestMetaRegistryRevocationUsesTwoCredentialCASAndRedactedAudit(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	db := fixture.db
	org := fixture.org
	scoped := fixture.app.scopedApp(db, org.ID)

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
	binding, err := scoped.loadMetaRegistryBinding(metaregistry.ResolveRequest{
		Channel: models.ChannelMessenger, ExternalAccountID: fixture.account.ExternalAccountID,
		Purpose: metaregistry.ResolvePurposeHealth,
	}, now)
	require.NoError(t, err)
	assert.Equal(t, "123", binding.PlatformAppID)

	metadata := cloneJSONB(fixture.account.Metadata)
	metadata["meta_webhook_app"] = "clinic_specific_app"
	require.NoError(t, db.Model(&models.ChannelAccount{}).Where("id = ?", fixture.account.ID).Update("metadata", metadata).Error)
	_, err = scoped.loadMetaRegistryBinding(metaregistry.ResolveRequest{
		Channel: models.ChannelMessenger, ExternalAccountID: fixture.account.ExternalAccountID,
		Purpose: metaregistry.ResolvePurposeInbound,
	}, now)
	require.ErrorIs(t, err, metaregistry.ErrNotFound)

	for _, missing := range []string{"pages_messaging", "pages_manage_metadata"} {
		metadata = cloneJSONB(fixture.account.Metadata)
		metadata["meta_granted_scopes"] = slices.DeleteFunc(
			append([]string(nil), metaMessengerRequiredScopes...),
			func(scope string) bool { return scope == missing },
		)
		require.NoError(t, db.Model(&models.ChannelAccount{}).Where("id = ?", fixture.account.ID).Update("metadata", metadata).Error)
		_, err = scoped.loadMetaRegistryBinding(metaregistry.ResolveRequest{
			Channel: models.ChannelMessenger, ExternalAccountID: fixture.account.ExternalAccountID,
			Purpose: metaregistry.ResolvePurposeHealth,
		}, now)
		require.ErrorIs(t, err, metaregistry.ErrNotFound, missing)
	}
}

func TestMetaRegistryBindingAcceptsOnlyCurrentBISUAuthorityEvidence(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	fixture := createMetaRegistryFixture(t, db, org.ID, models.ChannelMessenger, "page-bisu-proof")
	scoped := metaRegistryTestApp(db, org.ID)
	now := time.Now().UTC()
	checkedAt, err := time.Parse(
		time.RFC3339Nano,
		stringConfigValue(fixture.account.Metadata, "meta_ownership_checked_at"),
	)
	require.NoError(t, err)
	base := cloneJSONB(fixture.account.Metadata)
	base["meta_granted_scopes"] = append([]string(nil), metaMessengerSystemUserRequiredScopes...)
	setMetaMessengerBusinessAuthorityEvidence(
		base,
		stringConfigValue(base, "meta_platform_app_id"),
		stringConfigValue(base, "meta_authorizing_user_id"),
		stringConfigValue(base, "meta_business_id"),
		fixture.account.ExternalAccountID,
		checkedAt,
		fixture.oauth.ID,
		fixture.oauth.Version,
	)
	update := func(metadata models.JSONB) error {
		return db.Model(&models.ChannelAccount{}).
			Where("id = ? AND organization_id = ?", fixture.account.ID, org.ID).
			Update("metadata", metadata).Error
	}
	resolve := func() error {
		_, resolveErr := scoped.loadMetaRegistryBinding(metaregistry.ResolveRequest{
			Channel: models.ChannelMessenger, ExternalAccountID: fixture.account.ExternalAccountID,
			Purpose: metaregistry.ResolvePurposeHealth,
		}, now)
		return resolveErr
	}

	require.NoError(t, update(base))
	require.NoError(t, resolve())

	for _, testCase := range []struct {
		name   string
		mutate func(models.JSONB)
	}{
		{name: "missing marker", mutate: func(metadata models.JSONB) {
			delete(metadata, metaregistry.MessengerBusinessAuthorityMetadataKey)
		}},
		{name: "wrong token kind", mutate: func(metadata models.JSONB) {
			metadata[metaMessengerAuthorizationTokenKindKey] = metaMessengerTokenKindUser
		}},
		{name: "old OAuth generation", mutate: func(metadata models.JSONB) {
			metadata[metaregistry.MessengerBusinessAuthorityOAuthVersionMetadataKey] = fixture.oauth.Version + 1
		}},
		{name: "Page identity mismatch", mutate: func(metadata models.JSONB) {
			metadata[metaregistry.MessengerBusinessAuthorityPageIDMetadataKey] = "different-page"
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			metadata := cloneJSONB(base)
			testCase.mutate(metadata)
			require.NoError(t, update(metadata))
			require.ErrorIs(t, resolve(), metaregistry.ErrNotFound)
		})
	}
}

func TestMetaRegistryPendingBindingIsHealthOnlyUntilExplicitApproval(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	fixture := createMetaRegistryFixture(t, db, org.ID, models.ChannelMessenger, "page-pending")
	require.NoError(t, db.Model(&models.ChannelAccount{}).Where("id = ?", fixture.account.ID).
		Update("status", models.ChannelAccountStatusPending).Error)
	scoped := metaRegistryTestApp(db, org.ID)
	now := time.Now().UTC()
	_, err := scoped.loadMetaRegistryBinding(metaregistry.ResolveRequest{
		Channel: models.ChannelMessenger, ExternalAccountID: fixture.account.ExternalAccountID,
		Purpose: metaregistry.ResolvePurposeInbound,
	}, now)
	require.ErrorIs(t, err, metaregistry.ErrStaleBinding)
	binding, err := scoped.loadMetaRegistryBinding(metaregistry.ResolveRequest{
		Channel: models.ChannelMessenger, ExternalAccountID: fixture.account.ExternalAccountID,
		Purpose: metaregistry.ResolvePurposeHealth,
	}, now)
	require.NoError(t, err)
	assert.Equal(t, fixture.oauth.Version, binding.CredentialVersion)
}

func TestManagedMessengerActivationRequiresFreshHealthAndCredentialFences(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	fixture := createMetaRegistryFixture(t, db, organization.ID, models.ChannelMessenger, "page-activation")
	app := metaRegistryTestApp(db, organization.ID)
	now := time.Now().UTC().Truncate(time.Microsecond)
	staleHealth := now.Add(-16 * time.Minute)
	metadata := cloneJSONB(fixture.account.Metadata)
	metadata["meta_subscription_state"] = "verified"
	metadata["meta_activation_state"] = "awaiting_admin_approval"
	metadata["meta_health_checked_at"] = staleHealth.Format(time.RFC3339Nano)
	metadata["meta_health_oauth_credential_id"] = fixture.oauth.ID.String()
	metadata["meta_health_oauth_version"] = fixture.oauth.Version
	metadata["meta_health_webhook_credential_id"] = fixture.webhook.ID.String()
	metadata["meta_health_webhook_version"] = fixture.webhook.Version
	configJSON := cloneJSONB(fixture.account.Config)
	configJSON["outbound_enabled"] = false
	configJSON["ai_reply_enabled"] = false
	require.NoError(t, db.Model(&models.ChannelAccount{}).Where("id = ?", fixture.account.ID).Updates(map[string]any{
		"status": models.ChannelAccountStatusPending, "config": configJSON, "metadata": metadata,
		"last_health_check_at": staleHealth, "last_error": "",
	}).Error)
	subscriptionApproval := metaMessengerSubscriptionApproval{
		PageID: fixture.account.ExternalAccountID, CheckedAt: now,
		OAuthCredentialID: fixture.oauth.ID, OAuthCredentialVersion: fixture.oauth.Version,
		WebhookCredentialID: fixture.webhook.ID, WebhookCredentialVersion: fixture.webhook.Version,
	}

	_, err := app.activateMetaMessengerAccount(
		organization.ID, fixture.userID, fixture.account.ID, now, subscriptionApproval,
	)
	require.Error(t, err)
	var pending models.ChannelAccount
	require.NoError(t, db.First(&pending, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusPending, pending.Status)
	assert.False(t, boolConfigValue(pending.Config, "outbound_enabled"))
	assert.False(t, boolConfigValue(pending.Config, "ai_reply_enabled"))

	// Metadata written before the precision fix could retain nanoseconds while
	// PostgreSQL stored the matching column at microsecond precision.
	freshHealth := now.Add(-time.Minute).Add(789 * time.Nanosecond)
	metadata["meta_health_checked_at"] = freshHealth.Format(time.RFC3339Nano)
	metadata["meta_ownership_checked_at"] = freshHealth.Format(time.RFC3339Nano)
	require.NoError(t, db.Model(&models.ChannelAccount{}).Where("id = ?", fixture.account.ID).Updates(map[string]any{
		"metadata": metadata, "last_health_check_at": freshHealth.Add(-time.Microsecond),
	}).Error)
	_, err = app.activateMetaMessengerAccount(
		organization.ID, fixture.userID, fixture.account.ID, now, subscriptionApproval,
	)
	require.Error(t, err, "a full-microsecond evidence mismatch must remain fail closed")
	require.NoError(t, db.Model(&models.ChannelAccount{}).Where("id = ?", fixture.account.ID).
		Update("last_health_check_at", freshHealth).Error)
	var storedHealth models.ChannelAccount
	require.NoError(t, db.First(&storedHealth, "id = ?", fixture.account.ID).Error)
	require.NotNil(t, storedHealth.LastHealthCheckAt)
	assert.False(t, storedHealth.LastHealthCheckAt.UTC().Equal(freshHealth.UTC()))
	assert.Equal(t, freshHealth.Truncate(time.Microsecond), storedHealth.LastHealthCheckAt.UTC())
	assert.True(t, channelHealthTimestampsMatch(*storedHealth.LastHealthCheckAt, freshHealth))
	activated, err := app.activateMetaMessengerAccount(
		organization.ID, fixture.userID, fixture.account.ID, now, subscriptionApproval,
	)
	require.NoError(t, err)
	assert.Equal(t, models.ChannelAccountStatusActive, activated.Status)
	assert.True(t, boolConfigValue(activated.Config, "outbound_enabled"))
	assert.False(t, boolConfigValue(activated.Config, "ai_reply_enabled"))
	assert.NotNil(t, activated.ConnectedAt)
	assert.Equal(t, "active", stringConfigValue(activated.Metadata, "meta_activation_state"))
}

func TestChannelHealthTimestampPrecision(t *testing.T) {
	base := time.Date(2026, time.August, 22, 1, 2, 3, 456789000, time.UTC)
	source := base.Add(987 * time.Nanosecond).In(time.FixedZone("test", 8*60*60))
	canonical := canonicalChannelHealthTimestamp(source)

	assert.Equal(t, base, canonical)
	assert.True(t, channelHealthTimestampsMatch(base, base))
	assert.True(t, channelHealthTimestampsMatch(base, base.Add(999*time.Nanosecond)))
	assert.True(t, channelHealthTimestampsMatch(base.Add(time.Microsecond), base.Add(501*time.Nanosecond)))
	assert.False(t, channelHealthTimestampsMatch(base, base.Add(time.Microsecond)))
	assert.False(t, channelHealthTimestampsMatch(base, base.Add(-time.Microsecond)))
}

func TestManagedMessengerProductionRejectsCarriedDevelopmentUserToken(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	fixture := createMetaRegistryFixture(t, db, organization.ID, models.ChannelMessenger, "page-user-token")
	app := metaRegistryTestApp(db, organization.ID)
	app.Config.App.Environment = "production"
	app.Config.MetaMessenger.AllowDevelopmentUserToken = false
	now := time.Now().UTC().Truncate(time.Microsecond)
	metadata := cloneJSONB(fixture.account.Metadata)
	metadata[metaMessengerAuthorizationTokenKindKey] = metaMessengerTokenKindUser
	metadata["meta_subscription_state"] = "verified"
	metadata["meta_activation_state"] = "awaiting_admin_approval"
	metadata["meta_health_checked_at"] = now.Format(time.RFC3339Nano)
	metadata["meta_health_oauth_credential_id"] = fixture.oauth.ID.String()
	metadata["meta_health_oauth_version"] = fixture.oauth.Version
	metadata["meta_health_webhook_credential_id"] = fixture.webhook.ID.String()
	metadata["meta_health_webhook_version"] = fixture.webhook.Version
	configJSON := cloneJSONB(fixture.account.Config)
	configJSON["outbound_enabled"] = false
	configJSON["ai_reply_enabled"] = false
	require.NoError(t, db.Model(&models.ChannelAccount{}).Where("id = ?", fixture.account.ID).Updates(map[string]any{
		"status": models.ChannelAccountStatusPending, "config": configJSON, "metadata": metadata,
		"last_health_check_at": now, "last_error": "",
	}).Error)

	_, err := app.loadMetaRegistryBinding(metaregistry.ResolveRequest{
		Channel: models.ChannelMessenger, ExternalAccountID: fixture.account.ExternalAccountID,
		Purpose: metaregistry.ResolvePurposeHealth,
	}, now)
	require.ErrorIs(t, err, metaregistry.ErrNotFound, "production must issue no registry lease for a USER token")
	_, err = app.activateMetaMessengerAccount(
		organization.ID,
		fixture.userID,
		fixture.account.ID,
		now,
		metaMessengerSubscriptionApproval{
			PageID: fixture.account.ExternalAccountID, CheckedAt: now,
			OAuthCredentialID: fixture.oauth.ID, OAuthCredentialVersion: fixture.oauth.Version,
			WebhookCredentialID: fixture.webhook.ID, WebhookCredentialVersion: fixture.webhook.Version,
		},
	)
	require.Error(t, err, "production approval must fail before enabling outbound")
	snapshot, err := app.loadMetaMessengerRevalidationSnapshot(organization.ID, fixture.account.ID, now)
	require.NoError(t, err)
	outcome, reason, businessAuthorityVerified := app.checkMetaMessengerOwnership(t.Context(), snapshot)
	assert.Equal(t, metaregistry.OwnershipStale, outcome)
	assert.Equal(t, "authorization_token_kind_not_allowed", reason)
	assert.False(t, businessAuthorityVerified)
}

func TestProvisionMetaRegistryBindingRequiresTokenKindMatchedBusinessAuthority(t *testing.T) {
	for _, testCase := range []struct {
		name                      string
		tokenKind                 string
		businessAuthorityVerified bool
	}{
		{
			name:      "system user without exact edge proof",
			tokenKind: metaMessengerTokenKindSystemUser,
		},
		{
			name:                      "user token cannot claim system user edge proof",
			tokenKind:                 metaMessengerTokenKindUser,
			businessAuthorityVerified: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			db := testutil.SetupTestDB(t)
			org := testutil.CreateTestOrganization(t, db)
			user := testutil.CreateTestUser(t, db, org.ID)
			app := metaRegistryTestApp(db, org.ID)
			_, err := app.provisionMetaRegistryBinding(metaRegistryProvisionInput{
				OrganizationID:                 org.ID,
				UserID:                         user.ID,
				Channel:                        models.ChannelMessenger,
				Name:                           "Rejected Messenger binding",
				ExternalAccountID:              "780000000000114",
				WebhookApp:                     "messenger",
				PlatformAppID:                  "123",
				MetaBusinessID:                 "280000000000114",
				AuthorizingMetaUserID:          "980000000000114",
				AuthorizationTokenKind:         testCase.tokenKind,
				BusinessAuthorityVerified:      testCase.businessAuthorityVerified,
				GrantedScopes:                  append([]string(nil), metaMessengerSystemUserRequiredScopes...),
				AccessToken:                    "provider-token",
				AuthorityToken:                 "authority-token",
				OwnershipCheckedAt:             time.Now().UTC(),
				ReReplyBaseURL:                 "https://app.example.test",
				RelayBaseURL:                   "https://app.example.test/meta-relay",
				SubscriptionOperationExpiresAt: time.Now().UTC().Add(metaMessengerSubscriptionOperationLease),
			})
			require.ErrorIs(t, err, metaregistry.ErrInvalidRequest)
			var count int64
			require.NoError(t, db.Model(&models.ChannelAccount{}).
				Where("organization_id = ?", org.ID).
				Count(&count).Error)
			assert.Zero(t, count)
		})
	}
}

func TestProvisionMetaRegistryBindingCreatesEncryptedAuditedTenantRecord(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, org.ID)
	root := &App{
		DB: db, Log: testutil.NopLogger(),
		Config: &config.Config{
			App: config.AppConfig{EncryptionKey: metaRegistryTestEncryptionKey},
			MetaRegistry: config.MetaRegistryConfig{
				LeaseSeconds: 30, OwnershipMaxAgeMins: 24 * 60,
			},
			MetaMessenger: config.MetaMessengerConfig{
				AppID: "123", AllowedOrganizationIDs: org.ID.String(),
			},
		},
	}
	var result metaRegistryProvisionResult
	err := root.WithCommittedTenantApp(org.ID, func(scoped *App) error {
		var provisionErr error
		result, provisionErr = scoped.provisionMetaRegistryBinding(metaRegistryProvisionInput{
			OrganizationID: org.ID, UserID: user.ID, Channel: models.ChannelMessenger,
			Name: "Synthetic Clinic Messenger", ExternalAccountID: "780000000000113",
			WebhookApp: "messenger", PlatformAppID: "123", MetaBusinessID: "280000000000113", AuthorizingMetaUserID: "980000000000113",
			AuthorizationTokenKind:    metaMessengerTokenKindSystemUser,
			BusinessAuthorityVerified: true,
			GrantedScopes:             append([]string(nil), metaMessengerSystemUserRequiredScopes...),
			AccessToken:               "plaintext-provider-token", AuthorityToken: "plaintext-authority-token",
			OwnershipCheckedAt: time.Now().UTC(),
			ReReplyBaseURL:     "https://app.example.test", RelayBaseURL: "https://app.example.test/meta-relay",
		})
		return provisionErr
	})
	require.NoError(t, err)
	assert.Equal(t, org.ID, result.Account.OrganizationID)
	assert.Equal(t, models.ChannelAccountStatusPending, result.Account.Status)
	assert.False(t, boolConfigValue(result.Account.Config, "outbound_enabled"), "outbound still requires an explicit post-health approval")
	assert.Equal(
		t,
		metaregistry.MessengerBusinessAuthoritySystemUserExactEdges,
		stringConfigValue(result.Account.Metadata, metaregistry.MessengerBusinessAuthorityMetadataKey),
	)
	assert.NotContains(t, result.Account.Metadata["meta_granted_scopes"], "business_management")
	assert.True(t, metaregistry.MessengerBusinessAuthorityCurrent(
		result.Account.Metadata,
		result.Account.ExternalAccountID,
		result.OAuthCredentialID,
		result.OAuthVersion,
	))

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
	require.Equal(t, models.ChannelAccountStatusPending, account.Status)
	require.Equal(t, metaregistry.OwnershipVerified, stringConfigValue(account.Metadata, "meta_ownership_state"))
}

func TestMetaRegistryBISUAuthorityRefreshRequiresScheduledCurrentGeneration(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	fixture := createMetaRegistryFixture(t, db, org.ID, models.ChannelMessenger, "page-bisu-refresh")
	base := time.Now().UTC().Truncate(time.Microsecond)
	metadata := cloneJSONB(fixture.account.Metadata)
	metadata["meta_platform_app_id"] = "123"
	metadata["meta_authorizing_user_id"] = "system-user-refresh"
	metadata["meta_business_id"] = "business-refresh"
	metadata[metaMessengerAuthorizationTokenKindKey] = metaMessengerTokenKindSystemUser
	metadata["meta_granted_scopes"] = append([]string(nil), metaMessengerSystemUserRequiredScopes...)
	metadata["meta_ownership_state"] = metaregistry.OwnershipVerified
	metadata["meta_ownership_checked_at"] = base.Format(time.RFC3339Nano)
	setMetaMessengerBusinessAuthorityEvidence(
		metadata,
		"123",
		"system-user-refresh",
		"business-refresh",
		fixture.account.ExternalAccountID,
		base,
		fixture.oauth.ID,
		fixture.oauth.Version,
	)
	require.NoError(t, db.Model(&models.ChannelAccount{}).
		Where("id = ?", fixture.account.ID).
		Update("metadata", metadata).Error)

	scoped := metaRegistryTestApp(db, org.ID)
	request := metaregistry.MutationRequest{
		ChannelAccountID: fixture.account.ID,
		CredentialID:     fixture.oauth.ID, CredentialVersion: fixture.oauth.Version,
		WebhookCredentialID: fixture.webhook.ID, WebhookCredentialVersion: fixture.webhook.Version,
		CheckedAt: base.Add(time.Second),
		Reason:    "scheduled_graph_revalidation",
	}
	applied, err := scoped.applyMetaRegistryMutation(request, metaregistry.OwnershipVerified)
	require.ErrorIs(t, err, metaregistry.ErrNotFound)
	require.False(t, applied, "caller-controlled reason must not refresh exact BISU authority")

	request.CheckedAt = base.Add(2 * time.Second)
	applied, err = scoped.applyMetaRegistryMutationWithMessengerAuthorityProof(
		request,
		metaregistry.OwnershipVerified,
		true,
	)
	require.NoError(t, err)
	require.True(t, applied)

	var refreshed models.ChannelAccount
	require.NoError(t, db.First(&refreshed, "id = ?", fixture.account.ID).Error)
	assert.Equal(
		t,
		request.CheckedAt.Format(time.RFC3339Nano),
		stringConfigValue(refreshed.Metadata, metaregistry.MessengerBusinessAuthorityCheckedAtMetadataKey),
	)
	assert.Equal(
		t,
		request.CheckedAt.Format(time.RFC3339Nano),
		stringConfigValue(refreshed.Metadata, "meta_ownership_checked_at"),
	)
	assert.True(t, metaregistry.MessengerBusinessAuthorityCurrent(
		refreshed.Metadata,
		refreshed.ExternalAccountID,
		fixture.oauth.ID,
		fixture.oauth.Version,
	))

	request.CheckedAt = base.Add(3 * time.Second)
	request.Reason = "provider_temporarily_unavailable"
	applied, err = scoped.applyMetaRegistryMutation(request, metaregistry.OwnershipStale)
	require.NoError(t, err)
	require.True(t, applied)
	var staleAccount models.ChannelAccount
	require.NoError(t, db.First(&staleAccount, "id = ?", fixture.account.ID).Error)
	assert.False(t, metaregistry.MessengerBusinessAuthorityCurrent(
		staleAccount.Metadata,
		staleAccount.ExternalAccountID,
		fixture.oauth.ID,
		fixture.oauth.Version,
	))
	assert.True(t, metaregistry.MessengerBusinessAuthorityGenerationBound(
		staleAccount.Metadata,
		staleAccount.ExternalAccountID,
		fixture.oauth.ID,
		fixture.oauth.Version,
	))

	request.CheckedAt = base.Add(4 * time.Second)
	request.Reason = "scheduled_graph_revalidation"
	applied, err = scoped.applyMetaRegistryMutationWithMessengerAuthorityProof(
		request,
		metaregistry.OwnershipVerified,
		true,
	)
	require.NoError(t, err)
	require.True(t, applied, "a same-generation exact Graph proof must recover a stale account")
	require.NoError(t, db.First(&refreshed, "id = ?", fixture.account.ID).Error)
	assert.True(t, metaregistry.MessengerBusinessAuthorityCurrent(
		refreshed.Metadata,
		refreshed.ExternalAccountID,
		fixture.oauth.ID,
		fixture.oauth.Version,
	))

	stale := cloneJSONB(refreshed.Metadata)
	stale[metaregistry.MessengerBusinessAuthorityOAuthVersionMetadataKey] = fixture.oauth.Version + 1
	require.NoError(t, db.Model(&models.ChannelAccount{}).
		Where("id = ?", fixture.account.ID).
		Update("metadata", stale).Error)
	request.CheckedAt = base.Add(5 * time.Second)
	applied, err = scoped.applyMetaRegistryMutationWithMessengerAuthorityProof(
		request,
		metaregistry.OwnershipVerified,
		true,
	)
	require.ErrorIs(t, err, metaregistry.ErrNotFound)
	require.False(t, applied, "stale proof generation must not be refreshed")
}

func TestMetaRegistryServiceCannotSpoofScheduledBISUAuthorityProof(t *testing.T) {
	redisClient := testutil.SetupTestRedis(t)
	if redisClient == nil {
		t.Skip("TEST_REDIS_URL is required for the authenticated mutation regression")
	}
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	fixture := createMetaRegistryFixture(t, db, org.ID, models.ChannelMessenger, "page-bisu-service-spoof")
	base := time.Now().UTC().Truncate(time.Microsecond)
	metadata := cloneJSONB(fixture.account.Metadata)
	metadata["meta_platform_app_id"] = "123"
	metadata["meta_authorizing_user_id"] = "system-user-service-spoof"
	metadata["meta_business_id"] = "business-service-spoof"
	metadata[metaMessengerAuthorizationTokenKindKey] = metaMessengerTokenKindSystemUser
	metadata["meta_granted_scopes"] = append([]string(nil), metaMessengerSystemUserRequiredScopes...)
	metadata["meta_ownership_state"] = metaregistry.OwnershipVerified
	metadata["meta_ownership_checked_at"] = base.Format(time.RFC3339Nano)
	setMetaMessengerBusinessAuthorityEvidence(
		metadata,
		"123",
		"system-user-service-spoof",
		"business-service-spoof",
		fixture.account.ExternalAccountID,
		base,
		fixture.oauth.ID,
		fixture.oauth.Version,
	)
	require.NoError(t, db.Model(&models.ChannelAccount{}).
		Where("id = ?", fixture.account.ID).
		Update("metadata", metadata).Error)

	app := metaRegistryTestApp(db, org.ID)
	app.Redis = redisClient
	app.Config.MetaRegistry.ServiceSecret = metaRegistryTestServiceSecret
	mutation := metaregistry.MutationRequest{
		ChannelAccountID: fixture.account.ID,
		CredentialID:     fixture.oauth.ID, CredentialVersion: fixture.oauth.Version,
		WebhookCredentialID: fixture.webhook.ID, WebhookCredentialVersion: fixture.webhook.Version,
		Outcome:   metaregistry.OwnershipVerified,
		Reason:    "scheduled_graph_revalidation",
		CheckedAt: base.Add(time.Second),
	}
	request := testutil.NewJSONRequest(t, mutation)
	request.RequestCtx.Request.Header.SetMethod(fasthttp.MethodPost)
	now := time.Now().UTC()
	nonce := "messenger-bisu-spoof-" + uuid.NewString()
	raw := request.RequestCtx.PostBody()
	request.RequestCtx.Request.Header.Set(
		metaregistry.TimestampHeader,
		strconv.FormatInt(now.Unix(), 10),
	)
	request.RequestCtx.Request.Header.Set(metaregistry.NonceHeader, nonce)
	request.RequestCtx.Request.Header.Set(
		metaregistry.SignatureHeader,
		metaregistry.SignRequest(
			metaRegistryTestServiceSecret,
			fasthttp.MethodPost,
			metaregistry.ReviewPath,
			now,
			nonce,
			raw,
		),
	)
	require.NoError(t, app.RecordMetaRegistryRevalidation(request))
	assert.Equal(t, fasthttp.StatusNotFound, testutil.GetResponseStatusCode(request))
	require.NoError(t, metaregistry.VerifyResponse(
		metaRegistryTestServiceSecret,
		nonce,
		request.RequestCtx.Response.StatusCode(),
		request.RequestCtx.Response.Body(),
		string(request.RequestCtx.Response.Header.Peek(metaregistry.ResponseHeader)),
	))

	var unchanged models.ChannelAccount
	require.NoError(t, db.First(&unchanged, "id = ?", fixture.account.ID).Error)
	assert.Equal(
		t,
		base.Format(time.RFC3339Nano),
		stringConfigValue(unchanged.Metadata, metaregistry.MessengerBusinessAuthorityCheckedAtMetadataKey),
	)
	assert.Equal(
		t,
		base.Format(time.RFC3339Nano),
		stringConfigValue(unchanged.Metadata, "meta_ownership_checked_at"),
	)
}

func TestGenericChannelAPIProtectsManagedMetaRoutingButAllowsSafeProfileToggle(t *testing.T) {
	fixture := newChannelAccountConcurrencyFixture(t, false)
	fixture.Account.Channel = models.ChannelMessenger
	fixture.Account.Config["meta_registry_managed"] = true
	fixture.Account.Config["meta_management_mode"] = metaregistry.ManagementModePlatformOAuth
	fixture.Account.Metadata["meta_ownership_state"] = metaregistry.OwnershipVerified
	require.NoError(t, fixture.App.DB.Model(&models.ChannelAccount{}).
		Where("id = ?", fixture.Account.ID).Updates(map[string]any{
		"channel": fixture.Account.Channel, "config": fixture.Account.Config,
		"metadata": fixture.Account.Metadata,
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

func TestGenericChannelCreateRejectsManualMessengerRelayBypass(t *testing.T) {
	db := testutil.SetupTestDB(t)
	organization := testutil.CreateTestOrganization(t, db)
	role := testutil.CreateTestRoleWithKeys(
		t, db, organization.ID, "manual-messenger-channel-writer",
		[]string{models.ResourceChannelAccounts + ":" + models.ActionWrite},
	)
	user := testutil.CreateTestUser(t, db, organization.ID, testutil.WithRoleID(&role.ID))
	enableChannelAccountConcurrencyEntitlement(t, db, organization.ID, user.ID)
	app := &App{
		DB: db, Log: testutil.NopLogger(),
		Config: &config.Config{App: config.AppConfig{EncryptionKey: metaRegistryTestEncryptionKey}},
	}
	request := testutil.NewJSONRequest(t, map[string]any{
		"channel":             "messenger",
		"provider":            channelapi.RelayProvider,
		"name":                "Manual Messenger bypass",
		"external_account_id": "manual-page-123",
		"config": map[string]any{
			"relay_url": "https://relay.example.test/meta",
		},
	})
	testutil.SetAuthContext(request, organization.ID, user.ID)
	require.NoError(t, app.CreateChannelAccount(request))
	testutil.AssertErrorResponse(t, request, fasthttp.StatusBadRequest, "managed Meta onboarding")

	for name, model := range map[string]any{
		"account":    &models.ChannelAccount{},
		"credential": &models.ChannelCredential{},
		"audit":      &models.AuditLog{},
	} {
		var count int64
		require.NoError(t, db.Model(model).Where("organization_id = ?", organization.ID).Count(&count).Error)
		assert.Zero(t, count, "manual Messenger rejection must create no %s side effect", name)
	}
}

func TestGenericChannelCreateGatesManagedInstagramPilotButPreservesOffPilotStatic(t *testing.T) {
	for _, quarantineOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("pilot quarantine=%t", quarantineOnly), func(t *testing.T) {
			db := testutil.SetupTestDB(t)
			organization := testutil.CreateTestOrganization(t, db)
			role := testutil.CreateTestRoleWithKeys(
				t, db, organization.ID, "manual-instagram-pilot-writer-"+uuid.NewString(),
				[]string{models.ResourceChannelAccounts + ":" + models.ActionWrite},
			)
			user := testutil.CreateTestUser(t, db, organization.ID, testutil.WithRoleID(&role.ID))
			enableBookingCommerceTestEntitlement(
				t, db, organization.ID, user.ID, "omnichannel.enabled",
			)
			app := &App{
				DB: db, Log: testutil.NopLogger(),
				Config: &config.Config{
					App: config.AppConfig{EncryptionKey: metaRegistryTestEncryptionKey},
					MetaInstagram: config.MetaInstagramConfig{
						Enabled: true, QuarantineOnly: quarantineOnly,
						AllowedOrganizationID: organization.ID.String(),
					},
				},
			}
			request := testutil.NewJSONRequest(t, map[string]any{
				"channel": models.ChannelInstagram, "provider": channelapi.RelayProvider,
				"name": "Synthetic pilot static bypass", "external_account_id": "700000000009981",
				"config": map[string]any{"relay_url": "https://relay.example.com/instagram"},
			})
			testutil.SetAuthContext(request, organization.ID, user.ID)
			require.NoError(t, app.CreateChannelAccount(request))
			testutil.AssertErrorResponse(t, request, fasthttp.StatusConflict, "managed Meta onboarding")
			for name, model := range map[string]any{
				"account": &models.ChannelAccount{}, "credential": &models.ChannelCredential{}, "audit": &models.AuditLog{},
			} {
				var count int64
				require.NoError(t, db.Model(model).Where("organization_id = ?", organization.ID).Count(&count).Error)
				assert.Zero(t, count, "pilot rejection must create no %s side effect", name)
			}
		})
	}

	t.Run("off-pilot static Instagram remains compatible", func(t *testing.T) {
		db := testutil.SetupTestDB(t)
		pilot := testutil.CreateTestOrganization(t, db)
		offPilot := testutil.CreateTestOrganization(t, db)
		role := testutil.CreateTestRoleWithKeys(
			t, db, offPilot.ID, "manual-instagram-off-pilot-writer-"+uuid.NewString(),
			[]string{models.ResourceChannelAccounts + ":" + models.ActionWrite},
		)
		user := testutil.CreateTestUser(t, db, offPilot.ID, testutil.WithRoleID(&role.ID))
		enableBookingCommerceTestEntitlement(
			t, db, offPilot.ID, user.ID, "omnichannel.enabled",
		)
		app := &App{
			DB: db, Log: testutil.NopLogger(),
			Config: &config.Config{
				App: config.AppConfig{EncryptionKey: metaRegistryTestEncryptionKey},
				MetaInstagram: config.MetaInstagramConfig{
					Enabled: true, AllowedOrganizationID: pilot.ID.String(),
				},
			},
		}
		request := testutil.NewJSONRequest(t, map[string]any{
			"channel": models.ChannelInstagram, "provider": channelapi.RelayProvider,
			"name": "Synthetic off-pilot static Instagram", "external_account_id": "700000000009982",
			"config": map[string]any{"relay_url": "https://relay.example.com/instagram"},
		})
		testutil.SetAuthContext(request, offPilot.ID, user.ID)
		require.NoError(t, app.CreateChannelAccount(request))
		assert.Equal(t, fasthttp.StatusOK, request.RequestCtx.Response.StatusCode())
		var accounts, credentials, audits int64
		require.NoError(t, db.Model(&models.ChannelAccount{}).Where("organization_id = ?", offPilot.ID).Count(&accounts).Error)
		require.NoError(t, db.Model(&models.ChannelCredential{}).Where("organization_id = ?", offPilot.ID).Count(&credentials).Error)
		require.NoError(t, db.Model(&models.AuditLog{}).Where("organization_id = ?", offPilot.ID).Count(&audits).Error)
		assert.Equal(t, int64(1), accounts)
		assert.Equal(t, int64(1), credentials)
		assert.Equal(t, int64(1), audits)
	})
}

func createMetaRegistryFixture(t *testing.T, db *gorm.DB, orgID uuid.UUID, channel models.Channel, externalID string) metaRegistryFixture {
	t.Helper()
	user := testutil.CreateTestUser(t, db, orgID)
	now := time.Now().UTC()
	mode := ""
	webhookApp := "messenger"
	platformAppID := "123"
	scopes := append([]string(nil), metaMessengerRequiredScopes...)
	if channel == models.ChannelInstagram {
		mode = "instagram_login"
		webhookApp = "instagram_login"
		platformAppID = "456"
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
			"meta_ownership_state":                 metaregistry.OwnershipVerified,
			"meta_ownership_checked_at":            now.Format(time.RFC3339Nano),
			metaMessengerAuthorizationGrantedAtKey: now.Add(-time.Minute).Format(time.RFC3339Nano),
			metaMessengerAuthorizationTokenKindKey: metaMessengerTokenKindSystemUser,
			"meta_webhook_app":                     webhookApp,
			"meta_platform_app_id":                 platformAppID,
			"meta_business_id":                     "business-" + externalID,
			"meta_authorizing_user_id":             "user-" + externalID,
			"meta_granted_scopes":                  scopes,
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
		CredentialBlob: models.JSONB{
			"access_token":    encrypt("provider-token-" + externalID),
			"authority_token": encrypt("authority-token-" + externalID),
		},
		Status: models.ChannelCredentialStatusActive, KeyVersion: "app:v1", Metadata: models.JSONB{},
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
			MetaMessenger: config.MetaMessengerConfig{
				AppID: "123", HealthApprovalMaxAgeMins: 15,
				AllowedOrganizationIDs: organizationID.String(),
			},
		},
	}
}

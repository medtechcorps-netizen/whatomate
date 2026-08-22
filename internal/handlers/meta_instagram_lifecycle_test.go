package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	configpkg "github.com/shridarpatil/whatomate/internal/config"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/metaregistry"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	metaInstagramTestAppID         = "100000000000101"
	metaInstagramTestAppSecret     = "synthetic-instagram-lifecycle-app-secret-at-least-32-bytes"
	metaInstagramTestEncryptionKey = "synthetic-instagram-lifecycle-encryption-key-at-least-32-bytes"
)

var metaInstagramTestProfileSequence atomic.Uint64

type metaInstagramLifecycleFixture struct {
	app       *App
	db        *gorm.DB
	org       *models.Organization
	user      *models.User
	profileID string
	account   models.ChannelAccount
	oauth     models.ChannelCredential
	webhook   models.ChannelCredential
}

type metaInstagramRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn metaInstagramRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

type metaInstagramHealthAdapter struct {
	channelapi.Adapter
	validate func(context.Context, *models.ChannelAccount) (channelapi.AccountValidationResult, error)
}

func (adapter metaInstagramHealthAdapter) ValidateAccount(
	ctx context.Context,
	account *models.ChannelAccount,
) (channelapi.AccountValidationResult, error) {
	return adapter.validate(ctx, account)
}

var metaInstagramLifecycleEndpointHandlers = []struct {
	name    string
	handler func(*App, *fastglue.Request) error
}{
	{"status", (*App).GetMetaInstagramOnboardingStatus},
	{"start", (*App).StartMetaInstagramOnboarding},
	{"reconnect", (*App).ReconnectMetaInstagramOnboarding},
	{"reconcile", (*App).ReconcileMetaInstagramSubscription},
	{"approve", (*App).ApproveMetaInstagramActivation},
	{"disconnect", (*App).DisconnectMetaInstagram},
}

func newMetaInstagramLifecycleFixture(t *testing.T) metaInstagramLifecycleFixture {
	t.Helper()
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	role := testutil.CreateTestRoleWithKeys(
		t, db, org.ID, "synthetic-instagram-lifecycle-admin-"+uuid.NewString(),
		[]string{
			models.ResourceChannelAccounts + ":" + models.ActionRead,
			models.ResourceChannelAccounts + ":" + models.ActionWrite,
			models.ResourceChannelAccounts + ":" + models.ActionDelete,
			models.ResourceSettingsIntegrations + ":" + models.ActionRead,
			models.ResourceSettingsIntegrations + ":" + models.ActionWrite,
		},
	)
	user := testutil.CreateTestUser(t, db, org.ID, testutil.WithRoleID(&role.ID))
	enableBookingCommerceTestEntitlement(t, db, org.ID, user.ID, "omnichannel.enabled")
	now := time.Now().UTC().Truncate(time.Microsecond)
	profileID := strconv.FormatUint(
		960000000000100+metaInstagramTestProfileSequence.Add(1), 10,
	)
	accountID := uuid.New()
	oauthID := uuid.New()
	webhookID := uuid.New()
	operation := metaMessengerSubscriptionOperation{
		ID: uuid.New(), OAuthCredentialID: oauthID, OAuthVersion: 1,
		WebhookCredentialID: webhookID, WebhookVersion: 1,
		DesiredState: metaMessengerSubscriptionDesiredSubscribed,
		State:        metaMessengerSubscriptionSubscribeComplete,
		ExpiresAt:    now.Add(time.Hour),
	}
	metadata := models.JSONB{
		"meta_ownership_state":                 metaregistry.OwnershipVerified,
		"meta_ownership_checked_at":            now.Add(-2 * time.Hour).Format(time.RFC3339Nano),
		"meta_ownership_reason":                "initial_exact_binding",
		metaMessengerAuthorizationGrantedAtKey: now.Add(-24 * time.Hour).Format(time.RFC3339Nano),
		metaMessengerAuthorizationTokenKindKey: metaMessengerTokenKindUser,
		"meta_webhook_app":                     "instagram_login",
		"meta_platform_app_id":                 metaInstagramTestAppID,
		"meta_authorizing_user_id":             profileID,
		metaInstagramOAuthSubjectIDKey:         profileID,
		"meta_authority_asset_id":              profileID,
		"meta_granted_scopes":                  append([]string(nil), metaInstagramRequiredScopes...),
		"meta_release_evidence_mode":           "app_review_approved",
		"meta_release_review_status":           "approved",
		"meta_subscription_state":              "verified",
		"meta_activation_state":                "active",
	}
	metadata = metadataWithMetaMessengerSubscriptionOperation(
		metadata, operation, metaMessengerSubscriptionRemoteSubscribed,
	)
	metadata[metaMessengerSubscriptionRemoteConfirmedAtKey] = now.Add(-2 * time.Hour).Format(time.RFC3339Nano)
	account := models.ChannelAccount{
		BaseModel: models.BaseModel{ID: accountID}, OrganizationID: org.ID,
		Channel: models.ChannelInstagram, Provider: channelapi.RelayProvider,
		Name: "Synthetic Instagram Profile", ExternalAccountID: profileID,
		Status: models.ChannelAccountStatusActive,
		Config: models.JSONB{
			"meta_registry_managed": true,
			"meta_management_mode":  metaregistry.ManagementModePlatformOAuth,
			"instagram_api_mode":    "instagram_login",
			"rereply_webhook_url":   "https://app.example.test/api/webhooks/channels/" + accountID.String(),
			"relay_url":             "https://relay.example.test/v1/accounts/instagram/" + profileID,
			"outbound_enabled":      true,
			"ai_reply_enabled":      false,
		},
		Metadata: metadata, Capabilities: models.JSONB{"text": true},
		CreatedByID: &user.ID, UpdatedByID: &user.ID,
	}
	require.NoError(t, db.Create(&account).Error)
	encrypt := func(value string) string {
		ciphertext, err := appcrypto.Encrypt(value, metaInstagramTestEncryptionKey)
		require.NoError(t, err)
		return ciphertext
	}
	expiresAt := now.Add(2 * time.Hour)
	oauth := models.ChannelCredential{
		BaseModel: models.BaseModel{ID: oauthID}, OrganizationID: org.ID,
		ChannelAccountID: account.ID, Kind: models.ChannelCredentialKindOAuth, Version: 1,
		CredentialBlob: models.JSONB{
			"access_token":    encrypt("synthetic-old-instagram-token"),
			"authority_token": encrypt("synthetic-old-instagram-token"),
		},
		Status: models.ChannelCredentialStatusActive, KeyVersion: "app:v1",
		ExpiresAt: &expiresAt, LastValidatedAt: &now,
		Metadata: models.JSONB{"refresh_mode": "instagram_long_lived"},
	}
	webhook := models.ChannelCredential{
		BaseModel: models.BaseModel{ID: webhookID}, OrganizationID: org.ID,
		ChannelAccountID: account.ID, Kind: models.ChannelCredentialKindWebhook, Version: 1,
		CredentialBlob: models.JSONB{
			"inbound_secret":  encrypt("synthetic-inbound-secret"),
			"outbound_secret": encrypt("synthetic-outbound-secret"),
		},
		Status: models.ChannelCredentialStatusActive, KeyVersion: "app:v1", Metadata: models.JSONB{},
	}
	require.NoError(t, db.Create(&oauth).Error)
	require.NoError(t, db.Create(&webhook).Error)
	app := &App{
		DB: db, Log: testutil.NopLogger(),
		Config: &configpkg.Config{
			App: configpkg.AppConfig{
				Environment: "production", EncryptionKey: metaInstagramTestEncryptionKey,
			},
			MetaRegistry: configpkg.MetaRegistryConfig{
				Enabled: true, LeaseSeconds: 30, OwnershipMaxAgeMins: 24 * 60,
			},
			MetaInstagram: configpkg.MetaInstagramConfig{
				Enabled: true, AppID: metaInstagramTestAppID, AppSecret: metaInstagramTestAppSecret,
				AppReviewStatus: "approved", GraphAPIVersion: "v25.0",
				AuthorizationBaseURL: "https://www.instagram.com",
				TokenBaseURL:         "https://api.instagram.com",
				GraphBaseURL:         "https://graph.instagram.test",
				ReReplyBaseURL:       "https://app.example.test", RelayBaseURL: "https://relay.example.test",
				HealthApprovalMaxAgeMins: 15, RevalidationLeadMins: 60,
				TokenRefreshLeadHours: 168, SchedulerIntervalSeconds: 60,
				AllowedOrganizationID: org.ID.String(),
			},
		},
	}
	return metaInstagramLifecycleFixture{
		app: app, db: db, org: org, user: user, profileID: profileID,
		account: account, oauth: oauth, webhook: webhook,
	}
}

func TestManagedInstagramEndpointsRejectImplicitOrganizationFallbackBeforeSideEffects(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	for _, endpoint := range metaInstagramLifecycleEndpointHandlers {
		t.Run(endpoint.name, func(t *testing.T) {
			request := testutil.NewJSONRequest(t, map[string]any{})
			testutil.SetAuthContext(request, fixture.org.ID, fixture.user.ID)
			require.NoError(t, endpoint.handler(fixture.app, request))
			testutil.AssertErrorResponse(
				t, request, fasthttp.StatusBadRequest,
				"X-Organization-ID must identify the selected organization",
			)
		})
	}
}

func TestManagedInstagramMutationsRequireIntegrationWriteBeforeRedisOrProvider(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	role := testutil.CreateTestRoleWithKeys(
		t, fixture.db, fixture.org.ID, "synthetic-instagram-channel-only",
		[]string{
			models.ResourceChannelAccounts + ":" + models.ActionWrite,
			models.ResourceChannelAccounts + ":" + models.ActionDelete,
		},
	)
	user := testutil.CreateTestUser(t, fixture.db, fixture.org.ID, testutil.WithRoleID(&role.ID))
	var providerCalls atomic.Int32
	fixture.app.Redis = nil
	fixture.app.HTTPClient = &http.Client{Transport: metaInstagramRoundTripFunc(func(*http.Request) (*http.Response, error) {
		providerCalls.Add(1)
		return nil, errors.New("unexpected provider call")
	})}
	for _, endpoint := range metaInstagramLifecycleEndpointHandlers {
		if endpoint.name == "status" {
			continue
		}
		t.Run(endpoint.name, func(t *testing.T) {
			request := testutil.NewJSONRequest(t, map[string]any{})
			testutil.SetAuthContext(request, fixture.org.ID, user.ID)
			testutil.SetHeader(request, "X-Organization-ID", fixture.org.ID.String())
			require.NoError(t, endpoint.handler(fixture.app, request))
			testutil.AssertErrorResponse(t, request, fasthttp.StatusForbidden, "Insufficient permissions")
		})
	}
	assert.Zero(t, providerCalls.Load())
}

func TestManagedInstagramRegistryRequiresServerOwnedReleaseEvidenceAndPurpose(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	scoped := fixture.app.scopedApp(fixture.db, fixture.org.ID)
	now := time.Now().UTC()
	binding, err := scoped.loadMetaRegistryBinding(metaregistry.ResolveRequest{
		Channel: models.ChannelInstagram, ExternalAccountID: fixture.profileID,
		Purpose: metaregistry.ResolvePurposeHealth,
	}, now)
	require.NoError(t, err)
	assert.Equal(t, metaInstagramTestAppID, binding.PlatformAppID)
	assert.Equal(t, "instagram_login", binding.InstagramAPIMode)

	metadata := cloneJSONB(fixture.account.Metadata)
	delete(metadata, "meta_release_evidence_mode")
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
		Where("id = ?", fixture.account.ID).Update("metadata", metadata).Error)
	_, err = scoped.loadMetaRegistryBinding(metaregistry.ResolveRequest{
		Channel: models.ChannelInstagram, ExternalAccountID: fixture.profileID,
		Purpose: metaregistry.ResolvePurposeHealth,
	}, now)
	require.ErrorIs(t, err, metaregistry.ErrNotFound)
}

func TestManagedInstagramRegistryResolutionIgnoresForeignStaticShadow(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	foreignOrganization := testutil.CreateTestOrganization(t, fixture.db)
	shadow := models.ChannelAccount{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: foreignOrganization.ID,
		Channel: models.ChannelInstagram, Provider: channelapi.RelayProvider,
		Name: "Synthetic foreign static shadow", ExternalAccountID: fixture.profileID,
		Status: models.ChannelAccountStatusPending,
		Config: models.JSONB{
			"instagram_api_mode": "instagram_login",
			"relay_url":          "https://static-relay.example.test/instagram",
		},
		Metadata: models.JSONB{}, Capabilities: models.JSONB{},
	}
	require.Error(t, fixture.db.Create(&shadow).Error,
		"global routable identity uniqueness must reject the foreign shadow")
	request := metaregistry.ResolveRequest{
		Channel: models.ChannelInstagram, ExternalAccountID: fixture.profileID,
		Purpose: metaregistry.ResolvePurposeInbound,
	}
	resolvedOrganizationID, err := fixture.app.resolveMetaRegistryOrganization(request)
	require.NoError(t, err)
	require.Equal(t, fixture.org.ID, resolvedOrganizationID)
	_, err = fixture.app.scopedApp(fixture.db, resolvedOrganizationID).
		loadMetaRegistryBinding(request, time.Now().UTC())
	require.NoError(t, err)
	var shadowCount int64
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
		Where("id = ?", shadow.ID).Count(&shadowCount).Error)
	assert.Zero(t, shadowCount)
}

func syntheticMetaInstagramProvisionInput(
	fixture metaInstagramLifecycleFixture,
	profileID string,
	operationID uuid.UUID,
) metaRegistryProvisionInput {
	now := time.Now().UTC()
	expiresAt := now.Add(24 * time.Hour)
	return metaRegistryProvisionInput{
		OrganizationID: fixture.org.ID, UserID: fixture.user.ID,
		Channel: models.ChannelInstagram, Name: "Synthetic new Instagram profile",
		ExternalAccountID: profileID, InstagramAPIMode: "instagram_login",
		WebhookApp: "instagram_login", PlatformAppID: metaInstagramTestAppID,
		MetaAuthorityAssetID: profileID, AuthorizingMetaUserID: profileID,
		AuthorizationTokenKind: metaMessengerTokenKindUser,
		GrantedScopes:          append([]string(nil), metaInstagramRequiredScopes...),
		AccessToken:            "synthetic-new-profile-token",
		AuthorityToken:         "synthetic-new-profile-token",
		TokenExpiresAt:         &expiresAt, OwnershipCheckedAt: now,
		AuthorizationStartedAt: now.Add(-time.Second),
		ReReplyBaseURL:         "https://app.example.test", RelayBaseURL: "https://relay.example.test",
		SubscriptionOperationID:        operationID,
		SubscriptionOperationExpiresAt: now.Add(5 * time.Minute),
	}
}

func TestManagedInstagramProvisionRejectsSameOrganizationStaticProfileWithoutMutation(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	profileID := "700000000009921"
	static := models.ChannelAccount{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: fixture.org.ID,
		Channel: models.ChannelInstagram, Provider: channelapi.RelayProvider,
		Name: "Synthetic legacy static profile", ExternalAccountID: profileID,
		Status: models.ChannelAccountStatusPending,
		Config: models.JSONB{
			"instagram_api_mode": "instagram_login",
			"relay_url":          "https://static.example.test/instagram",
		},
		Metadata: models.JSONB{}, Capabilities: models.JSONB{},
	}
	require.NoError(t, fixture.db.Create(&static).Error)
	var providerCalls atomic.Int32
	fixture.app.HTTPClient = &http.Client{Transport: metaInstagramRoundTripFunc(func(*http.Request) (*http.Response, error) {
		providerCalls.Add(1)
		return nil, errors.New("profile conflict must not reach subscription provider")
	})}
	err := fixture.app.WithCommittedTenantApp(fixture.org.ID, func(scoped *App) error {
		_, provisionErr := scoped.provisionMetaRegistryBinding(
			syntheticMetaInstagramProvisionInput(fixture, profileID, uuid.New()),
		)
		return provisionErr
	})
	require.ErrorIs(t, err, errMetaInstagramProfileAlreadyRegistered)
	assert.Zero(t, providerCalls.Load())
	var accounts, credentials int64
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
		Where("organization_id = ? AND channel = ? AND external_account_id = ?", fixture.org.ID, models.ChannelInstagram, profileID).
		Count(&accounts).Error)
	require.Equal(t, int64(1), accounts)
	require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
		Where("channel_account_id = ?", static.ID).Count(&credentials).Error)
	require.Zero(t, credentials)
	var staticAfter models.ChannelAccount
	require.NoError(t, fixture.db.First(&staticAfter, "id = ?", static.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusPending, staticAfter.Status)
	assert.Equal(t, static.Config, staticAfter.Config)
}

func TestManagedInstagramConcurrentFreshProvisionHasOneAccountAndSubscriptionWinner(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	profileID := "700000000009922"
	start := make(chan struct{})
	type outcome struct {
		result metaRegistryProvisionResult
		err    error
	}
	outcomes := make(chan outcome, 2)
	var subscriptionAttempts atomic.Int32
	for index := 0; index < 2; index++ {
		go func() {
			<-start
			var result metaRegistryProvisionResult
			err := fixture.app.WithCommittedTenantApp(fixture.org.ID, func(scoped *App) error {
				var provisionErr error
				result, provisionErr = scoped.provisionMetaRegistryBinding(
					syntheticMetaInstagramProvisionInput(fixture, profileID, uuid.New()),
				)
				return provisionErr
			})
			if err == nil {
				err = fixture.app.withLockedMetaInstagramSubscriptionProviderAttempt(
					t.Context(), fixture.org.ID, result.Account.ID,
					result.SubscriptionOperation, metaMessengerSubscriptionSubscribePending,
					func(models.ChannelAccount, string) error {
						subscriptionAttempts.Add(1)
						return nil
					},
				)
			}
			outcomes <- outcome{result: result, err: err}
		}()
	}
	close(start)
	results := []outcome{<-outcomes, <-outcomes}
	successes, conflicts := 0, 0
	for _, result := range results {
		switch {
		case result.err == nil:
			successes++
		case errors.Is(result.err, errMetaInstagramProfileAlreadyRegistered):
			conflicts++
		default:
			require.NoError(t, result.err)
		}
	}
	require.Equal(t, 1, successes)
	require.Equal(t, 1, conflicts)
	require.Equal(t, int32(1), subscriptionAttempts.Load())
	var accounts []models.ChannelAccount
	require.NoError(t, fixture.db.Where(
		"organization_id = ? AND channel = ? AND external_account_id = ?",
		fixture.org.ID, models.ChannelInstagram, profileID,
	).Find(&accounts).Error)
	require.Len(t, accounts, 1)
	var credentials int64
	require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
		Where("channel_account_id = ?", accounts[0].ID).Count(&credentials).Error)
	require.Equal(t, int64(2), credentials)
}

func TestManagedInstagramIdentityMismatchCannotLeaseOrAcknowledgeDeauthorization(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Redis = testutil.SetupTestRedis(t)
	metadata := cloneJSONB(fixture.account.Metadata)
	metadata["meta_authorizing_user_id"] = "700000000000999"
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
		Where("id = ?", fixture.account.ID).Update("metadata", metadata).Error)

	_, err := fixture.app.scopedApp(fixture.db, fixture.org.ID).loadMetaRegistryBinding(
		metaregistry.ResolveRequest{
			Channel: models.ChannelInstagram, ExternalAccountID: fixture.profileID,
			Purpose: metaregistry.ResolvePurposeOutbound,
		}, time.Now().UTC(),
	)
	require.Error(t, err)

	request := newMetaInstagramDeauthorizationRequest(t, signMetaInstagramLifecycleRequest(
		t, time.Now().UTC().Add(-time.Minute).Truncate(time.Second),
		fixture.profileID, metaInstagramTestAppSecret,
	))
	require.NoError(t, fixture.app.DeauthorizeMetaInstagram(request))
	assert.Equal(t, fasthttp.StatusServiceUnavailable, request.RequestCtx.Response.StatusCode())
	var account models.ChannelAccount
	require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusActive, account.Status)
	assert.NotEmpty(t, fixture.app.metaInstagramReleaseGuardReason(account, fixture.org.ID))
}

func TestManagedInstagramZeroOAuthGenerationCannotLease(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
		Where("id = ?", fixture.oauth.ID).UpdateColumn("created_at", time.Time{}).Error)
	_, err := fixture.app.scopedApp(fixture.db, fixture.org.ID).loadMetaRegistryBinding(
		metaregistry.ResolveRequest{
			Channel: models.ChannelInstagram, ExternalAccountID: fixture.profileID,
			Purpose: metaregistry.ResolvePurposeOutbound,
		}, time.Now().UTC(),
	)
	require.ErrorIs(t, err, metaregistry.ErrStaleBinding)
}

func TestManagedInstagramURLBindingRequiresExactDerivedEndpoints(t *testing.T) {
	accountID := uuid.New()
	profileID := "700000000009901"
	app := &App{Config: &configpkg.Config{MetaInstagram: configpkg.MetaInstagramConfig{
		ReReplyBaseURL: "https://app.example.test",
		RelayBaseURL:   "https://relay.example.test/meta-relay",
	}}}
	valid := models.ChannelAccount{
		BaseModel: models.BaseModel{ID: accountID},
		Channel:   models.ChannelInstagram, ExternalAccountID: profileID,
		Config: models.JSONB{
			"rereply_webhook_url": "https://app.example.test/api/webhooks/channels/" + accountID.String(),
			"relay_url":           "https://relay.example.test/meta-relay/v1/accounts/instagram/" + profileID,
		},
	}
	require.Empty(t, app.metaInstagramManagedURLBindingReason(&valid))

	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "foreign webhook host", key: "rereply_webhook_url", value: "https://attacker.example.test/collect"},
		{name: "webhook userinfo", key: "rereply_webhook_url", value: "https://user@app.example.test/api/webhooks/channels/" + accountID.String()},
		{name: "webhook query", key: "rereply_webhook_url", value: "https://app.example.test/api/webhooks/channels/" + accountID.String() + "?next=attacker"},
		{name: "webhook fragment", key: "rereply_webhook_url", value: "https://app.example.test/api/webhooks/channels/" + accountID.String() + "#attacker"},
		{name: "webhook encoded raw path", key: "rereply_webhook_url", value: "https://app.example.test/api/webhooks%2Fchannels/" + accountID.String()},
		{name: "relay deceptive suffix", key: "relay_url", value: "https://relay.example.test.attacker.invalid/meta-relay/v1/accounts/instagram/" + profileID},
		{name: "relay extra path", key: "relay_url", value: "https://relay.example.test/meta-relay/v1/accounts/instagram/" + profileID + "/collect"},
		{name: "relay query", key: "relay_url", value: "https://relay.example.test/meta-relay/v1/accounts/instagram/" + profileID + "?token=1"},
		{name: "health override", key: "health_url", value: "https://attacker.example.test/health"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			account := valid
			account.Config = cloneJSONB(valid.Config)
			account.Config[test.key] = test.value
			assert.NotEmpty(t, app.metaInstagramManagedURLBindingReason(&account))
		})
	}
}

func TestManagedInstagramRegistryRejectsSeededURLDrift(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "foreign rereply webhook", key: "rereply_webhook_url", value: "https://attacker.example.test/capture"},
		{name: "foreign relay", key: "relay_url", value: "https://attacker.example.test/relay"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newMetaInstagramLifecycleFixture(t)
			config := cloneJSONB(fixture.account.Config)
			config[test.key] = test.value
			require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
				Where("id = ?", fixture.account.ID).Update("config", config).Error)
			_, err := fixture.app.scopedApp(fixture.db, fixture.org.ID).loadMetaRegistryBinding(
				metaregistry.ResolveRequest{
					Channel: models.ChannelInstagram, ExternalAccountID: fixture.profileID,
					Purpose: metaregistry.ResolvePurposeInbound,
				}, time.Now().UTC(),
			)
			require.Error(t, err)
		})
	}
}

func TestManagedInstagramInvalidCredentialGenerationQuarantinesWithoutGraphOrLease(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, metaInstagramLifecycleFixture)
	}{
		{
			name: "oauth expiry missing",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
					Where("id = ?", fixture.oauth.ID).UpdateColumn("expires_at", nil).Error)
			},
		},
		{
			name: "oauth created in future",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
					Where("id = ?", fixture.oauth.ID).UpdateColumn("created_at", time.Now().UTC().Add(2*time.Minute)).Error)
			},
		},
		{
			name: "webhook generation missing",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
					Where("id = ?", fixture.webhook.ID).UpdateColumn("created_at", time.Time{}).Error)
			},
		},
		{
			name: "webhook created in future",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
					Where("id = ?", fixture.webhook.ID).UpdateColumn("created_at", time.Now().UTC().Add(2*time.Minute)).Error)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newMetaInstagramLifecycleFixture(t)
			test.mutate(t, fixture)
			var providerCalls atomic.Int32
			fixture.app.HTTPClient = &http.Client{Transport: metaInstagramRoundTripFunc(func(*http.Request) (*http.Response, error) {
				providerCalls.Add(1)
				return nil, errors.New("invalid generation must not reach Graph")
			})}
			fixture.app.revalidateOneMetaInstagramBinding(
				t.Context(), fixture.org.ID, fixture.account.ID, time.Now().UTC(),
			)
			assert.Zero(t, providerCalls.Load())
			var account models.ChannelAccount
			require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
			assert.Equal(t, models.ChannelAccountStatusDegraded, account.Status)
			assert.False(t, boolConfigValue(account.Config, "outbound_enabled"))
			_, err := fixture.app.scopedApp(fixture.db, fixture.org.ID).loadMetaRegistryBinding(
				metaregistry.ResolveRequest{
					Channel: models.ChannelInstagram, ExternalAccountID: fixture.profileID,
					Purpose: metaregistry.ResolvePurposeOutbound,
				}, time.Now().UTC(),
			)
			require.Error(t, err)
		})
	}
}

func TestManagedInstagramRegistryMutationFencesRecoveryAndCancelsDowngradeQueues(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	queued, dispatching := createMetaInstagramManualOutboxPair(t, fixture)
	scoped := fixture.app.scopedApp(fixture.db, fixture.org.ID)
	base, err := time.Parse(
		time.RFC3339Nano,
		stringConfigValue(fixture.account.Metadata, "meta_ownership_checked_at"),
	)
	require.NoError(t, err)
	request := metaregistry.MutationRequest{
		ChannelAccountID:         fixture.account.ID,
		CredentialID:             fixture.oauth.ID,
		CredentialVersion:        fixture.oauth.Version,
		WebhookCredentialID:      fixture.webhook.ID,
		WebhookCredentialVersion: fixture.webhook.Version,
		Outcome:                  metaregistry.OwnershipStale,
		Reason:                   "registry_health_stale",
		CheckedAt:                base.Add(time.Second),
	}
	applied, err := scoped.applyMetaRegistryMutation(request, request.Outcome)
	require.NoError(t, err)
	require.True(t, applied)
	assertMetaInstagramQueuedWorkFence(t, fixture, queued, dispatching)

	request.Outcome = metaregistry.OwnershipVerified
	request.CheckedAt = request.CheckedAt.Add(time.Second)
	applied, err = scoped.applyMetaRegistryMutation(request, request.Outcome)
	require.NoError(t, err)
	require.True(t, applied)
	var recovered models.ChannelAccount
	require.NoError(t, fixture.db.First(&recovered, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusPending, recovered.Status)
	assert.False(t, boolConfigValue(recovered.Config, "outbound_enabled"))

	fixture.app.Config.MetaInstagram.QuarantineOnly = true
	request.CheckedAt = request.CheckedAt.Add(time.Second)
	applied, err = scoped.applyMetaRegistryMutation(request, request.Outcome)
	require.ErrorIs(t, err, metaregistry.ErrNotFound)
	assert.False(t, applied)
	fixture.app.Config.MetaInstagram.QuarantineOnly = false

	metadata := cloneJSONB(recovered.Metadata)
	metadata[metaDeauthorizationPendingDigestKey] = strings.Repeat("a", 64)
	metadata[metaDeauthorizationPendingIssuedKey] = time.Now().UTC().Format(time.RFC3339)
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
		Where("id = ?", fixture.account.ID).Update("metadata", metadata).Error)
	request.CheckedAt = request.CheckedAt.Add(time.Second)
	applied, err = scoped.applyMetaRegistryMutation(request, request.Outcome)
	require.ErrorIs(t, err, metaregistry.ErrNotFound)
	assert.False(t, applied)
}

func TestManagedInstagramAuthenticatedRegistryDowngradeCancelsManualQueue(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Redis = testutil.SetupTestRedis(t)
	fixture.app.Config.MetaRegistry.ServiceSecret = metaRegistryTestServiceSecret
	fixture.app.Config.MetaRegistry.ReplayWindowSeconds = int(metaregistry.ReplayWindowFloor.Seconds())
	queued, dispatching := createMetaInstagramManualOutboxPair(t, fixture)
	resolvedOrganizationID, err := fixture.app.resolveChannelAccountOrganization(fixture.account.ID)
	require.NoError(t, err)
	require.Equal(t, fixture.org.ID, resolvedOrganizationID)
	request := metaregistry.MutationRequest{
		ChannelAccountID: fixture.account.ID,
		CredentialID:     fixture.oauth.ID, CredentialVersion: fixture.oauth.Version,
		WebhookCredentialID: fixture.webhook.ID, WebhookCredentialVersion: fixture.webhook.Version,
		Outcome: metaregistry.OwnershipStale, Reason: "synthetic_registry_health_stale",
		CheckedAt: time.Now().UTC(),
	}
	probeTx := fixture.db.Begin()
	require.NoError(t, probeTx.Error)
	probeApplied, probeErr := fixture.app.scopedApp(probeTx, fixture.org.ID).
		applyMetaRegistryMutation(request, request.Outcome)
	require.NoError(t, probeErr)
	require.True(t, probeApplied)
	require.NoError(t, probeTx.Rollback().Error)
	httpRequest := newSignedMetaRegistryMutationRequest(t, fixture, request)
	require.NoError(t, fixture.app.RecordMetaRegistryRevalidation(httpRequest))
	assert.Equal(t, fasthttp.StatusOK, httpRequest.RequestCtx.Response.StatusCode(), string(httpRequest.RequestCtx.Response.Body()))
	assertMetaInstagramQueuedWorkFence(t, fixture, queued, dispatching)
}

func TestManagedInstagramGenericOutboundDisableCancelsManualQueue(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	queued, dispatching := createMetaInstagramManualOutboxPair(t, fixture)
	request := testutil.NewJSONRequest(t, map[string]any{"outbound_enabled": false})
	testutil.SetAuthContext(request, fixture.org.ID, fixture.user.ID)
	testutil.SetHeader(request, "X-Organization-ID", fixture.org.ID.String())
	testutil.SetPathParam(request, "id", fixture.account.ID.String())
	require.NoError(t, fixture.app.UpdateChannelAccount(request))
	assert.Equal(t, fasthttp.StatusOK, request.RequestCtx.Response.StatusCode())
	assertMetaInstagramQueuedWorkFence(t, fixture, queued, dispatching)
}

func TestManagedInstagramGenericEnableRejectsRuntimeBindingDrift(t *testing.T) {
	for _, test := range []struct {
		name   string
		body   map[string]any
		mutate func(*testing.T, metaInstagramLifecycleFixture, models.JSONB)
	}{
		{
			name: "quarantine only", body: map[string]any{"outbound_enabled": true},
			mutate: func(_ *testing.T, fixture metaInstagramLifecycleFixture, _ models.JSONB) {
				fixture.app.Config.MetaInstagram.QuarantineOnly = true
			},
		},
		{
			name: "release evidence removed", body: map[string]any{"outbound_enabled": true},
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture, metadata models.JSONB) {
				delete(metadata, "meta_release_evidence_mode")
				require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
					Where("id = ?", fixture.account.ID).Update("metadata", metadata).Error)
			},
		},
		{
			name: "deauthorization pending", body: map[string]any{"ai_reply_enabled": true},
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture, metadata models.JSONB) {
				metadata[metaDeauthorizationPendingDigestKey] = strings.Repeat("a", sha256.Size*2)
				metadata[metaDeauthorizationPendingIssuedKey] = time.Now().UTC().Format(time.RFC3339)
				require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
					Where("id = ?", fixture.account.ID).Update("metadata", metadata).Error)
			},
		},
		{
			name: "profile identity drift", body: map[string]any{"outbound_enabled": true},
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture, metadata models.JSONB) {
				metadata["meta_authorizing_user_id"] = "8" + fixture.profileID
				require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
					Where("id = ?", fixture.account.ID).Update("metadata", metadata).Error)
			},
		},
		{
			name: "subscription generation drift", body: map[string]any{"outbound_enabled": true},
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture, metadata models.JSONB) {
				metadata[metaMessengerSubscriptionOAuthVersionKey] = fixture.oauth.Version + 1
				require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
					Where("id = ?", fixture.account.ID).Update("metadata", metadata).Error)
			},
		},
		{
			name: "both managed markers stripped", body: map[string]any{"outbound_enabled": true},
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture, _ models.JSONB) {
				var account models.ChannelAccount
				require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
				config := cloneJSONB(account.Config)
				delete(config, "meta_registry_managed")
				delete(config, "meta_management_mode")
				require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
					Where("id = ?", fixture.account.ID).Update("config", config).Error)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newMetaInstagramLifecycleFixture(t)
			healthAt := time.Now().UTC().Add(-time.Minute)
			metadata := cloneJSONB(fixture.account.Metadata)
			metadata["meta_health_checked_at"] = healthAt.Format(time.RFC3339Nano)
			metadata["meta_health_oauth_credential_id"] = fixture.oauth.ID.String()
			metadata["meta_health_oauth_version"] = fixture.oauth.Version
			metadata["meta_health_webhook_credential_id"] = fixture.webhook.ID.String()
			metadata["meta_health_webhook_version"] = fixture.webhook.Version
			config := cloneJSONB(fixture.account.Config)
			config["outbound_enabled"] = false
			config["ai_reply_enabled"] = false
			require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
				Where("id = ?", fixture.account.ID).Updates(map[string]any{
				"config": config, "metadata": metadata,
				"last_health_check_at": healthAt, "last_error": "", "last_error_at": nil,
			}).Error)
			test.mutate(t, fixture, cloneJSONB(metadata))
			request := testutil.NewJSONRequest(t, test.body)
			request.RequestCtx.Request.Header.SetMethod(fasthttp.MethodPut)
			testutil.SetAuthContext(request, fixture.org.ID, fixture.user.ID)
			testutil.SetHeader(request, "X-Organization-ID", fixture.org.ID.String())
			testutil.SetPathParam(request, "id", fixture.account.ID.String())
			require.NoError(t, fixture.app.UpdateChannelAccount(request))
			assert.Equal(t, fasthttp.StatusConflict, request.RequestCtx.Response.StatusCode())
			var account models.ChannelAccount
			require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
			assert.False(t, boolConfigValue(account.Config, "outbound_enabled"))
			assert.False(t, boolConfigValue(account.Config, "ai_reply_enabled"))
			var outboxCount int64
			require.NoError(t, fixture.db.Model(&models.OutboxJob{}).
				Where("organization_id = ? AND channel_account_id = ?", fixture.org.ID, fixture.account.ID).
				Count(&outboxCount).Error)
			assert.Zero(t, outboxCount)
		})
	}
}

func TestManagedInstagramResidueBlocksGenericRelayConfigEditsAfterBothMarkersAreStripped(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	config := cloneJSONB(fixture.account.Config)
	delete(config, "meta_registry_managed")
	delete(config, "meta_management_mode")
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
		Where("id = ?", fixture.account.ID).Update("config", config).Error)
	request := testutil.NewJSONRequest(t, map[string]any{
		"config": map[string]any{
			"instagram_api_mode": "instagram_login",
			"relay_url":          "https://attacker.example.test/collect",
			"health_url":         "https://attacker.example.test/collect",
		},
	})
	request.RequestCtx.Request.Header.SetMethod(fasthttp.MethodPut)
	testutil.SetAuthContext(request, fixture.org.ID, fixture.user.ID)
	testutil.SetHeader(request, "X-Organization-ID", fixture.org.ID.String())
	testutil.SetPathParam(request, "id", fixture.account.ID.String())
	require.NoError(t, fixture.app.UpdateChannelAccount(request))
	assert.Equal(t, fasthttp.StatusConflict, request.RequestCtx.Response.StatusCode())
	var after models.ChannelAccount
	require.NoError(t, fixture.db.First(&after, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, config, after.Config)
}

func TestManagedInstagramGenericHealthFailureCancelsManualQueue(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	queued, dispatching := createMetaInstagramManualOutboxPair(t, fixture)
	request := testutil.NewJSONRequest(t, map[string]any{})
	testutil.SetAuthContext(request, fixture.org.ID, fixture.user.ID)
	testutil.SetHeader(request, "X-Organization-ID", fixture.org.ID.String())
	testutil.SetPathParam(request, "id", fixture.account.ID.String())
	require.NoError(t, fixture.app.TestChannelAccount(request))
	assert.Equal(t, fasthttp.StatusOK, request.RequestCtx.Response.StatusCode())
	assertMetaInstagramQueuedWorkFence(t, fixture, queued, dispatching)
	var account models.ChannelAccount
	require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusDegraded, account.Status)
	assert.False(t, boolConfigValue(account.Config, "outbound_enabled"))
	assert.False(t, boolConfigValue(account.Config, "ai_reply_enabled"))
}

func TestManagedInstagramHealthPreflightFencesBeforeRelayAndCancelsQueues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, metaInstagramLifecycleFixture)
	}{
		{
			name: "release quarantine",
			mutate: func(_ *testing.T, fixture metaInstagramLifecycleFixture) {
				fixture.app.Config.MetaInstagram.QuarantineOnly = true
			},
		},
		{
			name: "server-owned health URL override",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				config := cloneJSONB(fixture.account.Config)
				config["health_url"] = "https://attacker.example.test/capture"
				require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
					Where("id = ?", fixture.account.ID).Update("config", config).Error)
			},
		},
		{
			name: "OAuth expiry missing",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
					Where("id = ?", fixture.oauth.ID).UpdateColumn("expires_at", nil).Error)
			},
		},
		{
			name: "release evidence drift",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				metadata := cloneJSONB(fixture.account.Metadata)
				delete(metadata, "meta_release_evidence_mode")
				require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
					Where("id = ?", fixture.account.ID).Update("metadata", metadata).Error)
			},
		},
		{
			name: "registry marker only with attacker health URL",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				config := cloneJSONB(fixture.account.Config)
				delete(config, "meta_management_mode")
				config["instagram_api_mode"] = "facebook_login"
				config["health_url"] = "https://attacker.example.test/collect"
				require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
					Where("id = ?", fixture.account.ID).Update("config", config).Error)
			},
		},
		{
			name: "platform marker only with attacker relay URL",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				config := cloneJSONB(fixture.account.Config)
				delete(config, "meta_registry_managed")
				config["instagram_api_mode"] = "facebook_login"
				config["relay_url"] = "https://attacker.example.test/collect"
				require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
					Where("id = ?", fixture.account.ID).Update("config", config).Error)
			},
		},
		{
			name: "both markers stripped with managed residue and attacker URL",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				config := cloneJSONB(fixture.account.Config)
				delete(config, "meta_registry_managed")
				delete(config, "meta_management_mode")
				config["instagram_api_mode"] = "facebook_login"
				config["health_url"] = "https://attacker.example.test/collect"
				config["relay_url"] = "https://attacker.example.test/collect"
				require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
					Where("id = ?", fixture.account.ID).Update("config", config).Error)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newMetaInstagramLifecycleFixture(t)
			queued, dispatching := createMetaInstagramManualOutboxPair(t, fixture)
			test.mutate(t, fixture)
			var providerCalls atomic.Int32
			fixture.app.HTTPClient = &http.Client{Transport: metaInstagramRoundTripFunc(func(*http.Request) (*http.Response, error) {
				providerCalls.Add(1)
				return nil, errors.New("health preflight must make zero relay calls")
			})}
			request := testutil.NewJSONRequest(t, map[string]any{})
			testutil.SetAuthContext(request, fixture.org.ID, fixture.user.ID)
			testutil.SetHeader(request, "X-Organization-ID", fixture.org.ID.String())
			testutil.SetPathParam(request, "id", fixture.account.ID.String())
			require.NoError(t, fixture.app.TestChannelAccount(request))
			assert.Equal(t, fasthttp.StatusConflict, request.RequestCtx.Response.StatusCode())
			assert.Zero(t, providerCalls.Load())
			assertMetaInstagramQueuedWorkFence(t, fixture, queued, dispatching)
			var account models.ChannelAccount
			require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
			assert.Equal(t, models.ChannelAccountStatusDegraded, account.Status)
			assert.False(t, boolConfigValue(account.Config, "outbound_enabled"))
			assert.Empty(t, stringConfigValue(account.Metadata, "meta_health_checked_at"))
		})
	}
}

func TestManagedInstagramHealthSuccessCannotCrossRuntimeMetadataDrift(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(models.JSONB)
	}{
		{
			name: "release evidence removed",
			mutate: func(metadata models.JSONB) {
				delete(metadata, "meta_release_evidence_mode")
			},
		},
		{
			name: "profile identity changed",
			mutate: func(metadata models.JSONB) {
				metadata["meta_authorizing_user_id"] = "8" + stringConfigValue(metadata, "meta_authorizing_user_id")
			},
		},
		{
			name: "subscription credential binding changed",
			mutate: func(metadata models.JSONB) {
				metadata[metaMessengerSubscriptionOAuthVersionKey] =
					intConfigValue(metadata, metaMessengerSubscriptionOAuthVersionKey) + 1
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newMetaInstagramLifecycleFixture(t)
			queued, dispatching := createMetaInstagramManualOutboxPair(t, fixture)
			require.NoError(t, fixture.db.Transaction(func(tx *gorm.DB) error {
				if err := lockChannelAIOrganizationScopeTx(tx, fixture.org.ID); err != nil {
					return err
				}
				var account models.ChannelAccount
				if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
					Where("id = ? AND organization_id = ?", fixture.account.ID, fixture.org.ID).
					First(&account).Error; err != nil {
					return err
				}
				metadata := cloneJSONB(account.Metadata)
				test.mutate(metadata)
				return tx.Model(&models.ChannelAccount{}).
					Where("id = ? AND organization_id = ?", account.ID, fixture.org.ID).
					Update("metadata", metadata).Error
			}))
			var providerCalls atomic.Int32
			fixture.app.channelAdapterFactory = func(*models.ChannelAccount) (channelapi.Adapter, error) {
				providerCalls.Add(1)
				return nil, errors.New("committed drift must fence before adapter construction")
			}
			request := testutil.NewJSONRequest(t, map[string]any{})
			testutil.SetAuthContext(request, fixture.org.ID, fixture.user.ID)
			testutil.SetHeader(request, "X-Organization-ID", fixture.org.ID.String())
			testutil.SetPathParam(request, "id", fixture.account.ID.String())
			require.NoError(t, fixture.app.TestChannelAccount(request))
			assert.Equal(t, fasthttp.StatusConflict, request.RequestCtx.Response.StatusCode())
			assert.Zero(t, providerCalls.Load())
			assertMetaInstagramQueuedWorkFence(t, fixture, queued, dispatching)
			var account models.ChannelAccount
			require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
			assert.Equal(t, models.ChannelAccountStatusDegraded, account.Status)
			assert.False(t, boolConfigValue(account.Config, "outbound_enabled"))
			assert.False(t, boolConfigValue(account.Config, "ai_reply_enabled"))
			assert.Equal(t, "quarantined", stringConfigValue(account.Metadata, "meta_activation_state"))
			assert.Empty(t, stringConfigValue(account.Metadata, "meta_health_checked_at"))
		})
	}
}

func TestManagedInstagramHealthFinalizationCannotOverwriteLifecycleWinner(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(*testing.T, metaInstagramLifecycleFixture) func(*App) (int, error)
	}{
		{
			name: "signed deauthorization",
			prepare: func(t *testing.T, fixture metaInstagramLifecycleFixture) func(*App) (int, error) {
				request := newMetaInstagramDeauthorizationRequest(t, signMetaInstagramLifecycleRequest(
					t, time.Now().UTC().Add(10*time.Second).Truncate(time.Second),
					fixture.profileID, metaInstagramTestAppSecret,
				))
				return func(app *App) (int, error) {
					err := app.DeauthorizeMetaInstagram(request)
					return request.RequestCtx.Response.StatusCode(), err
				}
			},
		},
		{
			name: "signed data deletion",
			prepare: func(t *testing.T, fixture metaInstagramLifecycleFixture) func(*App) (int, error) {
				request := newMetaInstagramDeletionRequest(t, signMetaInstagramLifecycleRequest(
					t, time.Now().UTC().Add(10*time.Second).Truncate(time.Second),
					fixture.profileID, metaInstagramTestAppSecret,
				))
				return func(app *App) (int, error) {
					err := app.DeleteMetaInstagramUserData(request)
					return request.RequestCtx.Response.StatusCode(), err
				}
			},
		},
		{
			name: "disconnect claim",
			prepare: func(_ *testing.T, fixture metaInstagramLifecycleFixture) func(*App) (int, error) {
				return func(app *App) (int, error) {
					err := app.WithCommittedTenantApp(fixture.org.ID, func(scoped *App) error {
						_, err := scoped.claimMetaInstagramDisconnect(
							fixture.org.ID, fixture.user.ID, fixture.account.ID,
							fixture.profileID, uuid.New(), time.Now().UTC().Add(5*time.Minute),
						)
						return err
					})
					return 0, err
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newMetaInstagramLifecycleFixture(t)
			fixture.app.Redis = testutil.SetupTestRedis(t)
			queued, dispatching := createMetaInstagramManualOutboxPair(t, fixture)
			request, providerStarted, releaseProvider, handlerDone :=
				startBlockedMetaInstagramHealthTest(t, fixture)
			select {
			case <-providerStarted:
			case <-time.After(5 * time.Second):
				require.FailNow(t, "managed Instagram health provider call did not start")
			}

			applyLifecycleWinner := test.prepare(t, fixture)
			type controlResult struct {
				account    models.ChannelAccount
				statusCode int
				err        error
			}
			controlPID := make(chan int, 1)
			controlReady := make(chan controlResult, 1)
			commitControl := make(chan struct{})
			controlDone := make(chan error, 1)
			go func() {
				controlDone <- fixture.db.Connection(func(connection *gorm.DB) error {
					controlTx := connection.Begin()
					if controlTx.Error != nil {
						controlPID <- 0
						return controlTx.Error
					}
					committed := false
					defer func() {
						if !committed {
							_ = controlTx.Rollback().Error
						}
					}()
					var backendPID int
					if err := controlTx.Raw("SELECT pg_backend_pid()").Scan(&backendPID).Error; err != nil {
						controlPID <- 0
						return err
					}
					controlPID <- backendPID
					threadApp := &App{
						Config: fixture.app.Config, DB: controlTx, Redis: fixture.app.Redis,
						Log: fixture.app.Log, HTTPClient: fixture.app.HTTPClient,
					}
					statusCode, mutationErr := applyLifecycleWinner(threadApp)
					if mutationErr != nil {
						controlReady <- controlResult{statusCode: statusCode, err: mutationErr}
						return mutationErr
					}
					var winner models.ChannelAccount
					if err := controlTx.Preload("Credentials", func(db *gorm.DB) *gorm.DB {
						return db.Order("version DESC")
					}).First(&winner, "id = ?", fixture.account.ID).Error; err != nil {
						controlReady <- controlResult{statusCode: statusCode, err: err}
						return err
					}
					controlReady <- controlResult{account: winner, statusCode: statusCode}
					<-commitControl
					if err := controlTx.Commit().Error; err != nil {
						return err
					}
					committed = true
					return nil
				})
			}()
			backendPID := <-controlPID
			require.Positive(t, backendPID)
			testutil.RequirePostgresBackendWaitingForLock(t, fixture.db, backendPID)
			close(releaseProvider)
			var winnerResult controlResult
			select {
			case winnerResult = <-controlReady:
				require.NoError(t, winnerResult.err)
			case <-time.After(5 * time.Second):
				require.FailNow(t, "managed Instagram lifecycle winner did not acquire the organization mutex")
			}
			if test.name != "disconnect claim" {
				assert.Equal(t, fasthttp.StatusOK, winnerResult.statusCode)
			}
			winnerConfig := cloneJSONB(winnerResult.account.Config)
			winnerMetadata := cloneJSONB(winnerResult.account.Metadata)
			winnerStatus := winnerResult.account.Status
			winnerCredentials := append(
				[]models.ChannelCredential(nil), winnerResult.account.Credentials...,
			)
			close(commitControl)
			require.NoError(t, <-controlDone)
			select {
			case err := <-handlerDone:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				require.FailNow(t, "managed Instagram health finalization did not resume")
			}
			assert.Equal(t, fasthttp.StatusConflict, request.RequestCtx.Response.StatusCode())
			var after models.ChannelAccount
			require.NoError(t, fixture.db.Preload("Credentials", func(db *gorm.DB) *gorm.DB {
				return db.Order("version DESC")
			}).First(&after, "id = ?", fixture.account.ID).Error)
			assert.Equal(t, winnerStatus, after.Status)
			assert.Equal(t, winnerConfig, after.Config)
			assert.Equal(t, winnerMetadata, after.Metadata)
			require.Len(t, after.Credentials, len(winnerCredentials))
			for index := range winnerCredentials {
				assert.Equal(t, winnerCredentials[index].ID, after.Credentials[index].ID)
				assert.Equal(t, winnerCredentials[index].Version, after.Credentials[index].Version)
				assert.Equal(t, winnerCredentials[index].Status, after.Credentials[index].Status)
			}
			assertMetaInstagramQueuedWorkFence(t, fixture, queued, dispatching)
		})
	}
}

func TestManagedInstagramSchedulingBeforeRegistryDowngradeIsAtomicallyCancelled(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	base, err := time.Parse(
		time.RFC3339Nano,
		stringConfigValue(fixture.account.Metadata, "meta_ownership_checked_at"),
	)
	require.NoError(t, err)
	enqueueTx := fixture.db.Begin()
	require.NoError(t, enqueueTx.Error)
	t.Cleanup(func() { _ = enqueueTx.Rollback().Error })
	require.NoError(t, lockChannelAIOrganizationScopeTx(enqueueTx, fixture.org.ID))
	messageID := uuid.New()
	job := models.ScheduledJob{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: fixture.org.ID,
		Kind: models.ScheduledJobKindChannelAIReply, AggregateType: models.ChannelAIReplyAggregateType,
		AggregateID: &messageID, RunAt: time.Now().UTC(), Status: models.ScheduledJobStatusPending,
		MaxAttempts: 5, IdempotencyKey: models.ChannelAIReplyIdempotencyKey(messageID), Version: 1,
		Payload: models.JSONB{"channel_account_id": fixture.account.ID.String()},
	}
	require.NoError(t, enqueueTx.Create(&job).Error)

	downgradePID := make(chan int, 1)
	downgradeDone := make(chan error, 1)
	go func() {
		downgradeDone <- fixture.db.Connection(func(connection *gorm.DB) error {
			session := connection.Session(&gorm.Session{NewDB: true})
			var backendPID int
			if err := session.Raw("SELECT pg_backend_pid()").Scan(&backendPID).Error; err != nil {
				downgradePID <- 0
				return err
			}
			downgradePID <- backendPID
			return session.Transaction(func(tx *gorm.DB) error {
				scoped := fixture.app.scopedApp(tx, fixture.org.ID)
				applied, mutationErr := scoped.applyMetaRegistryMutation(
					metaregistry.MutationRequest{
						ChannelAccountID: fixture.account.ID,
						CredentialID:     fixture.oauth.ID, CredentialVersion: fixture.oauth.Version,
						WebhookCredentialID:      fixture.webhook.ID,
						WebhookCredentialVersion: fixture.webhook.Version,
						Outcome:                  metaregistry.OwnershipStale,
						Reason:                   "concurrent_release_downgrade", CheckedAt: base.Add(time.Second),
					},
					metaregistry.OwnershipStale,
				)
				if mutationErr == nil && !applied {
					return errors.New("registry downgrade was not applied")
				}
				return mutationErr
			})
		})
	}()
	backendPID := <-downgradePID
	require.Positive(t, backendPID)
	testutil.RequirePostgresBackendWaitingForLock(t, fixture.db, backendPID)
	require.NoError(t, enqueueTx.Commit().Error)
	select {
	case err := <-downgradeDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "registry downgrade did not resume after scheduling committed")
	}
	require.NoError(t, fixture.db.First(&job, "id = ?", job.ID).Error)
	assert.Equal(t, models.ScheduledJobStatusCancelled, job.Status)
	var account models.ChannelAccount
	require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusDegraded, account.Status)
	assert.False(t, boolConfigValue(account.Config, "outbound_enabled"))
}

func TestManagedInstagramDevelopmentEvidenceIsExactProfileAndImpossibleInProduction(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	metadata := cloneJSONB(fixture.account.Metadata)
	metadata["meta_release_evidence_mode"] = "development_app_role"
	metadata["meta_release_review_status"] = "not_submitted"
	metadata["meta_release_profile_id"] = fixture.profileID
	metadata["meta_release_oauth_subject_id"] = fixture.profileID
	metadata["meta_release_app_role"] = "tester"
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
		Where("id = ?", fixture.account.ID).Update("metadata", metadata).Error)
	var account models.ChannelAccount
	require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)

	fixture.app.Config.App.Environment = "development"
	fixture.app.Config.MetaInstagram.AppReviewStatus = "not_submitted"
	fixture.app.Config.MetaInstagram.DevelopmentTestProfileID = fixture.profileID
	fixture.app.Config.MetaInstagram.DevelopmentTestOAuthSubjectID = fixture.profileID
	fixture.app.Config.MetaInstagram.DevelopmentAppRole = "tester"
	assert.True(t, metaInstagramReleaseEvidenceAllowed(fixture.app.Config, &account))

	fixture.app.Config.MetaInstagram.DevelopmentTestProfileID = "700000000000999"
	fixture.app.Config.MetaInstagram.DevelopmentTestOAuthSubjectID = fixture.profileID
	assert.False(t, metaInstagramReleaseEvidenceAllowed(fixture.app.Config, &account))
	fixture.app.Config.MetaInstagram.DevelopmentTestProfileID = fixture.profileID
	fixture.app.Config.MetaInstagram.DevelopmentTestOAuthSubjectID = fixture.profileID
	fixture.app.Config.App.Environment = "prod"
	assert.False(t, metaInstagramReleaseEvidenceAllowed(fixture.app.Config, &account))
	fixture.app.Config.App.Environment = "qa"
	assert.False(t, metaInstagramReleaseEvidenceAllowed(fixture.app.Config, &account))
	fixture.app.Config.App.Environment = "production"
	assert.False(t, metaInstagramReleaseEvidenceAllowed(fixture.app.Config, &account))
}

func TestManagedInstagramDevelopmentCallbackRejectsWrongShortTokenProfileBeforeGraphCalls(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Redis = testutil.SetupTestRedis(t)
	fixture.app.Config.App.Environment = "development"
	fixture.app.Config.MetaInstagram.AppReviewStatus = "not_submitted"
	fixture.app.Config.MetaInstagram.DevelopmentTestProfileID = fixture.profileID
	fixture.app.Config.MetaInstagram.DevelopmentTestOAuthSubjectID = "8" + fixture.profileID
	fixture.app.Config.MetaInstagram.DevelopmentAppRole = "tester"
	wrongProfileID := strconv.FormatUint(
		700000000100000+metaInstagramTestProfileSequence.Add(1), 10,
	)
	var shortExchangeCalls atomic.Int32
	var postShortExchangeCalls atomic.Int32
	fixture.app.HTTPClient = &http.Client{Transport: metaInstagramRoundTripFunc(
		func(request *http.Request) (*http.Response, error) {
			if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/oauth/access_token") {
				shortExchangeCalls.Add(1)
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader(
						`{"data":[{"access_token":"synthetic-wrong-profile-short-token","user_id":"` +
							wrongProfileID + `","permissions":"instagram_business_basic,instagram_business_manage_messages"}]}`,
					)),
					Request: request,
				}, nil
			}
			postShortExchangeCalls.Add(1)
			return nil, errors.New("unexpected post-short-exchange provider call")
		},
	)}
	now := time.Now().UTC()
	nonce := "synthetic-dev-profile-state-" + uuid.NewString()
	state := metaInstagramOAuthState{
		OrganizationID: fixture.org.ID.String(), UserID: fixture.user.ID.String(), Nonce: nonce,
		ConfigFingerprint: fixture.app.metaInstagramRuntimeFingerprint(
			fixture.app.Config.MetaInstagram, fixture.org.ID,
		),
		IssuedAt: now, ExpiresAt: now.Add(metaInstagramOAuthStateTTL),
	}
	browserVerifier := generateRandomString(metaInstagramOAuthBrowserSecretSize)
	state.BrowserBindingDigest = metaInstagramOAuthBrowserBindingDigest(state, browserVerifier)
	payload, err := json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, fixture.app.Redis.Set(
		t.Context(), metaInstagramOAuthStateKey(nonce), payload, metaInstagramOAuthStateTTL,
	).Err())
	var before int64
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
		Where("channel = ?", models.ChannelInstagram).Count(&before).Error)
	request := testutil.NewRequest(t)
	request.RequestCtx.QueryArgs().Set("state", nonce)
	request.RequestCtx.QueryArgs().Set("code", "synthetic-authorization-code")
	request.RequestCtx.Request.Header.SetCookie(
		metaInstagramOAuthBrowserCookieName(nonce), browserVerifier,
	)
	require.NoError(t, fixture.app.CallbackMetaInstagram(request))
	assert.Equal(t, fasthttp.StatusSeeOther, request.RequestCtx.Response.StatusCode())
	assert.Equal(t, int32(1), shortExchangeCalls.Load())
	assert.Zero(t, postShortExchangeCalls.Load())
	var after int64
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
		Where("channel = ?", models.ChannelInstagram).Count(&after).Error)
	assert.Equal(t, before, after)
}

func TestManagedInstagramQuarantineOnlyDowngradeCancelsQueuesWithoutGraphCall(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	queued, dispatching := createMetaInstagramManualOutboxPair(t, fixture)
	messageID := uuid.New()
	scheduled := models.ScheduledJob{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: fixture.org.ID,
		Kind: models.ScheduledJobKindChannelAIReply, AggregateType: models.ChannelAIReplyAggregateType,
		AggregateID: &messageID, RunAt: time.Now().UTC(), Status: models.ScheduledJobStatusPending,
		MaxAttempts: 5, IdempotencyKey: models.ChannelAIReplyIdempotencyKey(messageID), Version: 1,
		Payload: models.JSONB{"channel_account_id": fixture.account.ID.String()},
	}
	require.NoError(t, fixture.db.Create(&scheduled).Error)
	var providerCalls atomic.Int32
	fixture.app.HTTPClient = &http.Client{Transport: metaInstagramRoundTripFunc(func(*http.Request) (*http.Response, error) {
		providerCalls.Add(1)
		return nil, errors.New("provider must not be called after release downgrade")
	})}
	fixture.app.Config.MetaInstagram.QuarantineOnly = true
	fixture.app.Config.MetaInstagram.AppReviewStatus = "rejected"
	fixture.app.revalidateOneMetaInstagramBinding(
		t.Context(), fixture.org.ID, fixture.account.ID, time.Now().UTC(),
	)
	assert.Zero(t, providerCalls.Load())
	var account models.ChannelAccount
	require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusDegraded, account.Status)
	assert.Equal(t, metaregistry.OwnershipStale, stringConfigValue(account.Metadata, "meta_ownership_state"))
	assert.Equal(t, "managed_release_evidence_invalid", stringConfigValue(account.Metadata, "meta_ownership_reason"))
	assert.False(t, boolConfigValue(account.Config, "outbound_enabled"))
	assert.False(t, boolConfigValue(account.Config, "ai_reply_enabled"))
	assertMetaInstagramQueuedWorkFence(t, fixture, queued, dispatching)
	require.NoError(t, fixture.db.First(&scheduled, "id = ?", scheduled.ID).Error)
	assert.Equal(t, models.ScheduledJobStatusCancelled, scheduled.Status)
	_, err := fixture.app.scopedApp(fixture.db, fixture.org.ID).loadMetaRegistryBinding(
		metaregistry.ResolveRequest{
			Channel: models.ChannelInstagram, ExternalAccountID: fixture.profileID,
			Purpose: metaregistry.ResolvePurposeHealth,
		}, time.Now().UTC(),
	)
	require.ErrorIs(t, err, metaregistry.ErrStaleBinding)
	assert.False(t, fixture.app.metaInstagramOnboardingAvailable(fixture.org.ID))
	var credentials []models.ChannelCredential
	require.NoError(t, fixture.db.Where("channel_account_id = ?", account.ID).Find(&credentials).Error)
	for _, credential := range credentials {
		assert.Equal(t, models.ChannelCredentialStatusActive, credential.Status)
	}
}

func TestManagedInstagramQuarantineStartupBarrierAtomicallySettlesAllowedTenantOnly(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	foreignFixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Config.MetaInstagram.QuarantineOnly = true
	activeQueued, activeDispatching := createMetaInstagramManualOutboxPair(t, fixture)
	foreignQueued, _ := createMetaInstagramManualOutboxPair(t, foreignFixture)

	pendingFixture := fixture
	pendingFixture.profileID = strconv.FormatUint(
		800000000000100+metaInstagramTestProfileSequence.Add(1), 10,
	)
	pendingFixture.account = fixture.account
	pendingFixture.account.BaseModel = models.BaseModel{ID: uuid.New()}
	pendingFixture.account.Name = "Synthetic pending Instagram residue"
	pendingFixture.account.ExternalAccountID = pendingFixture.profileID
	pendingFixture.account.Status = models.ChannelAccountStatusPending
	pendingFixture.account.Config = cloneJSONB(fixture.account.Config)
	delete(pendingFixture.account.Config, "meta_registry_managed")
	delete(pendingFixture.account.Config, "meta_management_mode")
	pendingFixture.account.Config["rereply_webhook_url"] = "https://app.example.test/api/webhooks/channels/" + pendingFixture.account.ID.String()
	pendingFixture.account.Config["relay_url"] = "https://relay.example.test/v1/accounts/instagram/" + pendingFixture.profileID
	pendingFixture.account.Config["outbound_enabled"] = true
	pendingFixture.account.Config["ai_reply_enabled"] = true
	pendingFixture.account.Metadata = cloneJSONB(fixture.account.Metadata)
	pendingFixture.account.Metadata["meta_authorizing_user_id"] = pendingFixture.profileID
	pendingFixture.account.Metadata["meta_authority_asset_id"] = pendingFixture.profileID
	require.NoError(t, fixture.db.Create(&pendingFixture.account).Error)
	pendingQueued, pendingDispatching := createMetaInstagramManualOutboxPair(t, pendingFixture)

	staticFixture := fixture
	staticFixture.profileID = strconv.FormatUint(
		800000000000200+metaInstagramTestProfileSequence.Add(1), 10,
	)
	staticFixture.account = fixture.account
	staticFixture.account.BaseModel = models.BaseModel{ID: uuid.New()}
	staticFixture.account.Name = "Synthetic static Instagram account"
	staticFixture.account.ExternalAccountID = staticFixture.profileID
	staticFixture.account.Config = models.JSONB{
		"instagram_api_mode": "static_relay",
		"relay_url":          "https://static.example.test/instagram",
		"outbound_enabled":   true,
		"ai_reply_enabled":   true,
	}
	staticFixture.account.Metadata = models.JSONB{}
	require.NoError(t, fixture.db.Create(&staticFixture.account).Error)
	staticQueued, _ := createMetaInstagramManualOutboxPair(t, staticFixture)

	messengerFixture := fixture
	messengerFixture.profileID = strconv.FormatUint(
		800000000000300+metaInstagramTestProfileSequence.Add(1), 10,
	)
	messengerFixture.account = fixture.account
	messengerFixture.account.BaseModel = models.BaseModel{ID: uuid.New()}
	messengerFixture.account.Channel = models.ChannelMessenger
	messengerFixture.account.Name = "Synthetic managed Messenger account"
	messengerFixture.account.ExternalAccountID = messengerFixture.profileID
	messengerFixture.account.Config = cloneJSONB(fixture.account.Config)
	messengerFixture.account.Metadata = cloneJSONB(fixture.account.Metadata)
	require.NoError(t, fixture.db.Create(&messengerFixture.account).Error)
	messengerQueued, _ := createMetaInstagramManualOutboxPair(t, messengerFixture)

	createScheduled := func(accountID uuid.UUID, status models.ScheduledJobStatus) models.ScheduledJob {
		messageID := uuid.New()
		job := models.ScheduledJob{
			BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: fixture.org.ID,
			Kind: models.ScheduledJobKindChannelAIReply, AggregateType: models.ChannelAIReplyAggregateType,
			AggregateID: &messageID, RunAt: time.Now().UTC(), Status: status,
			MaxAttempts: 5, IdempotencyKey: models.ChannelAIReplyIdempotencyKey(messageID), Version: 1,
			Payload: models.JSONB{"channel_account_id": accountID.String()},
		}
		require.NoError(t, fixture.db.Create(&job).Error)
		return job
	}
	activeScheduled := createScheduled(fixture.account.ID, models.ScheduledJobStatusProcessing)
	pendingScheduled := createScheduled(pendingFixture.account.ID, models.ScheduledJobStatusPending)
	staticScheduled := createScheduled(staticFixture.account.ID, models.ScheduledJobStatusPending)
	messengerScheduled := createScheduled(messengerFixture.account.ID, models.ScheduledJobStatusPending)

	var providerCalls atomic.Int32
	fixture.app.HTTPClient = &http.Client{Transport: metaInstagramRoundTripFunc(func(*http.Request) (*http.Response, error) {
		providerCalls.Add(1)
		return nil, errors.New("startup quarantine must never call Graph")
	})}
	require.NoError(t, fixture.app.ReconcileMetaInstagramQuarantineStartup(t.Context()))
	assert.Zero(t, providerCalls.Load())

	for _, accountID := range []uuid.UUID{fixture.account.ID, pendingFixture.account.ID} {
		var account models.ChannelAccount
		require.NoError(t, fixture.db.First(&account, "id = ?", accountID).Error)
		assert.Equal(t, models.ChannelAccountStatusDegraded, account.Status)
		assert.False(t, boolConfigValue(account.Config, "outbound_enabled"))
		assert.False(t, boolConfigValue(account.Config, "ai_reply_enabled"))
		assert.Equal(t, metaregistry.OwnershipStale, stringConfigValue(account.Metadata, "meta_ownership_state"))
	}
	assertMetaInstagramQueuedWorkFence(t, fixture, activeQueued, activeDispatching)
	assertMetaInstagramQueuedWorkFence(t, pendingFixture, pendingQueued, pendingDispatching)
	for _, job := range []*models.ScheduledJob{&activeScheduled, &pendingScheduled} {
		require.NoError(t, fixture.db.First(job, "id = ?", job.ID).Error)
		assert.Equal(t, models.ScheduledJobStatusCancelled, job.Status)
	}

	for _, unchanged := range []struct {
		accountID uuid.UUID
		outbox    *models.OutboxJob
		scheduled *models.ScheduledJob
	}{
		{staticFixture.account.ID, &staticQueued, &staticScheduled},
		{messengerFixture.account.ID, &messengerQueued, &messengerScheduled},
	} {
		var account models.ChannelAccount
		require.NoError(t, fixture.db.First(&account, "id = ?", unchanged.accountID).Error)
		assert.Equal(t, models.ChannelAccountStatusActive, account.Status)
		assert.True(t, boolConfigValue(account.Config, "outbound_enabled"))
		require.NoError(t, fixture.db.First(unchanged.outbox, "id = ?", unchanged.outbox.ID).Error)
		assert.Equal(t, models.OutboxJobStatusProcessing, unchanged.outbox.Status)
		require.NoError(t, fixture.db.First(unchanged.scheduled, "id = ?", unchanged.scheduled.ID).Error)
		assert.Equal(t, models.ScheduledJobStatusPending, unchanged.scheduled.Status)
	}
	var foreignAccount models.ChannelAccount
	require.NoError(t, fixture.db.First(
		&foreignAccount, "id = ?", foreignFixture.account.ID,
	).Error)
	assert.Equal(t, models.ChannelAccountStatusActive, foreignAccount.Status)
	assert.True(t, boolConfigValue(foreignAccount.Config, "outbound_enabled"))
	require.NoError(t, fixture.db.First(&foreignQueued, "id = ?", foreignQueued.ID).Error)
	assert.Equal(t, models.OutboxJobStatusProcessing, foreignQueued.Status)
}

func TestManagedInstagramQuarantineSweepCommitsBeforeJournalCleanupFailureWithoutProvider(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Config.MetaInstagram.QuarantineOnly = true
	queued, dispatching := createMetaInstagramManualOutboxPair(t, fixture)
	var providerCalls atomic.Int32
	fixture.app.HTTPClient = &http.Client{Transport: metaInstagramRoundTripFunc(func(*http.Request) (*http.Response, error) {
		providerCalls.Add(1)
		return nil, errors.New("quarantine barrier must never call the provider")
	})}
	cleanupFailure := errors.New("synthetic journal cleanup failure")
	var cleanupCalls atomic.Int32
	err := fixture.app.revalidateDueMetaInstagramBindingsWithJournalCleanup(
		t.Context(),
		func(time.Time) error {
			cleanupCalls.Add(1)
			return cleanupFailure
		},
	)
	require.ErrorIs(t, err, cleanupFailure)
	assert.Equal(t, int32(1), cleanupCalls.Load())
	assert.Zero(t, providerCalls.Load())
	assertMetaInstagramQueuedWorkFence(t, fixture, queued, dispatching)
	var account models.ChannelAccount
	require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusDegraded, account.Status)
}

func TestManagedInstagramQuarantineStartupBarrierReturnsDatabaseReconciliationError(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Config.MetaInstagram.QuarantineOnly = true
	fixture.app.Config.MetaInstagram.AllowedOrganizationID = uuid.NewString()
	err := ReconcileMetaInstagramQuarantineStartup(
		t.Context(), fixture.db, fixture.app.Config,
	)
	require.Error(t, err)
	var account models.ChannelAccount
	require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusActive, account.Status)
	assert.True(t, boolConfigValue(account.Config, "outbound_enabled"))
}

func TestManagedInstagramLifecycleDiscoversPartialIntentAndLeavesStaticRowsUntouched(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(models.JSONB)
	}{
		{
			name: "registry marker only",
			mutate: func(config models.JSONB) {
				delete(config, "meta_management_mode")
			},
		},
		{
			name: "platform marker only",
			mutate: func(config models.JSONB) {
				delete(config, "meta_registry_managed")
			},
		},
		{
			name: "both markers stripped with managed residue",
			mutate: func(config models.JSONB) {
				delete(config, "meta_registry_managed")
				delete(config, "meta_management_mode")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newMetaInstagramLifecycleFixture(t)
			fixture.app.Redis = testutil.SetupTestRedis(t)
			config := cloneJSONB(fixture.account.Config)
			test.mutate(config)
			require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
				Where("id = ?", fixture.account.ID).Update("config", config).Error)

			queued, dispatching := createMetaInstagramManualOutboxPair(t, fixture)
			pending := queued
			pending.BaseModel = models.BaseModel{ID: uuid.New()}
			pending.IdempotencyKey = "ig-partial-pending:" + uuid.NewString()
			pending.Status = models.OutboxJobStatusPending
			pending.LockedAt = nil
			pending.LockedBy = ""
			retrying := pending
			retrying.BaseModel = models.BaseModel{ID: uuid.New()}
			retrying.IdempotencyKey = "ig-partial-retrying:" + uuid.NewString()
			retrying.Status = models.OutboxJobStatusRetrying
			require.NoError(t, fixture.db.Create(&pending).Error)
			require.NoError(t, fixture.db.Create(&retrying).Error)
			messageID := uuid.New()
			scheduled := models.ScheduledJob{
				BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: fixture.org.ID,
				Kind: models.ScheduledJobKindChannelAIReply, AggregateType: models.ChannelAIReplyAggregateType,
				AggregateID: &messageID, RunAt: time.Now().UTC(), Status: models.ScheduledJobStatusPending,
				MaxAttempts: 5, IdempotencyKey: models.ChannelAIReplyIdempotencyKey(messageID), Version: 1,
				Payload: models.JSONB{"channel_account_id": fixture.account.ID.String()},
			}
			require.NoError(t, fixture.db.Create(&scheduled).Error)
			var providerCalls atomic.Int32
			fixture.app.HTTPClient = &http.Client{Transport: metaInstagramRoundTripFunc(func(*http.Request) (*http.Response, error) {
				providerCalls.Add(1)
				return nil, errors.New("partial managed intent must be quarantined before Graph")
			})}

			organizations, err := fixture.app.metaInstagramLifecycleOrganizations(uuid.Nil)
			require.NoError(t, err)
			assert.Contains(t, organizations, fixture.org.ID)
			require.NoError(t, fixture.app.revalidateMetaInstagramOrganization(
				t.Context(), fixture.org.ID, time.Now().UTC(),
			))
			assert.Zero(t, providerCalls.Load())
			assertMetaInstagramQueuedWorkFence(t, fixture, queued, dispatching)
			for _, job := range []*models.OutboxJob{&pending, &retrying} {
				require.NoError(t, fixture.db.First(job, "id = ?", job.ID).Error)
				assert.Equal(t, models.OutboxJobStatusCancelled, job.Status)
			}
			require.NoError(t, fixture.db.First(&scheduled, "id = ?", scheduled.ID).Error)
			assert.Equal(t, models.ScheduledJobStatusCancelled, scheduled.Status)
			var account models.ChannelAccount
			require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
			assert.Equal(t, models.ChannelAccountStatusDegraded, account.Status)
			assert.Equal(t, "managed_instagram_binding_invalid", stringConfigValue(account.Metadata, "meta_ownership_reason"))
			assert.False(t, boolConfigValue(account.Config, "outbound_enabled"))
		})
	}

	t.Run("static row with no reserved markers is not discovered", func(t *testing.T) {
		fixture := newMetaInstagramLifecycleFixture(t)
		fixture.app.Redis = testutil.SetupTestRedis(t)
		config := cloneJSONB(fixture.account.Config)
		delete(config, "meta_registry_managed")
		delete(config, "meta_management_mode")
		config["instagram_api_mode"] = "static_relay"
		config["rereply_webhook_url"] = ""
		config["relay_url"] = "https://static.example.test/instagram"
		require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
			Where("id = ?", fixture.account.ID).Updates(map[string]any{
			"config": config, "metadata": models.JSONB{},
		}).Error)
		require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
			Where("id = ?", fixture.webhook.ID).Updates(map[string]any{
			"status": models.ChannelCredentialStatusRevoked, "revoked_at": time.Now().UTC(),
		}).Error)
		queued, _ := createMetaInstagramManualOutboxPair(t, fixture)
		organizations, err := fixture.app.metaInstagramLifecycleOrganizations(uuid.Nil)
		require.NoError(t, err)
		assert.Equal(t, []uuid.UUID{fixture.org.ID}, organizations,
			"the scheduler routes only through deployment config, then tenant-scoped discovery ignores static rows")
		require.NoError(t, fixture.app.revalidateMetaInstagramOrganization(
			t.Context(), fixture.org.ID, time.Now().UTC(),
		))
		var account models.ChannelAccount
		require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
		assert.Equal(t, models.ChannelAccountStatusActive, account.Status)
		require.NoError(t, fixture.db.First(&queued, "id = ?", queued.ID).Error)
		assert.Equal(t, models.OutboxJobStatusProcessing, queued.Status)
	})
}

func TestManagedInstagramLifecycleSweepNeverMutatesForeignManagedResidue(t *testing.T) {
	allowed := newMetaInstagramLifecycleFixture(t)
	foreign := newMetaInstagramLifecycleFixture(t)
	allowed.app.Redis = testutil.SetupTestRedis(t)

	allowedConfig := cloneJSONB(allowed.account.Config)
	delete(allowedConfig, "meta_management_mode")
	require.NoError(t, allowed.db.Model(&models.ChannelAccount{}).
		Where("id = ?", allowed.account.ID).Update("config", allowedConfig).Error)
	foreignConfig := cloneJSONB(foreign.account.Config)
	delete(foreignConfig, "meta_management_mode")
	delete(foreignConfig, "meta_registry_managed")
	require.NoError(t, foreign.db.Model(&models.ChannelAccount{}).
		Where("id = ?", foreign.account.ID).Update("config", foreignConfig).Error)
	allowedQueued, allowedDispatching := createMetaInstagramManualOutboxPair(t, allowed)
	foreignQueued, _ := createMetaInstagramManualOutboxPair(t, foreign)

	var providerCalls atomic.Int32
	allowed.app.HTTPClient = &http.Client{Transport: metaInstagramRoundTripFunc(func(*http.Request) (*http.Response, error) {
		providerCalls.Add(1)
		return nil, errors.New("partial intent must be quarantined before Graph")
	})}
	organizations, err := allowed.app.metaInstagramLifecycleOrganizations(uuid.Nil)
	require.NoError(t, err)
	assert.Contains(t, organizations, allowed.org.ID)
	assert.NotContains(t, organizations, foreign.org.ID)
	require.NoError(t, allowed.app.revalidateDueMetaInstagramBindings(t.Context()))
	assert.Zero(t, providerCalls.Load())
	assertMetaInstagramQueuedWorkFence(t, allowed, allowedQueued, allowedDispatching)

	var foreignAfter models.ChannelAccount
	require.NoError(t, foreign.db.First(&foreignAfter, "id = ?", foreign.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusActive, foreignAfter.Status)
	assert.True(t, boolConfigValue(foreignAfter.Config, "outbound_enabled"))
	require.NoError(t, foreign.db.First(&foreignQueued, "id = ?", foreignQueued.ID).Error)
	assert.Equal(t, models.OutboxJobStatusProcessing, foreignQueued.Status)
}

func TestManagedInstagramFutureOwnershipTimestampQuarantinesWithoutGraphCall(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	queued, dispatching := createMetaInstagramManualOutboxPair(t, fixture)
	metadata := cloneJSONB(fixture.account.Metadata)
	metadata["meta_ownership_checked_at"] = time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339Nano)
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
		Where("id = ?", fixture.account.ID).Update("metadata", metadata).Error)
	var providerCalls atomic.Int32
	fixture.app.HTTPClient = &http.Client{Transport: metaInstagramRoundTripFunc(func(*http.Request) (*http.Response, error) {
		providerCalls.Add(1)
		return nil, errors.New("future ownership timestamp must fail before Graph")
	})}

	fixture.app.revalidateOneMetaInstagramBinding(
		context.Background(), fixture.org.ID, fixture.account.ID, time.Now().UTC(),
	)
	assert.Zero(t, providerCalls.Load())
	assertMetaInstagramQueuedWorkFence(t, fixture, queued, dispatching)
	var account models.ChannelAccount
	require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusDegraded, account.Status)
	assert.Equal(t, "ownership_check_timestamp_in_future", stringConfigValue(account.Metadata, "meta_ownership_reason"))
}

func TestFetchMetaInstagramProfileAcceptsOnlyOfficialProfessionalAccountTypes(t *testing.T) {
	for _, test := range []struct {
		accountType string
		wantAllowed bool
	}{
		{accountType: "BUSINESS", wantAllowed: true},
		{accountType: "MEDIA_CREATOR", wantAllowed: true},
		{accountType: "CREATOR", wantAllowed: false},
		{accountType: "PERSONAL", wantAllowed: false},
	} {
		t.Run(test.accountType, func(t *testing.T) {
			provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				assert.Equal(t, "/v25.0/me", request.URL.Path)
				assert.Equal(t, "Bearer synthetic-profile-token", request.Header.Get("Authorization"))
				assert.Equal(
					t,
					metaInstagramAppSecretProof("synthetic-profile-token", metaInstagramTestAppSecret),
					request.URL.Query().Get("appsecret_proof"),
				)
				_, _ = io.WriteString(w, `{"data":[{"id":"800000000000101","user_id":"700000000000101","username":"synthetic_profile","account_type":"`+test.accountType+`"}]}`)
			}))
			defer provider.Close()
			app := &App{HTTPClient: testutil.NewHTTPSRewriteClient(t, map[string]*httptest.Server{
				"https://graph.instagram.test": provider,
			})}
			profile, err := app.fetchMetaInstagramProfile(
				t.Context(),
				configpkg.MetaInstagramConfig{
					GraphBaseURL: "https://graph.instagram.test", GraphAPIVersion: "v25.0",
					AppSecret: metaInstagramTestAppSecret,
				},
				"synthetic-profile-token",
			)
			if test.wantAllowed {
				require.NoError(t, err)
				assert.Equal(t, test.accountType, profile.AccountType)
				assert.Equal(t, "800000000000101", profile.oauthSubjectID())
				assert.Equal(t, "700000000000101", profile.professionalAccountID())
				return
			}
			var bindingErr *metaInstagramLifecycleBindingError
			require.ErrorAs(t, err, &bindingErr)
			assert.False(t, bindingErr.Revoked)
			assert.Equal(t, "instagram_professional_account_required", bindingErr.Reason)
		})
	}
}

func TestFetchMetaInstagramProfileRequiresCurrentSingleIdentityEnvelope(t *testing.T) {
	valid := `{"id":"800000000000101","user_id":"700000000000101","username":"synthetic_profile","account_type":"BUSINESS"}`
	for _, test := range []struct {
		name     string
		response string
	}{
		{name: "obsolete top-level profile", response: valid},
		{name: "empty data", response: `{"data":[]}`},
		{name: "multiple profiles", response: `{"data":[` + valid + `,` + valid + `]}`},
		{name: "missing OAuth subject", response: `{"data":[{"user_id":"700000000000101","username":"synthetic_profile","account_type":"BUSINESS"}]}`},
		{name: "missing professional ID", response: `{"data":[{"id":"800000000000101","username":"synthetic_profile","account_type":"BUSINESS"}]}`},
		{name: "trailing JSON", response: `{"data":[` + valid + `]} {}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, test.response)
			}))
			defer provider.Close()
			app := &App{HTTPClient: testutil.NewHTTPSRewriteClient(t, map[string]*httptest.Server{
				"https://graph.instagram.test": provider,
			})}
			profile, err := app.fetchMetaInstagramProfile(
				t.Context(), configpkg.MetaInstagramConfig{
					GraphBaseURL: "https://graph.instagram.test", GraphAPIVersion: "v25.0",
					AppSecret: metaInstagramTestAppSecret,
				}, "synthetic-profile-token",
			)
			require.Error(t, err)
			assert.Empty(t, profile.ID)
			assert.Empty(t, profile.UserID)
		})
	}
}

func TestExchangeMetaInstagramAuthorizationCodeRequiresCurrentBusinessLoginEnvelope(t *testing.T) {
	validEntry := `{"access_token":"synthetic-short-token","user_id":"700000000000101","permissions":"instagram_business_manage_messages, instagram_business_basic"}`
	for _, test := range []struct {
		name        string
		response    string
		wantSuccess bool
	}{
		{name: "current one-element envelope", response: `{"data":[` + validEntry + `]}`, wantSuccess: true},
		{name: "empty data", response: `{"data":[]}`},
		{name: "multiple data entries", response: `{"data":[` + validEntry + `,` + validEntry + `]}`},
		{name: "data is not an array", response: `{"data":{}}`},
		{name: "obsolete top-level response", response: validEntry},
		{name: "permissions missing", response: `{"data":[{"access_token":"synthetic-short-token","user_id":"700000000000101"}]}`},
		{name: "required permission missing", response: `{"data":[{"access_token":"synthetic-short-token","user_id":"700000000000101","permissions":"instagram_business_basic"}]}`},
		{name: "trailing JSON value", response: `{"data":[` + validEntry + `]} {}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				assert.Equal(t, http.MethodPost, request.Method)
				assert.Equal(t, "/oauth/access_token", request.URL.Path)
				assert.Equal(t, "application/x-www-form-urlencoded", request.Header.Get("Content-Type"))
				_, _ = io.WriteString(w, test.response)
			}))
			defer provider.Close()
			app := &App{HTTPClient: testutil.NewHTTPSRewriteClient(t, map[string]*httptest.Server{
				"https://api.instagram.test": provider,
			})}
			response, err := app.exchangeMetaInstagramAuthorizationCode(
				t.Context(),
				configpkg.MetaInstagramConfig{
					AppID: "100000000000101", AppSecret: metaInstagramTestAppSecret,
					TokenBaseURL: "https://api.instagram.test",
				},
				"synthetic-code", "https://app.example.test/api/integrations/meta/instagram/callback",
			)
			if !test.wantSuccess {
				require.Error(t, err)
				assert.Empty(t, response.AccessToken)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "synthetic-short-token", response.AccessToken)
			assert.Equal(t, "700000000000101", response.UserID)
			assert.Equal(t, metaInstagramRequiredScopes, response.Permissions)
		})
	}
}

func TestManagedInstagramDeterministicGraphLossImmediatelyDowngradesAndCancels(t *testing.T) {
	tests := []struct {
		name            string
		debugResponse   string
		profileResponse string
		wantStatus      models.ChannelAccountStatus
		wantReason      string
		wantCalls       int32
	}{
		{
			name:          "invalid token binding",
			debugResponse: `{"data":{"app_id":"100000000000101","is_valid":false,"user_id":"700000000000101","scopes":["instagram_business_basic","instagram_business_manage_messages"]}}`,
			wantStatus:    models.ChannelAccountStatusDisconnected,
			wantReason:    "instagram_token_binding_invalid", wantCalls: 1,
		},
		{
			name:          "required scope removed",
			debugResponse: `{"data":{"app_id":"100000000000101","is_valid":true,"user_id":"700000000000101","scopes":["instagram_business_basic"]}}`,
			wantStatus:    models.ChannelAccountStatusDegraded,
			wantReason:    "instagram_required_permissions_missing", wantCalls: 1,
		},
		{
			name:            "professional account type removed",
			debugResponse:   `{"data":{"app_id":"100000000000101","is_valid":true,"user_id":"700000000000101","scopes":["instagram_business_basic","instagram_business_manage_messages"]}}`,
			profileResponse: `{"data":[{"id":"800000000000101","user_id":"700000000000101","username":"synthetic_profile","account_type":"PERSONAL"}]}`,
			wantStatus:      models.ChannelAccountStatusDegraded,
			wantReason:      "instagram_professional_account_required", wantCalls: 2,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newMetaInstagramLifecycleFixture(t)
			queued, dispatching := createMetaInstagramManualOutboxPair(t, fixture)
			fixture.app.Config.MetaInstagram.TokenRefreshLeadHours = 1
			metadata := cloneJSONB(fixture.account.Metadata)
			metadata["meta_ownership_checked_at"] = time.Now().UTC().
				Add(-25 * time.Hour).Format(time.RFC3339Nano)
			require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
				Where("id = ?", fixture.account.ID).Update("metadata", metadata).Error)
			var calls atomic.Int32
			fixture.app.HTTPClient = &http.Client{Transport: metaInstagramRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls.Add(1)
				body := ""
				switch request.URL.Path {
				case "/debug_token":
					body = strings.ReplaceAll(test.debugResponse, "700000000000101", fixture.profileID)
				case "/v25.0/me":
					body = strings.ReplaceAll(test.profileResponse, "700000000000101", fixture.profileID)
				default:
					return nil, errors.New("deterministic loss must stop before subscription renewal")
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(body)),
					Request:    request,
				}, nil
			})}

			fixture.app.revalidateOneMetaInstagramBinding(
				t.Context(), fixture.org.ID, fixture.account.ID, time.Now().UTC(),
			)
			assert.Equal(t, test.wantCalls, calls.Load())
			assertMetaInstagramQueuedWorkFence(t, fixture, queued, dispatching)
			var account models.ChannelAccount
			require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
			assert.Equal(t, test.wantStatus, account.Status)
			assert.Equal(t, test.wantReason, stringConfigValue(account.Metadata, "meta_ownership_reason"))
			assert.False(t, boolConfigValue(account.Config, "outbound_enabled"))
			_, err := fixture.app.scopedApp(fixture.db, fixture.org.ID).loadMetaRegistryBinding(
				metaregistry.ResolveRequest{
					Channel: models.ChannelInstagram, ExternalAccountID: fixture.profileID,
					Purpose: metaregistry.ResolvePurposeOutbound,
				}, time.Now().UTC(),
			)
			require.Error(t, err)
		})
	}
}

func TestManagedInstagramActivationRequiresFreshHealthAndExactCredentialFences(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	healthCheckedAt := now.Add(-time.Minute).Add(789 * time.Nanosecond)
	metadata := cloneJSONB(fixture.account.Metadata)
	metadata["meta_activation_state"] = "awaiting_admin_approval"
	metadata["meta_health_checked_at"] = healthCheckedAt.Format(time.RFC3339Nano)
	metadata["meta_ownership_checked_at"] = healthCheckedAt.Format(time.RFC3339Nano)
	metadata["meta_health_oauth_credential_id"] = fixture.oauth.ID.String()
	metadata["meta_health_oauth_version"] = fixture.oauth.Version
	metadata["meta_health_webhook_credential_id"] = fixture.webhook.ID.String()
	metadata["meta_health_webhook_version"] = fixture.webhook.Version
	accountConfig := cloneJSONB(fixture.account.Config)
	accountConfig["outbound_enabled"] = false
	accountConfig["ai_reply_enabled"] = false
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
		Where("id = ?", fixture.account.ID).Updates(map[string]any{
		"status": models.ChannelAccountStatusPending, "config": accountConfig,
		"metadata": metadata, "last_health_check_at": healthCheckedAt.Add(-time.Microsecond),
		"last_error": "", "connected_at": nil,
	}).Error)
	evidence := metaInstagramSubscriptionApproval{
		ProfileID: fixture.profileID, CheckedAt: now,
		OAuthCredentialID: fixture.oauth.ID, OAuthCredentialVersion: fixture.oauth.Version,
		WebhookCredentialID: fixture.webhook.ID, WebhookCredentialVersion: fixture.webhook.Version,
	}
	_, err := fixture.app.activateMetaInstagramAccount(
		fixture.org.ID, fixture.user.ID, fixture.account.ID, now, evidence,
	)
	require.Error(t, err, "a full-microsecond evidence mismatch must remain fail closed")
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
		Where("id = ?", fixture.account.ID).Update("last_health_check_at", healthCheckedAt).Error)
	var storedHealth models.ChannelAccount
	require.NoError(t, fixture.db.First(&storedHealth, "id = ?", fixture.account.ID).Error)
	require.NotNil(t, storedHealth.LastHealthCheckAt)
	assert.False(t, storedHealth.LastHealthCheckAt.UTC().Equal(healthCheckedAt.UTC()))
	assert.Equal(t, healthCheckedAt.Truncate(time.Microsecond), storedHealth.LastHealthCheckAt.UTC())
	assert.True(t, channelHealthTimestampsMatch(*storedHealth.LastHealthCheckAt, healthCheckedAt))
	activated, err := fixture.app.activateMetaInstagramAccount(
		fixture.org.ID, fixture.user.ID, fixture.account.ID, now, evidence,
	)
	require.NoError(t, err)
	assert.Equal(t, models.ChannelAccountStatusActive, activated.Status)
	assert.True(t, boolConfigValue(activated.Config, "outbound_enabled"))
	assert.False(t, boolConfigValue(activated.Config, "ai_reply_enabled"))

	// Reusing a successful approval is impossible after the pending row has
	// already transitioned, even with otherwise identical evidence.
	_, err = fixture.app.activateMetaInstagramAccount(
		fixture.org.ID, fixture.user.ID, fixture.account.ID, now, evidence,
	)
	require.Error(t, err)
}

func TestManagedInstagramActivationFinalLockRejectsDriftedPlatformBinding(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	metadata := cloneJSONB(fixture.account.Metadata)
	metadata["meta_activation_state"] = "awaiting_admin_approval"
	metadata["meta_health_checked_at"] = now.Add(-time.Minute).Format(time.RFC3339Nano)
	metadata["meta_ownership_checked_at"] = now.Add(-time.Minute).Format(time.RFC3339Nano)
	metadata["meta_health_oauth_credential_id"] = fixture.oauth.ID.String()
	metadata["meta_health_oauth_version"] = fixture.oauth.Version
	metadata["meta_health_webhook_credential_id"] = fixture.webhook.ID.String()
	metadata["meta_health_webhook_version"] = fixture.webhook.Version
	metadata["meta_granted_scopes"] = []string{"instagram_business_basic"}
	accountConfig := cloneJSONB(fixture.account.Config)
	accountConfig["outbound_enabled"] = false
	accountConfig["ai_reply_enabled"] = false
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
		Where("id = ?", fixture.account.ID).Updates(map[string]any{
		"status": models.ChannelAccountStatusPending, "config": accountConfig,
		"metadata": metadata, "last_health_check_at": now.Add(-time.Minute),
		"last_error": "", "connected_at": nil,
	}).Error)
	_, err := fixture.app.activateMetaInstagramAccount(
		fixture.org.ID, fixture.user.ID, fixture.account.ID, now,
		metaInstagramSubscriptionApproval{
			ProfileID: fixture.profileID, CheckedAt: now,
			OAuthCredentialID: fixture.oauth.ID, OAuthCredentialVersion: fixture.oauth.Version,
			WebhookCredentialID:      fixture.webhook.ID,
			WebhookCredentialVersion: fixture.webhook.Version,
		},
	)
	require.Error(t, err)
	var account models.ChannelAccount
	require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusPending, account.Status)
	assert.False(t, boolConfigValue(account.Config, "outbound_enabled"))
}

func TestManagedInstagramActivationRejectsSeededUnsubscribeOperationBeforeGraph(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Redis = testutil.SetupTestRedis(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	metadata := cloneJSONB(fixture.account.Metadata)
	metadata["meta_activation_state"] = "awaiting_admin_approval"
	metadata["meta_health_checked_at"] = now.Add(-time.Minute).Format(time.RFC3339Nano)
	metadata["meta_ownership_checked_at"] = now.Add(-time.Minute).Format(time.RFC3339Nano)
	metadata["meta_health_oauth_credential_id"] = fixture.oauth.ID.String()
	metadata["meta_health_oauth_version"] = fixture.oauth.Version
	metadata["meta_health_webhook_credential_id"] = fixture.webhook.ID.String()
	metadata["meta_health_webhook_version"] = fixture.webhook.Version
	metadata[metaMessengerSubscriptionDesiredStateKey] = metaMessengerSubscriptionDesiredUnsubscribed
	accountConfig := cloneJSONB(fixture.account.Config)
	accountConfig["outbound_enabled"] = false
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
		Where("id = ?", fixture.account.ID).Updates(map[string]any{
		"status": models.ChannelAccountStatusPending, "config": accountConfig,
		"metadata": metadata, "last_health_check_at": now.Add(-time.Minute),
		"last_error": "", "last_error_at": nil, "connected_at": nil,
	}).Error)
	var providerCalls atomic.Int32
	fixture.app.HTTPClient = &http.Client{Transport: metaInstagramRoundTripFunc(func(*http.Request) (*http.Response, error) {
		providerCalls.Add(1)
		return nil, errors.New("seeded unsubscribe operation must fail before Graph")
	})}
	request := testutil.NewJSONRequest(t, map[string]any{"approve": true})
	testutil.SetAuthContext(request, fixture.org.ID, fixture.user.ID)
	testutil.SetHeader(request, "X-Organization-ID", fixture.org.ID.String())
	testutil.SetPathParam(request, "id", fixture.account.ID.String())
	require.NoError(t, fixture.app.ApproveMetaInstagramActivation(request))
	assert.Equal(t, fasthttp.StatusConflict, request.RequestCtx.Response.StatusCode())
	assert.Zero(t, providerCalls.Load())
	var account models.ChannelAccount
	require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusPending, account.Status)
	assert.False(t, boolConfigValue(account.Config, "outbound_enabled"))
}

func TestManagedInstagramApprovalRejectsPartialManagedMarkersBeforeGraph(t *testing.T) {
	for _, removedMarker := range []string{"meta_registry_managed", "meta_management_mode"} {
		t.Run(removedMarker, func(t *testing.T) {
			fixture := newMetaInstagramLifecycleFixture(t)
			fixture.app.Redis = testutil.SetupTestRedis(t)
			now := time.Now().UTC().Truncate(time.Microsecond)
			metadata := cloneJSONB(fixture.account.Metadata)
			metadata["meta_activation_state"] = "awaiting_admin_approval"
			metadata["meta_health_checked_at"] = now.Add(-time.Minute).Format(time.RFC3339Nano)
			metadata["meta_ownership_checked_at"] = now.Add(-time.Minute).Format(time.RFC3339Nano)
			metadata["meta_health_oauth_credential_id"] = fixture.oauth.ID.String()
			metadata["meta_health_oauth_version"] = fixture.oauth.Version
			metadata["meta_health_webhook_credential_id"] = fixture.webhook.ID.String()
			metadata["meta_health_webhook_version"] = fixture.webhook.Version
			config := cloneJSONB(fixture.account.Config)
			delete(config, removedMarker)
			config["outbound_enabled"] = false
			config["ai_reply_enabled"] = false
			require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
				Where("id = ?", fixture.account.ID).Updates(map[string]any{
				"status": models.ChannelAccountStatusPending, "config": config,
				"metadata": metadata, "last_health_check_at": now.Add(-time.Minute),
				"last_error": "", "last_error_at": nil, "connected_at": nil,
			}).Error)
			var providerCalls atomic.Int32
			fixture.app.HTTPClient = &http.Client{Transport: metaInstagramRoundTripFunc(func(*http.Request) (*http.Response, error) {
				providerCalls.Add(1)
				return nil, errors.New("partial managed approval must not reach Graph")
			})}
			request := testutil.NewJSONRequest(t, map[string]any{"approve": true})
			testutil.SetAuthContext(request, fixture.org.ID, fixture.user.ID)
			testutil.SetHeader(request, "X-Organization-ID", fixture.org.ID.String())
			testutil.SetPathParam(request, "id", fixture.account.ID.String())
			require.NoError(t, fixture.app.ApproveMetaInstagramActivation(request))
			assert.Equal(t, fasthttp.StatusConflict, request.RequestCtx.Response.StatusCode())
			assert.Zero(t, providerCalls.Load())
			var account models.ChannelAccount
			require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
			assert.Equal(t, models.ChannelAccountStatusPending, account.Status)
			assert.False(t, boolConfigValue(account.Config, "outbound_enabled"))
		})
	}
}

func TestManagedInstagramReconnectCallbackRejectsPartialManagedMarkersBeforeProvider(t *testing.T) {
	for _, removedMarker := range []string{"meta_registry_managed", "meta_management_mode"} {
		t.Run(removedMarker, func(t *testing.T) {
			fixture := newMetaInstagramLifecycleFixture(t)
			fixture.app.Redis = testutil.SetupTestRedis(t)
			now := time.Now().UTC()
			nonce := "synthetic-partial-reconnect-" + uuid.NewString()
			state := metaInstagramOAuthState{
				ReconnectAccountID: fixture.account.ID.String(),
				OrganizationID:     fixture.org.ID.String(), UserID: fixture.user.ID.String(), Nonce: nonce,
				ConfigFingerprint: fixture.app.metaInstagramRuntimeFingerprint(
					fixture.app.Config.MetaInstagram, fixture.org.ID,
				),
				IssuedAt: now, ExpiresAt: now.Add(metaInstagramOAuthStateTTL),
			}
			browserVerifier := generateRandomString(metaInstagramOAuthBrowserSecretSize)
			state.BrowserBindingDigest = metaInstagramOAuthBrowserBindingDigest(state, browserVerifier)
			payload, err := json.Marshal(state)
			require.NoError(t, err)
			require.NoError(t, fixture.app.Redis.Set(
				t.Context(), metaInstagramOAuthStateKey(nonce), payload, metaInstagramOAuthStateTTL,
			).Err())
			config := cloneJSONB(fixture.account.Config)
			delete(config, removedMarker)
			require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
				Where("id = ?", fixture.account.ID).Update("config", config).Error)
			var providerCalls atomic.Int32
			fixture.app.HTTPClient = &http.Client{Transport: metaInstagramRoundTripFunc(func(*http.Request) (*http.Response, error) {
				providerCalls.Add(1)
				return nil, errors.New("partial reconnect must fail before code exchange")
			})}
			request := testutil.NewRequest(t)
			request.RequestCtx.QueryArgs().Set("state", nonce)
			request.RequestCtx.QueryArgs().Set("code", "synthetic-authorization-code")
			request.RequestCtx.Request.Header.SetCookie(
				metaInstagramOAuthBrowserCookieName(nonce), browserVerifier,
			)
			require.NoError(t, fixture.app.CallbackMetaInstagram(request))
			assert.Equal(t, fasthttp.StatusSeeOther, request.RequestCtx.Response.StatusCode())
			assert.Zero(t, providerCalls.Load())
		})
	}
}

func TestManagedInstagramRefreshKeepsQueueGenerationAndEncryptsReplacement(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	var calls atomic.Int32
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.URL.Query().Get("appsecret_proof") == "" {
			t.Errorf("%s omitted appsecret_proof", request.URL.Path)
		}
		switch request.URL.Path {
		case "/refresh_access_token":
			if request.URL.Query().Get("grant_type") != "ig_refresh_token" ||
				request.URL.Query().Get("access_token") != "synthetic-old-instagram-token" {
				t.Errorf("refresh request was not bound to the current token")
			}
			_, _ = io.WriteString(w, `{"access_token":"synthetic-refreshed-instagram-token","token_type":"bearer","expires_in":5184000}`)
		case "/debug_token":
			if request.URL.Query().Get("input_token") != "synthetic-refreshed-instagram-token" {
				t.Errorf("inspection did not use refreshed token")
			}
			_, _ = io.WriteString(w, `{"data":{"app_id":"100000000000101","is_valid":true,"user_id":"`+fixture.profileID+`","scopes":["instagram_business_basic","instagram_business_manage_messages"],"expires_at":`+strconv.FormatInt(time.Now().UTC().Add(60*24*time.Hour).Unix(), 10)+`}}`)
		case "/v25.0/me":
			_, _ = io.WriteString(w, `{"data":[{"id":"`+fixture.profileID+`","user_id":"`+fixture.profileID+`","username":"synthetic_profile","account_type":"MEDIA_CREATOR"}]}`)
		case "/v25.0/" + fixture.profileID + "/subscribed_apps":
			_, _ = io.WriteString(w, `{"data":[{"id":"100000000000101","subscribed_fields":["messages"]}]}`)
		default:
			http.NotFound(w, request)
		}
	}))
	defer provider.Close()
	fixture.app.HTTPClient = testutil.NewHTTPSRewriteClient(t, map[string]*httptest.Server{
		"https://graph.instagram.test": provider,
	})
	fixture.app.revalidateOneMetaInstagramBinding(
		t.Context(), fixture.org.ID, fixture.account.ID, time.Now().UTC(),
	)
	assert.Equal(t, int32(4), calls.Load())

	var oauth models.ChannelCredential
	require.NoError(t, fixture.db.First(&oauth, "id = ?", fixture.oauth.ID).Error)
	assert.Equal(t, fixture.oauth.ID, oauth.ID)
	assert.Equal(t, fixture.oauth.Version, oauth.Version, "refresh must not change queued generation fences")
	assert.NotNil(t, oauth.RotatedAt)
	assert.NotNil(t, oauth.ExpiresAt)
	assert.True(t, oauth.ExpiresAt.After(time.Now().UTC().Add(50*24*time.Hour)))
	ciphertext, _ := oauth.CredentialBlob["access_token"].(string)
	assert.True(t, appcrypto.IsEncrypted(ciphertext))
	plaintext, err := appcrypto.Decrypt(ciphertext, metaInstagramTestEncryptionKey)
	require.NoError(t, err)
	assert.Equal(t, "synthetic-refreshed-instagram-token", plaintext)
	assert.NotContains(t, ciphertext, plaintext)

	var account models.ChannelAccount
	require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusActive, account.Status)
	assert.Equal(t, metaregistry.OwnershipVerified, stringConfigValue(account.Metadata, "meta_ownership_state"))
}

func TestManagedInstagramDeauthorizationLookupCannotCrossMessengerChannel(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	messenger := fixture.account
	messenger.ID = uuid.New()
	messenger.Channel = models.ChannelMessenger
	messenger.ExternalAccountID = "700000000000202"
	messenger.Name = "Synthetic Messenger Page"
	messenger.Config = cloneJSONB(fixture.account.Config)
	delete(messenger.Config, "instagram_api_mode")
	messenger.Metadata = cloneJSONB(fixture.account.Metadata)
	messenger.Metadata["meta_webhook_app"] = "messenger"
	messenger.Metadata["meta_platform_app_id"] = metaInstagramTestAppID
	messenger.Metadata["meta_authorizing_user_id"] = fixture.profileID
	messenger.Metadata["meta_business_id"] = "200000000000202"
	require.NoError(t, fixture.db.Create(&messenger).Error)

	targets, err := fixture.app.resolveMetaInstagramCallbackTargets(
		metaInstagramTestAppID, fixture.profileID,
	)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	assert.Equal(t, fixture.account.ID, targets[0].AccountID)

	_, err = fixture.app.resolveAllMetaDeauthorizationTargetsForChannel(
		models.ChannelInstagram, metaInstagramTestAppID, fixture.profileID,
	)
	require.Error(t, err, "Instagram callbacks must never use the global Messenger pager")
	_, err = fixture.app.resolveAllMetaDeauthorizationTargetsForChannel(
		models.ChannelThreads, metaInstagramTestAppID, fixture.profileID,
	)
	require.Error(t, err)
}

func TestManagedInstagramDeauthorizationIgnoresForeignCandidateAndRevokesPilot(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Redis = testutil.SetupTestRedis(t)
	queued, dispatching := createMetaInstagramManualOutboxPair(t, fixture)
	foreignOrg := testutil.CreateTestOrganization(t, fixture.db)
	foreignUser := testutil.CreateTestUser(t, fixture.db, foreignOrg.ID)
	foreign := fixture.account
	foreign.ID = uuid.New()
	foreign.OrganizationID = foreignOrg.ID
	foreign.ExternalAccountID = "9" + fixture.profileID
	foreign.Name = "Synthetic Foreign Instagram Profile"
	foreign.CreatedByID = &foreignUser.ID
	foreign.UpdatedByID = &foreignUser.ID
	require.NoError(t, fixture.db.Create(&foreign).Error)

	issuedAt := time.Now().UTC().Truncate(time.Second)
	metadata := cloneJSONB(fixture.account.Metadata)
	metadata[metaMessengerAuthorizationGrantedAtKey] = issuedAt.Add(-time.Hour).Format(time.RFC3339Nano)
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
		Where("id IN ?", []uuid.UUID{fixture.account.ID, foreign.ID}).
		Update("metadata", metadata).Error)
	require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
		Where("channel_account_id = ?", fixture.account.ID).
		UpdateColumn("created_at", issuedAt.Add(-time.Hour)).Error)
	request := newMetaInstagramDeauthorizationRequest(t, signMetaInstagramLifecycleRequest(
		t, issuedAt, fixture.profileID, metaInstagramTestAppSecret,
	))
	require.NoError(t, fixture.app.DeauthorizeMetaInstagram(request))
	assert.Equal(t, fasthttp.StatusOK, request.RequestCtx.Response.StatusCode())
	var allowedAfter models.ChannelAccount
	require.NoError(t, fixture.db.First(&allowedAfter, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusDisconnected, allowedAfter.Status)
	assert.False(t, boolConfigValue(allowedAfter.Config, "outbound_enabled"))
	assertMetaInstagramQueuedWorkFence(t, fixture, queued, dispatching)
	var foreignAfter models.ChannelAccount
	require.NoError(t, fixture.db.First(&foreignAfter, "id = ?", foreign.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusActive, foreignAfter.Status)
	assert.True(t, boolConfigValue(foreignAfter.Config, "outbound_enabled"))
	var journal models.MetaDeauthorizationEvent
	require.NoError(t, fixture.db.Where(
		"platform_app_id = ? AND authorizing_user_id = ?",
		metaInstagramTestAppID, fixture.profileID,
	).First(&journal).Error)
	assert.Equal(t, "completed", journal.State)
}

func TestManagedInstagramCallbacksIgnoreForeignCardinalityBeforeTenantMutation(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(*testing.T, metaInstagramLifecycleFixture, string)
	}{
		{
			name: "deauthorization",
			run: func(t *testing.T, fixture metaInstagramLifecycleFixture, signedRequest string) {
				queued, dispatching := createMetaInstagramManualOutboxPair(t, fixture)
				request := newMetaInstagramDeauthorizationRequest(t, signedRequest)
				require.NoError(t, fixture.app.DeauthorizeMetaInstagram(request))
				assert.Equal(t, fasthttp.StatusOK, request.RequestCtx.Response.StatusCode())
				assertMetaInstagramQueuedWorkFence(t, fixture, queued, dispatching)
				var journal models.MetaDeauthorizationEvent
				require.NoError(t, fixture.db.Where(
					"platform_app_id = ? AND authorizing_user_id = ?",
					metaInstagramTestAppID, fixture.profileID,
				).First(&journal).Error)
				assert.Equal(t, "completed", journal.State)
			},
		},
		{
			name: "data deletion",
			run: func(t *testing.T, fixture metaInstagramLifecycleFixture, signedRequest string) {
				queued, dispatching := createMetaInstagramManualOutboxPair(t, fixture)
				request := newMetaInstagramDeletionRequest(t, signedRequest)
				require.NoError(t, fixture.app.DeleteMetaInstagramUserData(request))
				assert.Equal(t, fasthttp.StatusOK, request.RequestCtx.Response.StatusCode())
				assertMetaInstagramQueuedWorkFence(t, fixture, queued, dispatching)
				var journal models.MetaInstagramDataDeletionEvent
				require.NoError(t, fixture.db.Where(
					"platform_app_id = ? AND authorizing_user_id = ?",
					metaInstagramTestAppID, fixture.profileID,
				).First(&journal).Error)
				assert.Equal(t, "completed", journal.State)
				var privacyCount int64
				require.NoError(t, fixture.db.Model(&models.PrivacyRequest{}).
					Where("organization_id = ? AND received_channel = ?",
						fixture.org.ID, metaInstagramDeletionReceivedChannel).
					Count(&privacyCount).Error)
				assert.Equal(t, int64(1), privacyCount)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newMetaInstagramLifecycleFixture(t)
			fixture.app.Redis = testutil.SetupTestRedis(t)
			foreignOrganizationID := seedForeignMetaInstagramCallbackCandidates(
				t, fixture, metaDeauthorizationMaxTargets+1,
			)
			issuedAt := time.Now().UTC().Truncate(time.Second)
			metadata := cloneJSONB(fixture.account.Metadata)
			metadata[metaMessengerAuthorizationGrantedAtKey] = issuedAt.
				Add(-time.Hour).Format(time.RFC3339Nano)
			require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
				Where("id = ?", fixture.account.ID).Update("metadata", metadata).Error)
			require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
				Where("channel_account_id = ?", fixture.account.ID).
				UpdateColumn("created_at", issuedAt.Add(-time.Hour)).Error)

			test.run(t, fixture, signMetaInstagramLifecycleRequest(
				t, issuedAt, fixture.profileID, metaInstagramTestAppSecret,
			))

			var account models.ChannelAccount
			require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
			assert.Equal(t, models.ChannelAccountStatusDisconnected, account.Status)
			var foreignActive int64
			require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
				Where("organization_id = ? AND status = ?",
					foreignOrganizationID, models.ChannelAccountStatusActive).
				Count(&foreignActive).Error)
			assert.Equal(t, int64(metaDeauthorizationMaxTargets+1), foreignActive)
		})
	}
}

func TestManagedInstagramDeauthorizationReplayCompletesAfterJournalFailure(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Redis = testutil.SetupTestRedis(t)
	queued, dispatching := createMetaInstagramManualOutboxPair(t, fixture)
	issuedAt := time.Now().UTC().Truncate(time.Second)
	metadata := cloneJSONB(fixture.account.Metadata)
	metadata[metaMessengerAuthorizationGrantedAtKey] = issuedAt.
		Add(-time.Hour).Format(time.RFC3339Nano)
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
		Where("id = ?", fixture.account.ID).Update("metadata", metadata).Error)
	require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
		Where("channel_account_id = ?", fixture.account.ID).
		UpdateColumn("created_at", issuedAt.Add(-time.Hour)).Error)

	require.NoError(t, fixture.db.Exec(`
		CREATE OR REPLACE FUNCTION test_fail_meta_instagram_deauth_completion()
		RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF NEW.state = 'completed' THEN
				RAISE EXCEPTION 'synthetic deauthorization completion failure';
			END IF;
			RETURN NEW;
		END;
		$$;
		DROP TRIGGER IF EXISTS test_fail_meta_instagram_deauth_completion
			ON meta_deauthorization_events;
		CREATE TRIGGER test_fail_meta_instagram_deauth_completion
			BEFORE UPDATE ON meta_deauthorization_events
			FOR EACH ROW EXECUTE FUNCTION test_fail_meta_instagram_deauth_completion();
	`).Error)
	t.Cleanup(func() {
		_ = fixture.db.Exec(`
			DROP TRIGGER IF EXISTS test_fail_meta_instagram_deauth_completion
				ON meta_deauthorization_events;
			DROP FUNCTION IF EXISTS test_fail_meta_instagram_deauth_completion();
		`).Error
	})
	signedRequest := signMetaInstagramLifecycleRequest(
		t, issuedAt, fixture.profileID, metaInstagramTestAppSecret,
	)
	first := newMetaInstagramDeauthorizationRequest(t, signedRequest)
	require.NoError(t, fixture.app.DeauthorizeMetaInstagram(first))
	assert.Equal(t, fasthttp.StatusServiceUnavailable, first.RequestCtx.Response.StatusCode())
	var account models.ChannelAccount
	require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusDisconnected, account.Status)
	assertMetaInstagramQueuedWorkFence(t, fixture, queued, dispatching)
	var journal models.MetaDeauthorizationEvent
	require.NoError(t, fixture.db.Where(
		"platform_app_id = ? AND authorizing_user_id = ?",
		metaInstagramTestAppID, fixture.profileID,
	).First(&journal).Error)
	assert.Equal(t, "verified", journal.State)
	assert.Nil(t, journal.CompletedAt)

	require.NoError(t, fixture.db.Exec(`
		DROP TRIGGER test_fail_meta_instagram_deauth_completion
			ON meta_deauthorization_events;
		DROP FUNCTION test_fail_meta_instagram_deauth_completion();
	`).Error)
	replay := newMetaInstagramDeauthorizationRequest(t, signedRequest)
	require.NoError(t, fixture.app.DeauthorizeMetaInstagram(replay))
	assert.Equal(t, fasthttp.StatusOK, replay.RequestCtx.Response.StatusCode())
	require.NoError(t, fixture.db.Where("digest = ?", journal.Digest).First(&journal).Error)
	assert.Equal(t, "completed", journal.State)
	require.NotNil(t, journal.CompletedAt)
	require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusDisconnected, account.Status)
}

func seedForeignMetaInstagramCallbackCandidates(
	t *testing.T,
	fixture metaInstagramLifecycleFixture,
	count int,
) uuid.UUID {
	t.Helper()
	foreignOrganization := testutil.CreateTestOrganization(t, fixture.db)
	foreignUser := testutil.CreateTestUser(t, fixture.db, foreignOrganization.ID)
	accounts := make([]models.ChannelAccount, count)
	batchBase := int64(780000000000000) +
		int64(metaInstagramTestProfileSequence.Add(1))*int64(metaDeauthorizationMaxTargets+10)
	for index := range accounts {
		externalID := strconv.FormatInt(batchBase+int64(index), 10)
		accounts[index] = models.ChannelAccount{
			BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: foreignOrganization.ID,
			Channel: models.ChannelInstagram, Provider: channelapi.RelayProvider,
			Name: "Synthetic foreign callback candidate " + strconv.Itoa(index), ExternalAccountID: externalID,
			Status: models.ChannelAccountStatusActive,
			Config: models.JSONB{
				"meta_registry_managed": true,
				"meta_management_mode":  metaregistry.ManagementModePlatformOAuth,
				"instagram_api_mode":    "instagram_login",
			},
			Metadata: models.JSONB{
				"meta_platform_app_id":          metaInstagramTestAppID,
				"meta_webhook_app":              "instagram_login",
				"meta_authorizing_user_id":      fixture.profileID,
				metaInstagramOAuthSubjectIDKey:  fixture.profileID,
				"meta_authority_asset_id":       externalID,
				"meta_ownership_state":          metaregistry.OwnershipVerified,
				"meta_ownership_checked_at":     time.Now().UTC().Format(time.RFC3339Nano),
				"meta_authorization_token_kind": metaMessengerTokenKindUser,
			},
			Capabilities: models.JSONB{}, CreatedByID: &foreignUser.ID, UpdatedByID: &foreignUser.ID,
		}
	}
	require.NoError(t, fixture.db.CreateInBatches(&accounts, 200).Error)
	return foreignOrganization.ID
}

func TestManagedInstagramDeauthorizationRechecksChannelBindingBeforeMutation(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	// Model a row being changed after the global pager returned its ID but
	// before the tenant mutation acquired its lock. The Instagram callback may
	// not follow that ID across the channel boundary.
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
		Where("id = ?", fixture.account.ID).
		Update("channel", models.ChannelMessenger).Error)

	changed, err := fixture.app.revokeMetaDeauthorizationTargetForBinding(
		metaDeauthorizationTarget{OrganizationID: fixture.org.ID, AccountID: fixture.account.ID},
		models.ChannelInstagram, metaInstagramTestAppID, fixture.profileID,
		time.Now().UTC(), strings.Repeat("d", sha256.Size*2), time.Now().UTC(),
	)
	assert.False(t, changed)
	require.ErrorIs(t, err, metaregistry.ErrNotFound)

	var account models.ChannelAccount
	require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusActive, account.Status)
	var oauth models.ChannelCredential
	require.NoError(t, fixture.db.First(&oauth, "id = ?", fixture.oauth.ID).Error)
	assert.Equal(t, models.ChannelCredentialStatusActive, oauth.Status)
}

func TestManagedInstagramAmbiguousDeauthorizationIsNonRoutableUntilExactLifecycleResolution(
	t *testing.T,
) {
	fixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Redis = testutil.SetupTestRedis(t)
	queued, dispatching := createMetaInstagramManualOutboxPair(t, fixture)
	eventIssuedAt := time.Now().UTC().Add(-5 * time.Second).Truncate(time.Second)
	metadata := cloneJSONB(fixture.account.Metadata)
	metadata[metaMessengerAuthorizationGrantedAtKey] = eventIssuedAt.Format(time.RFC3339Nano)
	metadata["meta_ownership_checked_at"] = eventIssuedAt.Add(-time.Minute).Format(time.RFC3339Nano)
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
		Where("id = ?", fixture.account.ID).Update("metadata", metadata).Error)
	require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
		Where("id = ?", fixture.oauth.ID).UpdateColumn("created_at", eventIssuedAt).Error)

	signedRequest := signMetaInstagramLifecycleRequest(
		t, eventIssuedAt, fixture.profileID, metaInstagramTestAppSecret,
	)
	request := newMetaInstagramDeauthorizationRequest(t, signedRequest)
	require.NoError(t, fixture.app.DeauthorizeMetaInstagram(request))
	assert.Equal(t, fasthttp.StatusServiceUnavailable, request.RequestCtx.Response.StatusCode())
	assertMetaInstagramQueuedWorkFence(t, fixture, queued, dispatching)

	var account models.ChannelAccount
	require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusDegraded, account.Status)
	assert.True(t, metaDeauthorizationReconciliationPending(account.Metadata))
	assert.Equal(
		t, "deauthorization_reconciliation_required",
		fixture.app.metaInstagramReleaseGuardReason(account, fixture.org.ID),
	)
	_, err := fixture.app.scopedApp(fixture.db, fixture.org.ID).loadMetaRegistryBinding(
		metaregistry.ResolveRequest{
			Channel: models.ChannelInstagram, ExternalAccountID: fixture.profileID,
			Purpose: metaregistry.ResolvePurposeHealth,
		}, time.Now().UTC(),
	)
	require.ErrorIs(t, err, metaregistry.ErrStaleBinding)
	_, err = fixture.app.loadMetaInstagramRevalidationSnapshot(
		fixture.org.ID, fixture.account.ID, time.Now().UTC(),
	)
	require.Error(t, err, "normal activation snapshot must reject the pending deauthorization fence")

	// Even a seeded pending/healthy shape cannot be activated while the durable
	// deauthorization fence is unresolved.
	healthAt := time.Now().UTC().Add(-time.Minute)
	metadata = cloneJSONB(account.Metadata)
	metadata["meta_activation_state"] = "awaiting_admin_approval"
	metadata["meta_health_checked_at"] = healthAt.Format(time.RFC3339Nano)
	metadata["meta_health_oauth_credential_id"] = fixture.oauth.ID.String()
	metadata["meta_health_oauth_version"] = fixture.oauth.Version
	metadata["meta_health_webhook_credential_id"] = fixture.webhook.ID.String()
	metadata["meta_health_webhook_version"] = fixture.webhook.Version
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
		Where("id = ?", fixture.account.ID).Updates(map[string]any{
		"status": models.ChannelAccountStatusPending, "metadata": metadata,
		"last_health_check_at": healthAt, "last_error": "", "last_error_at": nil,
	}).Error)
	_, err = fixture.app.activateMetaInstagramAccount(
		fixture.org.ID, fixture.user.ID, fixture.account.ID, time.Now().UTC(),
		metaInstagramSubscriptionApproval{
			ProfileID: fixture.profileID, CheckedAt: time.Now().UTC(),
			OAuthCredentialID: fixture.oauth.ID, OAuthCredentialVersion: fixture.oauth.Version,
			WebhookCredentialID: fixture.webhook.ID, WebhookCredentialVersion: fixture.webhook.Version,
		},
	)
	require.Error(t, err)

	var providerCalls atomic.Int32
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		providerCalls.Add(1)
		if request.URL.Query().Get("appsecret_proof") == "" {
			t.Errorf("%s omitted appsecret_proof", request.URL.Path)
		}
		switch request.URL.Path {
		case "/debug_token":
			_, _ = io.WriteString(w, `{"data":{"app_id":"100000000000101","is_valid":true,"user_id":"`+fixture.profileID+`","scopes":["instagram_business_basic","instagram_business_manage_messages"],"expires_at":`+strconv.FormatInt(time.Now().UTC().Add(30*24*time.Hour).Unix(), 10)+`}}`)
		case "/v25.0/me":
			_, _ = io.WriteString(w, `{"data":[{"id":"`+fixture.profileID+`","user_id":"`+fixture.profileID+`","username":"synthetic_profile","account_type":"BUSINESS"}]}`)
		case "/v25.0/" + fixture.profileID + "/subscribed_apps":
			_, _ = io.WriteString(w, `{"data":[{"id":"100000000000101","subscribed_fields":["messages"]}]}`)
		default:
			http.NotFound(w, request)
		}
	}))
	defer provider.Close()
	fixture.app.Config.MetaInstagram.TokenRefreshLeadHours = 1
	fixture.app.HTTPClient = testutil.NewHTTPSRewriteClient(t, map[string]*httptest.Server{
		"https://graph.instagram.test": provider,
	})
	fixture.app.revalidateOneMetaInstagramBinding(
		t.Context(), fixture.org.ID, fixture.account.ID, time.Now().UTC(),
	)
	assert.Equal(t, int32(3), providerCalls.Load())
	require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusPending, account.Status)
	assert.False(t, metaDeauthorizationReconciliationPending(account.Metadata))
	assert.Equal(t, "current_authorization_verified", stringConfigValue(account.Metadata, metaDeauthorizationResolvedStateKey))
	assert.Equal(t, "awaiting_health", stringConfigValue(account.Metadata, "meta_activation_state"))
	assert.Nil(t, account.LastHealthCheckAt)

	replay := newMetaInstagramDeauthorizationRequest(t, signedRequest)
	require.NoError(t, fixture.app.DeauthorizeMetaInstagram(replay))
	assert.Equal(t, fasthttp.StatusOK, replay.RequestCtx.Response.StatusCode())
	var journal models.MetaDeauthorizationEvent
	require.NoError(t, fixture.db.Where(
		"platform_app_id = ? AND authorizing_user_id = ?",
		metaInstagramTestAppID, fixture.profileID,
	).First(&journal).Error)
	assert.Equal(t, "completed", journal.State)
	var oauth models.ChannelCredential
	require.NoError(t, fixture.db.First(&oauth, "id = ?", fixture.oauth.ID).Error)
	assert.Equal(t, models.ChannelCredentialStatusActive, oauth.Status)
}

func TestManagedInstagramDeauthorizationQuarantinesMissingAndMixedGenerationFences(t *testing.T) {
	for _, test := range []struct {
		name       string
		wantReason string
		prepare    func(*testing.T, metaInstagramLifecycleFixture, time.Time)
	}{
		{
			name: "missing authorization timestamp", wantReason: "deauthorization_generation_fence_missing",
			prepare: func(t *testing.T, fixture metaInstagramLifecycleFixture, _ time.Time) {
				metadata := cloneJSONB(fixture.account.Metadata)
				delete(metadata, metaMessengerAuthorizationGrantedAtKey)
				require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
					Where("id = ?", fixture.account.ID).Update("metadata", metadata).Error)
			},
		},
		{
			name: "event between authorization and credential", wantReason: "deauthorization_generation_ambiguous",
			prepare: func(t *testing.T, fixture metaInstagramLifecycleFixture, eventAt time.Time) {
				metadata := cloneJSONB(fixture.account.Metadata)
				metadata[metaMessengerAuthorizationGrantedAtKey] = eventAt.Add(-time.Minute).Format(time.RFC3339Nano)
				require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
					Where("id = ?", fixture.account.ID).Update("metadata", metadata).Error)
				require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
					Where("id = ?", fixture.oauth.ID).UpdateColumn("created_at", eventAt.Add(time.Minute)).Error)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newMetaInstagramLifecycleFixture(t)
			fixture.app.Redis = testutil.SetupTestRedis(t)
			queued, dispatching := createMetaInstagramManualOutboxPair(t, fixture)
			eventAt := time.Now().UTC().Add(-5 * time.Second).Truncate(time.Second)
			test.prepare(t, fixture, eventAt)
			request := newMetaInstagramDeauthorizationRequest(t, signMetaInstagramLifecycleRequest(
				t, eventAt, fixture.profileID, metaInstagramTestAppSecret,
			))
			require.NoError(t, fixture.app.DeauthorizeMetaInstagram(request))
			assert.Equal(t, fasthttp.StatusServiceUnavailable, request.RequestCtx.Response.StatusCode())
			assertMetaInstagramQueuedWorkFence(t, fixture, queued, dispatching)
			var account models.ChannelAccount
			require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
			assert.Equal(t, models.ChannelAccountStatusDegraded, account.Status)
			assert.True(t, metaDeauthorizationReconciliationPending(account.Metadata))
			assert.Equal(t, test.wantReason, stringConfigValue(account.Metadata, "meta_ownership_reason"))
			_, err := fixture.app.scopedApp(fixture.db, fixture.org.ID).loadMetaRegistryBinding(
				metaregistry.ResolveRequest{
					Channel: models.ChannelInstagram, ExternalAccountID: fixture.profileID,
					Purpose: metaregistry.ResolvePurposeOutbound,
				}, time.Now().UTC(),
			)
			require.Error(t, err)
		})
	}
}

func TestManagedInstagramRevokedDeauthorizationCancelsUnsentManualOutbox(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	queued, dispatching := createMetaInstagramManualOutboxPair(t, fixture)
	authorizedAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	metadata := cloneJSONB(fixture.account.Metadata)
	metadata[metaMessengerAuthorizationGrantedAtKey] = authorizedAt.Format(time.RFC3339Nano)
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
		Where("id = ?", fixture.account.ID).Update("metadata", metadata).Error)
	require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
		Where("id = ?", fixture.oauth.ID).UpdateColumn("created_at", authorizedAt).Error)
	changed, err := fixture.app.revokeMetaDeauthorizationTargetForBinding(
		metaDeauthorizationTarget{OrganizationID: fixture.org.ID, AccountID: fixture.account.ID},
		models.ChannelInstagram, metaInstagramTestAppID, fixture.profileID,
		time.Now().UTC().Truncate(time.Second), strings.Repeat("b", sha256.Size*2), time.Now().UTC(),
	)
	require.NoError(t, err)
	assert.True(t, changed)
	assertMetaInstagramQueuedWorkFence(t, fixture, queued, dispatching)
	var account models.ChannelAccount
	require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusDisconnected, account.Status)
}

func TestManagedInstagramOutboundLeaseRequiresVerifiedSubscriptionFence(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	metadata := cloneJSONB(fixture.account.Metadata)
	metadata[metaMessengerSubscriptionRemoteStateKey] = metaMessengerSubscriptionRemoteUnknown
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
		Where("id = ?", fixture.account.ID).Update("metadata", metadata).Error)

	_, err := fixture.app.scopedApp(fixture.db, fixture.org.ID).loadMetaRegistryBinding(
		metaregistry.ResolveRequest{
			Channel: models.ChannelInstagram, ExternalAccountID: fixture.profileID,
			Purpose: metaregistry.ResolvePurposeOutbound,
		}, time.Now().UTC(),
	)
	require.ErrorIs(t, err, metaregistry.ErrStaleBinding)
}

func TestManagedInstagramSeededQuarantineStillCancelsQueuedAI(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	queued, dispatching := createMetaInstagramManualOutboxPair(t, fixture)
	reason := "managed_release_evidence_invalid"
	metadata := cloneJSONB(fixture.account.Metadata)
	metadata["meta_ownership_reason"] = reason
	metadata["meta_ownership_state"] = metaregistry.OwnershipStale
	metadata["meta_activation_state"] = "quarantined"
	accountConfig := cloneJSONB(fixture.account.Config)
	accountConfig["outbound_enabled"] = false
	accountConfig["ai_reply_enabled"] = false
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
		Where("id = ?", fixture.account.ID).
		Updates(map[string]any{
			"status":   models.ChannelAccountStatusDegraded,
			"metadata": metadata, "config": accountConfig,
		}).Error)
	messageID := uuid.New()
	job := models.ScheduledJob{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: fixture.org.ID,
		Kind: models.ScheduledJobKindChannelAIReply, AggregateType: models.ChannelAIReplyAggregateType,
		AggregateID: &messageID, RunAt: time.Now().UTC(), Status: models.ScheduledJobStatusPending,
		MaxAttempts: 5, IdempotencyKey: models.ChannelAIReplyIdempotencyKey(messageID), Version: 1,
		Payload: models.JSONB{"channel_account_id": fixture.account.ID.String()},
	}
	require.NoError(t, fixture.db.Create(&job).Error)

	require.NoError(t, fixture.app.quarantineMetaInstagramAccount(
		fixture.org.ID, fixture.account.ID, reason, time.Now().UTC(),
	))
	require.NoError(t, fixture.db.First(&job, "id = ?", job.ID).Error)
	assert.Equal(t, models.ScheduledJobStatusCancelled, job.Status)
	assertMetaInstagramQueuedWorkFence(t, fixture, queued, dispatching)
}

func TestManagedInstagramDataDeletionHasSeparateIdempotentJournalAndStatus(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	queued, dispatching := createMetaInstagramManualOutboxPair(t, fixture)
	fixture.app.Redis = testutil.SetupTestRedis(t)
	issuedAt := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
		Where("id = ?", fixture.oauth.ID).
		UpdateColumn("created_at", issuedAt.Add(-time.Hour)).Error)

	// A Messenger row can carry the same app/user strings in seeded data. The
	// Instagram deletion path must never select or mutate it.
	messenger := fixture.account
	messenger.ID = uuid.New()
	messenger.Channel = models.ChannelMessenger
	messenger.ExternalAccountID = "700000000000303"
	messenger.Name = "Synthetic Messenger Boundary"
	messenger.Config = cloneJSONB(fixture.account.Config)
	delete(messenger.Config, "instagram_api_mode")
	messenger.Metadata = cloneJSONB(fixture.account.Metadata)
	messenger.Metadata["meta_webhook_app"] = "messenger"
	messenger.Metadata["meta_business_id"] = "200000000000303"
	require.NoError(t, fixture.db.Create(&messenger).Error)

	signedRequest := signMetaInstagramLifecycleRequest(
		t, issuedAt, fixture.profileID, metaInstagramTestAppSecret,
	)
	first := newMetaInstagramDeletionRequest(t, signedRequest)
	require.NoError(t, fixture.app.DeleteMetaInstagramUserData(first))
	assert.Equal(t, fasthttp.StatusOK, first.RequestCtx.Response.StatusCode())
	var firstResponse metaInstagramDeletionResponse
	require.NoError(t, json.Unmarshal(first.RequestCtx.Response.Body(), &firstResponse))
	assert.True(t, validMetaInstagramDeletionConfirmationCode(firstResponse.ConfirmationCode))
	assert.Contains(t, firstResponse.URL, "/api/integrations/meta/instagram/data-deletion/status/")

	var privacyRequests int64
	require.NoError(t, fixture.db.Model(&models.PrivacyRequest{}).
		Where("organization_id = ? AND received_channel = ?", fixture.org.ID, metaInstagramDeletionReceivedChannel).
		Count(&privacyRequests).Error)
	assert.Equal(t, int64(1), privacyRequests)
	var deletionJournal models.MetaInstagramDataDeletionEvent
	require.NoError(t, fixture.db.Where(
		"platform_app_id = ? AND authorizing_user_id = ?",
		metaInstagramTestAppID, fixture.profileID,
	).First(&deletionJournal).Error)
	assert.Equal(t, "completed", deletionJournal.State)
	assert.Equal(t, firstResponse.ConfirmationCode, deletionJournal.RequestNumber)
	var deauthorizationEvents int64
	require.NoError(t, fixture.db.Model(&models.MetaDeauthorizationEvent{}).
		Where("platform_app_id = ? AND authorizing_user_id = ?", metaInstagramTestAppID, fixture.profileID).
		Count(&deauthorizationEvents).Error)
	assert.Zero(t, deauthorizationEvents, "data deletion must not use the deauthorization journal")

	var instagram models.ChannelAccount
	require.NoError(t, fixture.db.First(&instagram, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusDisconnected, instagram.Status)
	assert.Equal(t, "data_deletion_requested", stringConfigValue(instagram.Metadata, "meta_activation_state"))
	assert.False(t, boolConfigValue(instagram.Config, "outbound_enabled"))
	assertMetaInstagramQueuedWorkFence(t, fixture, queued, dispatching)
	var messengerAfter models.ChannelAccount
	require.NoError(t, fixture.db.First(&messengerAfter, "id = ?", messenger.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusActive, messengerAfter.Status)
	assert.True(t, boolConfigValue(messengerAfter.Config, "outbound_enabled"))

	retry := newMetaInstagramDeletionRequest(t, signedRequest)
	require.NoError(t, fixture.app.DeleteMetaInstagramUserData(retry))
	var retryResponse metaInstagramDeletionResponse
	require.NoError(t, json.Unmarshal(retry.RequestCtx.Response.Body(), &retryResponse))
	assert.Equal(t, firstResponse, retryResponse)
	require.NoError(t, fixture.db.Model(&models.PrivacyRequest{}).
		Where("organization_id = ? AND received_channel = ?", fixture.org.ID, metaInstagramDeletionReceivedChannel).
		Count(&privacyRequests).Error)
	assert.Equal(t, int64(1), privacyRequests)

	status := testutil.NewRequest(t)
	status.RequestCtx.SetUserValue("confirmation_code", firstResponse.ConfirmationCode)
	require.NoError(t, fixture.app.MetaInstagramDataDeletionStatus(status))
	assert.Equal(t, fasthttp.StatusOK, status.RequestCtx.Response.StatusCode())
	body := string(status.RequestCtx.Response.Body())
	assert.Contains(t, body, "Processing")
	assert.NotContains(t, body, fixture.profileID)
	assert.NotContains(t, body, fixture.org.ID.String())
}

func TestManagedInstagramDataDeletionJournalSerializesPrivacyRequestCreation(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	digest := strings.Repeat("d", sha256.Size*2)
	issuedAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	require.NoError(t, fixture.db.Create(&models.MetaInstagramDataDeletionEvent{
		Digest: digest, OrganizationID: fixture.org.ID, PlatformAppID: metaInstagramTestAppID,
		AuthorizingUserID: fixture.profileID,
		IssuedAt:          issuedAt, VerifiedAt: time.Now().UTC(), State: "verified",
	}).Error)

	firstTx := fixture.db.Begin()
	require.NoError(t, firstTx.Error)
	t.Cleanup(func() { _ = firstTx.Rollback().Error })
	firstScoped := fixture.app.scopedApp(firstTx, fixture.org.ID)
	first, err := firstScoped.createOrResumeMetaInstagramDeletionRequest(
		fixture.org.ID, metaInstagramTestAppID, fixture.profileID,
		fixture.profileID, digest, metaInstagramDeletionResolutionExact, false,
		issuedAt, time.Now().UTC(),
	)
	require.NoError(t, err)

	secondPID := make(chan int, 1)
	secondResult := make(chan models.PrivacyRequest, 1)
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- fixture.db.Connection(func(connection *gorm.DB) error {
			session := connection.Session(&gorm.Session{NewDB: true})
			var backendPID int
			if err := session.Raw("SELECT pg_backend_pid()").Scan(&backendPID).Error; err != nil {
				secondPID <- 0
				return err
			}
			secondPID <- backendPID
			return session.Transaction(func(tx *gorm.DB) error {
				scoped := fixture.app.scopedApp(tx, fixture.org.ID)
				request, createErr := scoped.createOrResumeMetaInstagramDeletionRequest(
					fixture.org.ID, metaInstagramTestAppID, fixture.profileID,
					fixture.profileID, digest, metaInstagramDeletionResolutionExact, false,
					issuedAt, time.Now().UTC(),
				)
				if createErr == nil {
					secondResult <- request
				}
				return createErr
			})
		})
	}()
	backendPID := <-secondPID
	require.Positive(t, backendPID)
	testutil.RequirePostgresBackendWaitingForLock(t, fixture.db, backendPID)
	require.NoError(t, firstTx.Commit().Error)
	select {
	case err := <-secondDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "second deletion callback did not resume after the durable lock committed")
	}
	second := <-secondResult
	assert.Equal(t, first.ID, second.ID)
	assert.Equal(t, first.RequestNumber, second.RequestNumber)
	var count int64
	require.NoError(t, fixture.db.Model(&models.PrivacyRequest{}).
		Where("organization_id = ? AND verification_token_hash = ?",
			fixture.org.ID,
			metaInstagramDeletionEventHash(metaInstagramTestAppID, fixture.profileID, digest),
		).Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

func TestManagedInstagramDataDeletionRejectsWrongSecretBeforeWrites(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Redis = testutil.SetupTestRedis(t)
	request := newMetaInstagramDeletionRequest(t, signMetaInstagramLifecycleRequest(
		t, time.Now().UTC(), fixture.profileID,
		"different-synthetic-instagram-secret-at-least-32-bytes",
	))
	require.NoError(t, fixture.app.DeleteMetaInstagramUserData(request))
	assert.Equal(t, fasthttp.StatusBadRequest, request.RequestCtx.Response.StatusCode())
	var journalCount, privacyCount int64
	require.NoError(t, fixture.db.Model(&models.MetaInstagramDataDeletionEvent{}).
		Where("platform_app_id = ? AND authorizing_user_id = ?", metaInstagramTestAppID, fixture.profileID).
		Count(&journalCount).Error)
	require.NoError(t, fixture.db.Model(&models.PrivacyRequest{}).
		Where("organization_id = ? AND received_channel = ?", fixture.org.ID, metaInstagramDeletionReceivedChannel).
		Count(&privacyCount).Error)
	assert.Zero(t, journalCount)
	assert.Zero(t, privacyCount)
	var account models.ChannelAccount
	require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusActive, account.Status)
}

func TestManagedInstagramDelayedDataDeletionKeepsNewAuthorizationGeneration(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Redis = testutil.SetupTestRedis(t)
	issuedAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	newAuthorizationAt := issuedAt.Add(30 * time.Second)
	metadata := cloneJSONB(fixture.account.Metadata)
	metadata[metaMessengerAuthorizationGrantedAtKey] = newAuthorizationAt.Format(time.RFC3339Nano)
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
		Where("id = ?", fixture.account.ID).Update("metadata", metadata).Error)
	require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
		Where("id = ?", fixture.oauth.ID).UpdateColumn("created_at", newAuthorizationAt).Error)

	request := newMetaInstagramDeletionRequest(t, signMetaInstagramLifecycleRequest(
		t, issuedAt, fixture.profileID, metaInstagramTestAppSecret,
	))
	require.NoError(t, fixture.app.DeleteMetaInstagramUserData(request))
	assert.Equal(t, fasthttp.StatusOK, request.RequestCtx.Response.StatusCode())
	var account models.ChannelAccount
	require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusActive, account.Status)
	assert.True(t, boolConfigValue(account.Config, "outbound_enabled"))
	var oauth models.ChannelCredential
	require.NoError(t, fixture.db.First(&oauth, "id = ?", fixture.oauth.ID).Error)
	assert.Equal(t, models.ChannelCredentialStatusActive, oauth.Status)
	var privacyCount int64
	require.NoError(t, fixture.db.Model(&models.PrivacyRequest{}).
		Where("organization_id = ? AND received_channel = ?", fixture.org.ID, metaInstagramDeletionReceivedChannel).
		Count(&privacyCount).Error)
	assert.Equal(t, int64(1), privacyCount, "privacy workflow must survive a newer reconnect")
}

func TestManagedInstagramDataDeletionQuarantinesMixedAuthorizationGeneration(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Redis = testutil.SetupTestRedis(t)
	queued, dispatching := createMetaInstagramManualOutboxPair(t, fixture)
	eventAt := time.Now().UTC().Add(-5 * time.Second).Truncate(time.Second)
	metadata := cloneJSONB(fixture.account.Metadata)
	metadata[metaMessengerAuthorizationGrantedAtKey] = eventAt.Add(-time.Minute).Format(time.RFC3339Nano)
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
		Where("id = ?", fixture.account.ID).Update("metadata", metadata).Error)
	require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
		Where("id = ?", fixture.oauth.ID).UpdateColumn("created_at", eventAt.Add(time.Minute)).Error)
	request := newMetaInstagramDeletionRequest(t, signMetaInstagramLifecycleRequest(
		t, eventAt, fixture.profileID, metaInstagramTestAppSecret,
	))
	require.NoError(t, fixture.app.DeleteMetaInstagramUserData(request))
	assert.Equal(t, fasthttp.StatusOK, request.RequestCtx.Response.StatusCode())
	assertMetaInstagramQueuedWorkFence(t, fixture, queued, dispatching)
	var account models.ChannelAccount
	require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusDegraded, account.Status)
	assert.True(t, metaInstagramDeletionReconciliationPending(account.Metadata))
	assert.Equal(t, "data_deletion_generation_ambiguous", stringConfigValue(account.Metadata, "meta_ownership_reason"))
}

func TestManagedInstagramAmbiguousDataDeletionRequiresStrictlyNewerReconnect(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Redis = testutil.SetupTestRedis(t)
	queued, dispatching := createMetaInstagramManualOutboxPair(t, fixture)
	eventIssuedAt := time.Now().UTC().Add(-10 * time.Second).Truncate(time.Second)
	metadata := cloneJSONB(fixture.account.Metadata)
	metadata[metaMessengerAuthorizationGrantedAtKey] = eventIssuedAt.Format(time.RFC3339Nano)
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
		Where("id = ?", fixture.account.ID).Update("metadata", metadata).Error)
	require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
		Where("id = ?", fixture.oauth.ID).UpdateColumn("created_at", eventIssuedAt).Error)

	request := newMetaInstagramDeletionRequest(t, signMetaInstagramLifecycleRequest(
		t, eventIssuedAt, fixture.profileID, metaInstagramTestAppSecret,
	))
	require.NoError(t, fixture.app.DeleteMetaInstagramUserData(request))
	assert.Equal(t, fasthttp.StatusOK, request.RequestCtx.Response.StatusCode())
	assertMetaInstagramQueuedWorkFence(t, fixture, queued, dispatching)
	var account models.ChannelAccount
	require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusDegraded, account.Status)
	assert.True(t, metaInstagramDeletionReconciliationPending(account.Metadata))
	assert.Equal(t, "data_deletion_reconciliation_required", stringConfigValue(account.Metadata, "meta_activation_state"))
	assert.Equal(
		t, "data_deletion_reconciliation_required",
		fixture.app.metaInstagramReleaseGuardReason(account, fixture.org.ID),
	)
	_, err := fixture.app.loadMetaInstagramRevalidationSnapshot(
		fixture.org.ID, fixture.account.ID, time.Now().UTC(),
	)
	require.Error(t, err, "activation snapshot must retain the unresolved deletion fence")

	var providerCalls atomic.Int32
	fixture.app.HTTPClient = &http.Client{Transport: metaInstagramRoundTripFunc(func(*http.Request) (*http.Response, error) {
		providerCalls.Add(1)
		return nil, errors.New("Graph must not run while deletion generation is unresolved")
	})}
	fixture.app.revalidateOneMetaInstagramBinding(
		t.Context(), fixture.org.ID, fixture.account.ID, time.Now().UTC(),
	)
	assert.Zero(t, providerCalls.Load())
	require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
	assert.True(t, metaInstagramDeletionReconciliationPending(account.Metadata))
	assert.Equal(t, models.ChannelAccountStatusDegraded, account.Status)
	_, err = fixture.app.scopedApp(fixture.db, fixture.org.ID).loadMetaRegistryBinding(
		metaregistry.ResolveRequest{
			Channel: models.ChannelInstagram, ExternalAccountID: fixture.profileID,
			Purpose: metaregistry.ResolvePurposeHealth,
		}, time.Now().UTC(),
	)
	require.ErrorIs(t, err, metaregistry.ErrStaleBinding)

	expiresAt := time.Now().UTC().Add(30 * 24 * time.Hour)
	rotate := func(startedAt time.Time, token string) error {
		return fixture.app.WithCommittedTenantApp(fixture.org.ID, func(scoped *App) error {
			_, rotateErr := scoped.rotateMetaInstagramBinding(metaInstagramRotateInput{
				AccountID: fixture.account.ID, OrganizationID: fixture.org.ID, UserID: fixture.user.ID,
				Profile: metaInstagramProfile{
					ID: fixture.profileID, UserID: fixture.profileID, Username: "synthetic_profile", AccountType: "BUSINESS",
				},
				Inspection: metaInstagramTokenInspection{
					AppID: metaInstagramTestAppID, UserID: fixture.profileID,
					Scopes: append([]string(nil), metaInstagramRequiredScopes...), CheckedAt: time.Now().UTC(),
				},
				AccessToken: token, AuthorizationStartedAt: startedAt, TokenExpiresAt: &expiresAt,
			})
			return rotateErr
		})
	}
	require.ErrorIs(
		t, rotate(eventIssuedAt, "synthetic-same-second-deletion-token"),
		errMetaInstagramAuthorizationSuperseded,
	)
	require.NoError(t, rotate(eventIssuedAt.Add(2*time.Second), "synthetic-newer-deletion-token"))
	account = models.ChannelAccount{}
	require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusPending, account.Status)
	assert.False(t, metaInstagramDeletionReconciliationPending(account.Metadata))
	assert.Equal(t, "newer_authorization_preserved", stringConfigValue(account.Metadata, metaInstagramDeletionResolvedStateKey))
	var credentials []models.ChannelCredential
	require.NoError(t, fixture.db.Where(
		"channel_account_id = ? AND kind = ?", fixture.account.ID, models.ChannelCredentialKindOAuth,
	).Order("version").Find(&credentials).Error)
	require.Len(t, credentials, 2)
	assert.Equal(t, models.ChannelCredentialStatusRevoked, credentials[0].Status)
	assert.Equal(t, models.ChannelCredentialStatusActive, credentials[1].Status)
}

func TestManagedInstagramPendingDeauthorizationReconnectRequiresNewerOAuthGeneration(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	queued, dispatching := createMetaInstagramManualOutboxPair(t, fixture)
	eventIssuedAt := time.Now().UTC().Add(-10 * time.Second).Truncate(time.Second)
	digest := strings.Repeat("c", sha256.Size*2)
	metadata := cloneJSONB(fixture.account.Metadata)
	metadata[metaDeauthorizationPendingDigestKey] = digest
	metadata[metaDeauthorizationPendingIssuedKey] = eventIssuedAt.Format(time.RFC3339)
	metadata["meta_ownership_state"] = metaregistry.OwnershipStale
	metadata["meta_activation_state"] = "deauthorization_reconciliation_required"
	accountConfig := cloneJSONB(fixture.account.Config)
	accountConfig["outbound_enabled"] = false
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
		Where("id = ?", fixture.account.ID).Updates(map[string]any{
		"status": models.ChannelAccountStatusDegraded, "metadata": metadata, "config": accountConfig,
	}).Error)
	expiresAt := time.Now().UTC().Add(30 * 24 * time.Hour)
	rotate := func(startedAt time.Time, token string) error {
		return fixture.app.WithCommittedTenantApp(fixture.org.ID, func(scoped *App) error {
			_, rotateErr := scoped.rotateMetaInstagramBinding(metaInstagramRotateInput{
				AccountID: fixture.account.ID, OrganizationID: fixture.org.ID, UserID: fixture.user.ID,
				Profile: metaInstagramProfile{
					ID: fixture.profileID, UserID: fixture.profileID, Username: "synthetic_profile", AccountType: "BUSINESS",
				},
				Inspection: metaInstagramTokenInspection{
					AppID: metaInstagramTestAppID, UserID: fixture.profileID,
					Scopes: append([]string(nil), metaInstagramRequiredScopes...), CheckedAt: time.Now().UTC(),
				},
				AccessToken: token, AuthorizationStartedAt: startedAt, TokenExpiresAt: &expiresAt,
			})
			return rotateErr
		})
	}
	require.ErrorIs(t, rotate(eventIssuedAt, "synthetic-same-second-deauth-token"), errMetaInstagramAuthorizationSuperseded)
	var queuedBefore models.OutboxJob
	require.NoError(t, fixture.db.First(&queuedBefore, "id = ?", queued.ID).Error)
	assert.Equal(t, models.OutboxJobStatusProcessing, queuedBefore.Status, "failed rotation must roll back cancellation")
	require.NoError(t, rotate(eventIssuedAt.Add(2*time.Second), "synthetic-newer-deauth-token"))
	assertMetaInstagramQueuedWorkFence(t, fixture, queued, dispatching)
	var account models.ChannelAccount
	require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
	assert.False(t, metaDeauthorizationReconciliationPending(account.Metadata))
	assert.Equal(t, digest, stringConfigValue(account.Metadata, metaDeauthorizationResolvedDigestKey))
	assert.Equal(t, "newer_authorization_preserved", stringConfigValue(account.Metadata, metaDeauthorizationResolvedStateKey))
}

func TestManagedInstagramDataDeletionJournalCleanupIsBounded(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	now := time.Now().UTC()
	oldCompleted := now.Add(-metaInstagramDeletionCompletedRetention - time.Hour)
	oldVerified := now.Add(-metaInstagramDeletionUnresolvedRetention - time.Hour)
	fresh := now.Add(-time.Hour)
	organizationID := fixture.org.ID
	requestID := uuid.New()
	rows := []models.MetaInstagramDataDeletionEvent{
		{Digest: strings.Repeat("1", 64), OrganizationID: organizationID, PlatformAppID: metaInstagramTestAppID, AuthorizingUserID: fixture.profileID, IssuedAt: oldCompleted, VerifiedAt: oldCompleted, State: "completed", PrivacyRequestID: &requestID, RequestNumber: "IGDEL" + strings.Repeat("A", 32), CompletedAt: &oldCompleted},
		{Digest: strings.Repeat("2", 64), OrganizationID: organizationID, PlatformAppID: metaInstagramTestAppID, AuthorizingUserID: fixture.profileID, IssuedAt: oldVerified, VerifiedAt: oldVerified, State: "verified"},
		{Digest: strings.Repeat("3", 64), OrganizationID: organizationID, PlatformAppID: metaInstagramTestAppID, AuthorizingUserID: fixture.profileID, IssuedAt: fresh, VerifiedAt: fresh, State: "verified"},
	}
	require.NoError(t, fixture.db.Create(&rows).Error)
	require.NoError(t, fixture.app.cleanupMetaInstagramDeletionEvents(now))
	var remaining []string
	require.NoError(t, fixture.db.Model(&models.MetaInstagramDataDeletionEvent{}).
		Where("platform_app_id = ? AND authorizing_user_id = ?", metaInstagramTestAppID, fixture.profileID).
		Order("digest").Pluck("digest", &remaining).Error)
	assert.Equal(t, []string{strings.Repeat("3", 64)}, remaining)
}

func TestManagedInstagramJournalCleanupRetainsPurposeEvidenceWithoutStarvingOrdinaryCleanup(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	_, complianceOrganization := createMetaInstagramPlatformComplianceOrganization(t, fixture.db)
	configureMetaInstagramOrganizationSet(
		fixture, []uuid.UUID{fixture.org.ID}, nil, complianceOrganization.ID,
	)

	now := time.Now().UTC().Truncate(time.Microsecond)
	oldVerified := now.Add(-metaInstagramDeletionUnresolvedRetention - time.Hour)
	purposeEvent := models.MetaInstagramDataDeletionEvent{
		Digest: strings.Repeat("4", 64), OrganizationID: complianceOrganization.ID,
		PlatformAppID: strings.Repeat("a", 64), AuthorizingUserID: strings.Repeat("b", 64),
		IssuedAt: oldVerified, VerifiedAt: oldVerified, State: "verified",
		TargetResolution: metaInstagramDeletionResolutionNoTarget, IdentityHashed: true,
		LastAttemptAt: &oldVerified,
	}
	ordinaryEvent := models.MetaInstagramDataDeletionEvent{
		Digest: strings.Repeat("5", 64), OrganizationID: fixture.org.ID,
		PlatformAppID: metaInstagramTestAppID, AuthorizingUserID: fixture.profileID,
		IssuedAt: oldVerified, VerifiedAt: oldVerified, State: "verified",
		TargetResolution: metaInstagramDeletionResolutionExact,
	}
	oldDeauthorization := models.MetaDeauthorizationEvent{
		Digest: strings.Repeat("6", 64), PlatformAppID: metaInstagramTestAppID,
		AuthorizingUserID: fixture.profileID, IssuedAt: oldVerified,
		VerifiedAt: oldVerified, State: "verified",
	}
	require.NoError(t, fixture.db.Create(&purposeEvent).Error)
	require.NoError(t, fixture.db.Create(&ordinaryEvent).Error)
	require.NoError(t, fixture.db.Create(&oldDeauthorization).Error)

	require.NoError(t, fixture.app.cleanupMetaInstagramLifecycleJournals(now))

	var purposeCount int64
	require.NoError(t, fixture.db.Model(&models.MetaInstagramDataDeletionEvent{}).
		Where("digest = ? AND organization_id = ?", purposeEvent.Digest, complianceOrganization.ID).
		Count(&purposeCount).Error)
	assert.Equal(t, int64(1), purposeCount, "purpose-owned evidence must be permanently retained")
	var ordinaryCount int64
	require.NoError(t, fixture.db.Model(&models.MetaInstagramDataDeletionEvent{}).
		Where("digest = ? AND organization_id = ?", ordinaryEvent.Digest, fixture.org.ID).
		Count(&ordinaryCount).Error)
	assert.Zero(t, ordinaryCount, "ordinary expired journals must still be cleaned")
	var deauthorizationCount int64
	require.NoError(t, fixture.db.Model(&models.MetaDeauthorizationEvent{}).
		Where("digest = ?", oldDeauthorization.Digest).
		Count(&deauthorizationCount).Error)
	assert.Zero(t, deauthorizationCount, "purpose retention must not starve later deauthorization cleanup")
}

func signMetaInstagramLifecycleRequest(
	t *testing.T,
	issuedAt time.Time,
	userID, secret string,
) string {
	t.Helper()
	payload, err := json.Marshal(metaMessengerSignedRequestPayload{
		Algorithm: "HMAC-SHA256", IssuedAt: issuedAt.UTC().Unix(), UserID: userID,
	})
	require.NoError(t, err)
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(encoded))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)) + "." + encoded
}

func newMetaInstagramDeletionRequest(t *testing.T, signedRequest string) *fastglue.Request {
	t.Helper()
	request := testutil.NewRequest(t)
	request.RequestCtx.Request.Header.SetMethod(fasthttp.MethodPost)
	request.RequestCtx.Request.Header.SetContentType("application/x-www-form-urlencoded")
	request.RequestCtx.Request.SetBodyString(url.Values{"signed_request": {signedRequest}}.Encode())
	return request
}

func TestManagedInstagramDisconnectClaimIsNonRoutableAndAmbiguityBlocksReconnect(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	queued, dispatching := createMetaInstagramManualOutboxPair(t, fixture)
	operationID := uuid.New()
	var claim metaInstagramDisconnectClaim
	err := fixture.app.WithCommittedTenantApp(fixture.org.ID, func(scoped *App) error {
		var claimErr error
		claim, claimErr = scoped.claimMetaInstagramDisconnect(
			fixture.org.ID, fixture.user.ID, fixture.account.ID,
			fixture.profileID, operationID, time.Now().UTC().Add(2*time.Minute),
		)
		return claimErr
	})
	require.NoError(t, err)
	assert.Equal(t, metaMessengerSubscriptionDesiredUnsubscribed, claim.Operation.DesiredState)
	var account models.ChannelAccount
	require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusPending, account.Status)
	assert.False(t, boolConfigValue(account.Config, "outbound_enabled"))
	assertMetaInstagramQueuedWorkFence(t, fixture, queued, dispatching)

	scoped := fixture.app.scopedApp(fixture.db, fixture.org.ID)
	_, err = scoped.loadMetaRegistryBinding(metaregistry.ResolveRequest{
		Channel: models.ChannelInstagram, ExternalAccountID: fixture.profileID,
		Purpose: metaregistry.ResolvePurposeHealth,
	}, time.Now().UTC())
	require.ErrorIs(t, err, metaregistry.ErrStaleBinding)

	err = fixture.app.WithCommittedTenantApp(fixture.org.ID, func(scoped *App) error {
		return scoped.recordMetaInstagramSubscriptionOperationFailure(
			fixture.org.ID, fixture.account.ID, claim.Operation,
			metaMessengerSubscriptionUnsubscribePending,
			metaMessengerSubscriptionUnsubscribeFailed,
			time.Now().UTC(),
		)
	})
	require.NoError(t, err)
	expiresAt := time.Now().UTC().Add(30 * 24 * time.Hour)
	authorizationStartedAt := time.Now().UTC().Add(-time.Second)
	err = fixture.app.WithCommittedTenantApp(fixture.org.ID, func(scoped *App) error {
		_, rotateErr := scoped.rotateMetaInstagramBinding(metaInstagramRotateInput{
			AccountID: fixture.account.ID, OrganizationID: fixture.org.ID, UserID: fixture.user.ID,
			Profile: metaInstagramProfile{
				ID: fixture.profileID, UserID: fixture.profileID, Username: "synthetic_profile", AccountType: "BUSINESS",
			},
			Inspection: metaInstagramTokenInspection{
				AppID: metaInstagramTestAppID, UserID: fixture.profileID,
				Scopes: append([]string(nil), metaInstagramRequiredScopes...), CheckedAt: time.Now().UTC(),
			},
			AccessToken:            "synthetic-reconnect-token",
			AuthorizationStartedAt: authorizationStartedAt,
			TokenExpiresAt:         &expiresAt,
		})
		return rotateErr
	})
	require.ErrorIs(t, err, errMetaMessengerSubscriptionFence)
}

func TestManagedInstagramDisconnectClaimRequiresExactTeardownBinding(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, metaInstagramLifecycleFixture)
	}{
		{
			name: "profile identity drift",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				metadata := cloneJSONB(fixture.account.Metadata)
				metadata["meta_authorizing_user_id"] = "700000000009911"
				require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
					Where("id = ?", fixture.account.ID).Update("metadata", metadata).Error)
			},
		},
		{
			name: "oauth expiry missing",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
					Where("id = ?", fixture.oauth.ID).UpdateColumn("expires_at", nil).Error)
			},
		},
		{
			name: "subscribed operation generation drift",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				metadata := cloneJSONB(fixture.account.Metadata)
				metadata[metaMessengerSubscriptionOAuthVersionKey] = fixture.oauth.Version + 1
				require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
					Where("id = ?", fixture.account.ID).Update("metadata", metadata).Error)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newMetaInstagramLifecycleFixture(t)
			original, ok := metaMessengerSubscriptionOperationFromMetadata(fixture.account.Metadata)
			require.True(t, ok)
			test.mutate(t, fixture)
			err := fixture.app.WithCommittedTenantApp(fixture.org.ID, func(scoped *App) error {
				_, claimErr := scoped.claimMetaInstagramDisconnect(
					fixture.org.ID, fixture.user.ID, fixture.account.ID,
					fixture.profileID, uuid.New(), time.Now().UTC().Add(2*time.Minute),
				)
				return claimErr
			})
			require.Error(t, err)
			var account models.ChannelAccount
			require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
			after, ok := metaMessengerSubscriptionOperationFromMetadata(account.Metadata)
			require.True(t, ok)
			assert.Equal(t, original.ID, after.ID, "a rejected teardown must not claim a new operation")
			assert.Equal(t, models.ChannelAccountStatusActive, account.Status)
		})
	}
}

func TestManagedInstagramUnsubscribeReconciliationRechecksExactTeardownBindingBeforeGraph(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, metaInstagramLifecycleFixture)
	}{
		{
			name: "identity drift",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				var account models.ChannelAccount
				require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
				metadata := cloneJSONB(account.Metadata)
				metadata["meta_authority_asset_id"] = "700000000009912"
				require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
					Where("id = ?", fixture.account.ID).Update("metadata", metadata).Error)
			},
		},
		{
			name: "credential generation drift",
			mutate: func(t *testing.T, fixture metaInstagramLifecycleFixture) {
				require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
					Where("id = ?", fixture.webhook.ID).UpdateColumn("created_at", time.Now().UTC().Add(2*time.Minute)).Error)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newMetaInstagramLifecycleFixture(t)
			fixture.app.Redis = testutil.SetupTestRedis(t)
			claim, err := func() (metaInstagramDisconnectClaim, error) {
				var value metaInstagramDisconnectClaim
				err := fixture.app.WithCommittedTenantApp(fixture.org.ID, func(scoped *App) error {
					var claimErr error
					value, claimErr = scoped.claimMetaInstagramDisconnect(
						fixture.org.ID, fixture.user.ID, fixture.account.ID,
						fixture.profileID, uuid.New(), time.Now().UTC().Add(2*time.Minute),
					)
					return claimErr
				})
				return value, err
			}()
			require.NoError(t, err)
			require.NotEqual(t, uuid.Nil, claim.Operation.ID)
			require.NoError(t, fixture.app.WithCommittedTenantApp(fixture.org.ID, func(scoped *App) error {
				return scoped.recordMetaInstagramSubscriptionOperationFailure(
					fixture.org.ID, fixture.account.ID, claim.Operation,
					metaMessengerSubscriptionUnsubscribePending,
					metaMessengerSubscriptionUnsubscribeFailed,
					time.Now().UTC(),
				)
			}))
			test.mutate(t, fixture)
			var providerCalls atomic.Int32
			fixture.app.HTTPClient = &http.Client{Transport: metaInstagramRoundTripFunc(func(*http.Request) (*http.Response, error) {
				providerCalls.Add(1)
				return nil, errors.New("drifted teardown must not reach Graph")
			})}
			request := newAuthenticatedMetaInstagramMutationRequest(t, fixture, fixture.account.ID)
			require.NoError(t, fixture.app.ReconcileMetaInstagramSubscription(request))
			assert.Equal(t, fasthttp.StatusConflict, request.RequestCtx.Response.StatusCode())
			assert.Zero(t, providerCalls.Load())
		})
	}
}

func TestManagedInstagramExpiredAmbiguousOperationIsAtomicallyReleasedForExactReconciliation(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Redis = testutil.SetupTestRedis(t)
	var claim metaInstagramDisconnectClaim
	require.NoError(t, fixture.app.WithCommittedTenantApp(fixture.org.ID, func(scoped *App) error {
		var claimErr error
		claim, claimErr = scoped.claimMetaInstagramDisconnect(
			fixture.org.ID, fixture.user.ID, fixture.account.ID,
			fixture.profileID, uuid.New(), time.Now().UTC().Add(2*time.Minute),
		)
		return claimErr
	}))
	require.NoError(t, fixture.app.WithCommittedTenantApp(fixture.org.ID, func(scoped *App) error {
		return scoped.recordMetaInstagramSubscriptionOperationFailure(
			fixture.org.ID, fixture.account.ID, claim.Operation,
			metaMessengerSubscriptionUnsubscribePending,
			metaMessengerSubscriptionUnsubscribeFailed,
			time.Now().UTC(),
		)
	}))
	var account models.ChannelAccount
	require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
	expired := time.Now().UTC().Add(-time.Minute)
	metadata := cloneJSONB(account.Metadata)
	metadata[metaMessengerSubscriptionOperationExpiresKey] = expired.Format(time.RFC3339Nano)
	metadata[metaMessengerSubscriptionFencedOperationEndKey] = expired.Format(time.RFC3339Nano)
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
		Where("id = ?", fixture.account.ID).Update("metadata", metadata).Error)
	var providerCalls atomic.Int32
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		providerCalls.Add(1)
		assert.NotEmpty(t, request.URL.Query().Get("appsecret_proof"))
		switch request.Method {
		case http.MethodDelete:
			_, _ = io.WriteString(w, `{"success":true}`)
		case http.MethodGet:
			_, _ = io.WriteString(w, `{"data":[]}`)
		default:
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
		}
	}))
	defer provider.Close()
	fixture.app.HTTPClient = testutil.NewHTTPSRewriteClient(t, map[string]*httptest.Server{
		"https://graph.instagram.test": provider,
	})
	request := newAuthenticatedMetaInstagramMutationRequest(t, fixture, fixture.account.ID)
	require.NoError(t, fixture.app.ReconcileMetaInstagramSubscription(request))
	assert.Equal(t, fasthttp.StatusOK, request.RequestCtx.Response.StatusCode())
	assert.Equal(t, int32(2), providerCalls.Load())
	require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusDisconnected, account.Status)
}

func TestManagedInstagramSubscribeReconciliationIsReleaseFencedButUnsubscribeCleanupRemains(
	t *testing.T,
) {
	fixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Redis = testutil.SetupTestRedis(t)
	fixture.app.Config.MetaInstagram.QuarantineOnly = true
	fixture.app.Config.MetaInstagram.AppReviewStatus = "rejected"
	now := time.Now().UTC()
	subscribeOperation := metaMessengerSubscriptionOperation{
		ID: uuid.New(), OAuthCredentialID: fixture.oauth.ID, OAuthVersion: fixture.oauth.Version,
		WebhookCredentialID: fixture.webhook.ID, WebhookVersion: fixture.webhook.Version,
		DesiredState: metaMessengerSubscriptionDesiredSubscribed,
		State:        metaMessengerSubscriptionSubscribeFailed,
		ExpiresAt:    now.Add(2 * time.Minute),
	}
	metadata := metadataWithMetaMessengerSubscriptionOperation(
		fixture.account.Metadata, subscribeOperation, metaMessengerSubscriptionRemoteUnknown,
	)
	metadata[metaMessengerSubscriptionOperationStateKey] = metaMessengerSubscriptionSubscribeFailed
	metadata[metaMessengerSubscriptionFencedOperationIDKey] = subscribeOperation.ID.String()
	metadata[metaMessengerSubscriptionFencedOperationEndKey] = subscribeOperation.ExpiresAt.Format(time.RFC3339Nano)
	metadata[metaMessengerSubscriptionFencedAckKey] = false
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
		Where("id = ?", fixture.account.ID).Update("metadata", metadata).Error)
	var providerCalls atomic.Int32
	fixture.app.HTTPClient = &http.Client{Transport: metaInstagramRoundTripFunc(func(*http.Request) (*http.Response, error) {
		providerCalls.Add(1)
		return nil, errors.New("subscribe must be fenced before provider mutation")
	})}
	request := newAuthenticatedMetaInstagramMutationRequest(t, fixture, fixture.account.ID)
	require.NoError(t, fixture.app.ReconcileMetaInstagramSubscription(request))
	assert.Equal(t, fasthttp.StatusConflict, request.RequestCtx.Response.StatusCode())
	assert.Zero(t, providerCalls.Load())

	unsubscribeOperation := subscribeOperation
	unsubscribeOperation.ID = uuid.New()
	unsubscribeOperation.DesiredState = metaMessengerSubscriptionDesiredUnsubscribed
	unsubscribeOperation.State = metaMessengerSubscriptionUnsubscribeFailed
	unsubscribeOperation.ExpiresAt = time.Now().UTC().Add(2 * time.Minute)
	metadata = metadataWithMetaMessengerSubscriptionOperation(
		metadata, unsubscribeOperation, metaMessengerSubscriptionRemoteUnknown,
	)
	metadata[metaMessengerSubscriptionOperationStateKey] = metaMessengerSubscriptionUnsubscribeFailed
	metadata[metaMessengerSubscriptionFencedOperationIDKey] = unsubscribeOperation.ID.String()
	metadata[metaMessengerSubscriptionFencedOperationEndKey] = unsubscribeOperation.ExpiresAt.Format(time.RFC3339Nano)
	metadata[metaMessengerSubscriptionFencedAckKey] = false
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
		Where("id = ?", fixture.account.ID).Update("metadata", metadata).Error)
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, providerRequest *http.Request) {
		providerCalls.Add(1)
		assert.NotEmpty(t, providerRequest.URL.Query().Get("appsecret_proof"))
		assert.Equal(t, "/v25.0/"+fixture.profileID+"/subscribed_apps", providerRequest.URL.Path)
		switch providerRequest.Method {
		case http.MethodDelete:
			_, _ = io.WriteString(w, `{"success":true}`)
		case http.MethodGet:
			_, _ = io.WriteString(w, `{"data":[]}`)
		default:
			t.Errorf("unexpected provider method %s", providerRequest.Method)
			http.Error(w, "unexpected", http.StatusMethodNotAllowed)
		}
	}))
	defer provider.Close()
	fixture.app.HTTPClient = testutil.NewHTTPSRewriteClient(t, map[string]*httptest.Server{
		"https://graph.instagram.test": provider,
	})
	request = newAuthenticatedMetaInstagramMutationRequest(t, fixture, fixture.account.ID)
	require.NoError(t, fixture.app.ReconcileMetaInstagramSubscription(request))
	assert.Equal(t, fasthttp.StatusOK, request.RequestCtx.Response.StatusCode())
	assert.Equal(t, int32(2), providerCalls.Load(), "only unsubscribe DELETE and verification GET may run")
	var account models.ChannelAccount
	require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusDisconnected, account.Status)
}

func TestManagedInstagramSubscribeReconciliationCannotRecoverQuarantinedOwnership(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Redis = testutil.SetupTestRedis(t)
	now := time.Now().UTC()
	operation := metaMessengerSubscriptionOperation{
		ID: uuid.New(), OAuthCredentialID: fixture.oauth.ID, OAuthVersion: fixture.oauth.Version,
		WebhookCredentialID: fixture.webhook.ID, WebhookVersion: fixture.webhook.Version,
		DesiredState: metaMessengerSubscriptionDesiredSubscribed,
		State:        metaMessengerSubscriptionSubscribeFailed,
		ExpiresAt:    now.Add(5 * time.Minute),
	}
	metadata := metadataWithMetaMessengerSubscriptionOperation(
		fixture.account.Metadata, operation, metaMessengerSubscriptionRemoteUnknown,
	)
	metadata["meta_ownership_state"] = metaregistry.OwnershipStale
	metadata["meta_activation_state"] = "quarantined"
	metadata[metaMessengerSubscriptionFencedOperationIDKey] = operation.ID.String()
	metadata[metaMessengerSubscriptionFencedOperationEndKey] = operation.ExpiresAt.Format(time.RFC3339Nano)
	metadata[metaMessengerSubscriptionFencedAckKey] = false
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
		Where("id = ?", fixture.account.ID).Updates(map[string]any{
		"status": models.ChannelAccountStatusDegraded, "metadata": metadata,
	}).Error)
	var providerCalls atomic.Int32
	fixture.app.HTTPClient = &http.Client{Transport: metaInstagramRoundTripFunc(func(*http.Request) (*http.Response, error) {
		providerCalls.Add(1)
		return nil, errors.New("quarantined subscribe must not reach Graph")
	})}
	request := newAuthenticatedMetaInstagramMutationRequest(t, fixture, fixture.account.ID)
	require.NoError(t, fixture.app.ReconcileMetaInstagramSubscription(request))
	assert.Equal(t, fasthttp.StatusConflict, request.RequestCtx.Response.StatusCode())
	assert.Zero(t, providerCalls.Load())
	var after models.ChannelAccount
	require.NoError(t, fixture.db.First(&after, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusDegraded, after.Status)
	assert.Equal(t, metaregistry.OwnershipStale, stringConfigValue(after.Metadata, "meta_ownership_state"))
	assert.Equal(t, "quarantined", stringConfigValue(after.Metadata, "meta_activation_state"))
}

func TestManagedInstagramSubscribeReconciliationRejectsPartialManagedMarkersBeforeGraph(t *testing.T) {
	for _, removedMarker := range []string{"meta_registry_managed", "meta_management_mode"} {
		t.Run(removedMarker, func(t *testing.T) {
			fixture := newMetaInstagramLifecycleFixture(t)
			fixture.app.Redis = testutil.SetupTestRedis(t)
			now := time.Now().UTC()
			operation := metaMessengerSubscriptionOperation{
				ID: uuid.New(), OAuthCredentialID: fixture.oauth.ID, OAuthVersion: fixture.oauth.Version,
				WebhookCredentialID: fixture.webhook.ID, WebhookVersion: fixture.webhook.Version,
				DesiredState: metaMessengerSubscriptionDesiredSubscribed,
				State:        metaMessengerSubscriptionSubscribeFailed,
				ExpiresAt:    now.Add(5 * time.Minute),
			}
			metadata := metadataWithMetaMessengerSubscriptionOperation(
				fixture.account.Metadata, operation, metaMessengerSubscriptionRemoteUnknown,
			)
			metadata["meta_activation_state"] = "awaiting_relay_registry"
			metadata[metaMessengerSubscriptionFencedOperationIDKey] = operation.ID.String()
			metadata[metaMessengerSubscriptionFencedOperationEndKey] = operation.ExpiresAt.Format(time.RFC3339Nano)
			metadata[metaMessengerSubscriptionFencedAckKey] = false
			config := cloneJSONB(fixture.account.Config)
			delete(config, removedMarker)
			config["outbound_enabled"] = false
			config["ai_reply_enabled"] = false
			require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
				Where("id = ?", fixture.account.ID).Updates(map[string]any{
				"status": models.ChannelAccountStatusPending,
				"config": config, "metadata": metadata,
			}).Error)
			var providerCalls atomic.Int32
			fixture.app.HTTPClient = &http.Client{Transport: metaInstagramRoundTripFunc(func(*http.Request) (*http.Response, error) {
				providerCalls.Add(1)
				return nil, errors.New("partial subscribe must not reach Graph")
			})}
			request := newAuthenticatedMetaInstagramMutationRequest(t, fixture, fixture.account.ID)
			require.NoError(t, fixture.app.ReconcileMetaInstagramSubscription(request))
			assert.Equal(t, fasthttp.StatusConflict, request.RequestCtx.Response.StatusCode())
			assert.Zero(t, providerCalls.Load())
			var account models.ChannelAccount
			require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
			assert.Equal(t, models.ChannelAccountStatusPending, account.Status)
			assert.Equal(t, metaMessengerSubscriptionSubscribeFailed,
				stringConfigValue(account.Metadata, metaMessengerSubscriptionOperationStateKey))
		})
	}
}

func TestMetaInstagramGraphErrorsNeverExposeProviderMessageOrCredential(t *testing.T) {
	const secretToken = "synthetic-sensitive-instagram-token"
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"`+secretToken+` leaked by provider","type":"OAuthException","code":190,"fbtrace_id":"safe-trace"}}`)
	}))
	defer provider.Close()
	app := &App{HTTPClient: provider.Client()}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, provider.URL, nil)
	require.NoError(t, err)
	var destination map[string]any
	err = app.doMetaInstagramJSON(request, &destination)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secretToken)
	assert.Contains(t, err.Error(), "code 190")
	assert.Contains(t, err.Error(), "safe-trace")
}

func TestClassifyMetaInstagramRevalidationErrorIsFailClosed(t *testing.T) {
	outcome, reason := classifyMetaInstagramRevalidationError(&metaInstagramProviderError{
		StatusCode: http.StatusUnauthorized, Code: 190,
	})
	assert.Equal(t, metaregistry.OwnershipRevoked, outcome)
	assert.Equal(t, "provider_authorization_revoked", reason)
	outcome, _ = classifyMetaInstagramRevalidationError(&metaInstagramProviderError{
		StatusCode: http.StatusServiceUnavailable,
	})
	assert.Equal(t, "transient", outcome)
	outcome, _ = classifyMetaInstagramRevalidationError(errors.New("malformed response"))
	assert.Equal(t, "transient", outcome)
}

func TestManagedInstagramFixtureContainsNoPlaintextCredentialRows(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	var credentials []models.ChannelCredential
	require.NoError(t, fixture.db.Where("channel_account_id = ?", fixture.account.ID).Find(&credentials).Error)
	for _, credential := range credentials {
		for _, value := range credential.CredentialBlob {
			encoded, _ := value.(string)
			assert.True(t, appcrypto.IsEncrypted(encoded))
			assert.False(t, strings.Contains(encoded, "synthetic-old-instagram-token"))
		}
	}
}

func createMetaInstagramManualOutboxPair(
	t *testing.T,
	fixture metaInstagramLifecycleFixture,
) (models.OutboxJob, models.OutboxJob) {
	t.Helper()
	contact := models.Contact{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: fixture.org.ID,
		PhoneNumber: "ig-" + uuid.NewString(), WhatsAppAccount: fixture.account.Name,
		Tags: models.JSONBArray{}, Metadata: models.JSONB{},
	}
	require.NoError(t, fixture.db.Create(&contact).Error)
	conversation := models.InboxConversation{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: fixture.org.ID,
		ChannelAccountID: fixture.account.ID, ContactID: contact.ID,
		Channel: fixture.account.Channel, ExternalConversationID: "meta-thread-" + uuid.NewString(),
		Status: models.InboxConversationStatusOpen, OpenedAt: time.Now().UTC(),
		Config: models.JSONB{}, Metadata: models.JSONB{},
	}
	require.NoError(t, fixture.db.Create(&conversation).Error)
	createMessage := func(content string) models.Message {
		message := models.Message{
			BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: fixture.org.ID,
			WhatsAppAccount: fixture.account.Name, ContactID: contact.ID,
			ConversationID: conversation.ExternalConversationID, InboxConversationID: &conversation.ID,
			Direction: models.DirectionOutgoing, MessageType: models.MessageTypeText,
			Content: content, Status: models.MessageStatusPending, Metadata: models.JSONB{},
		}
		require.NoError(t, fixture.db.Create(&message).Error)
		return message
	}
	now := time.Now().UTC()
	queuedMessage := createMessage("Synthetic queued manual reply")
	queued := models.OutboxJob{
		BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: fixture.org.ID,
		ChannelAccountID: fixture.account.ID, ConversationID: conversation.ID,
		MessageID: &queuedMessage.ID, IdempotencyKey: "ig-manual-queued:" + uuid.NewString(),
		PayloadDigest: strings.Repeat("1", sha256.Size*2), Purpose: models.ChannelPreferencePurposeService,
		Status: models.OutboxJobStatusProcessing, AvailableAt: now, LockedAt: &now,
		LockedBy: "synthetic-worker", MaxAttempts: 8, ProviderState: models.JSONB{}, Payload: models.JSONB{},
	}
	require.NoError(t, fixture.db.Create(&queued).Error)
	dispatchingMessage := createMessage("Synthetic dispatching manual reply")
	dispatching := queued
	dispatching.ID = uuid.New()
	dispatching.MessageID = &dispatchingMessage.ID
	dispatching.IdempotencyKey = "ig-manual-dispatching:" + uuid.NewString()
	dispatching.Status = models.OutboxJobStatusDispatching
	dispatching.ProviderState = models.JSONB{"provider_attempt": "fenced"}
	require.NoError(t, fixture.db.Create(&dispatching).Error)
	return queued, dispatching
}

func assertMetaInstagramQueuedWorkFence(
	t *testing.T,
	fixture metaInstagramLifecycleFixture,
	queued, dispatching models.OutboxJob,
) {
	t.Helper()
	require.NoError(t, fixture.db.First(&queued, "id = ?", queued.ID).Error)
	assert.Equal(t, models.OutboxJobStatusCancelled, queued.Status)
	assert.Equal(t, "managed_meta_generation_cancelled", queued.LastErrorCode)
	require.NotNil(t, queued.MessageID)
	var queuedMessage models.Message
	require.NoError(t, fixture.db.First(&queuedMessage, "id = ?", *queued.MessageID).Error)
	assert.Equal(t, models.MessageStatusFailed, queuedMessage.Status)

	require.NoError(t, fixture.db.First(&dispatching, "id = ?", dispatching.ID).Error)
	assert.Equal(t, models.OutboxJobStatusDispatching, dispatching.Status)
	require.NotNil(t, dispatching.MessageID)
	var dispatchingMessage models.Message
	require.NoError(t, fixture.db.First(&dispatchingMessage, "id = ?", *dispatching.MessageID).Error)
	assert.Equal(t, models.MessageStatusPending, dispatchingMessage.Status)
}

func newMetaInstagramDeauthorizationRequest(
	t *testing.T,
	signedRequest string,
) *fastglue.Request {
	t.Helper()
	request := testutil.NewRequest(t)
	request.RequestCtx.Request.Header.SetMethod(fasthttp.MethodPost)
	request.RequestCtx.Request.Header.SetContentType("application/x-www-form-urlencoded")
	request.RequestCtx.Request.SetBodyString(url.Values{"signed_request": {signedRequest}}.Encode())
	return request
}

func newAuthenticatedMetaInstagramMutationRequest(
	t *testing.T,
	fixture metaInstagramLifecycleFixture,
	accountID uuid.UUID,
) *fastglue.Request {
	t.Helper()
	request := testutil.NewJSONRequest(t, map[string]any{})
	testutil.SetAuthContext(request, fixture.org.ID, fixture.user.ID)
	testutil.SetHeader(request, "X-Organization-ID", fixture.org.ID.String())
	testutil.SetPathParam(request, "id", accountID.String())
	return request
}

func newSignedMetaRegistryMutationRequest(
	t *testing.T,
	fixture metaInstagramLifecycleFixture,
	mutation metaregistry.MutationRequest,
) *fastglue.Request {
	t.Helper()
	request := testutil.NewJSONRequest(t, mutation)
	request.RequestCtx.Request.Header.SetMethod(fasthttp.MethodPost)
	now := time.Now().UTC()
	nonce := "synthetic-instagram-registry-" + uuid.NewString()
	raw := request.RequestCtx.PostBody()
	request.RequestCtx.Request.Header.Set(
		metaregistry.TimestampHeader, strconv.FormatInt(now.Unix(), 10),
	)
	request.RequestCtx.Request.Header.Set(metaregistry.NonceHeader, nonce)
	request.RequestCtx.Request.Header.Set(
		metaregistry.SignatureHeader,
		metaregistry.SignRequest(
			fixture.app.Config.MetaRegistry.ServiceSecret,
			fasthttp.MethodPost,
			metaregistry.ReviewPath,
			now,
			nonce,
			raw,
		),
	)
	return request
}

func startBlockedMetaInstagramHealthTest(
	t *testing.T,
	fixture metaInstagramLifecycleFixture,
) (*fastglue.Request, <-chan struct{}, chan struct{}, <-chan error) {
	t.Helper()
	providerStarted := make(chan struct{})
	releaseProvider := make(chan struct{})
	fixture.app.channelAdapterFactory = func(*models.ChannelAccount) (channelapi.Adapter, error) {
		return metaInstagramHealthAdapter{validate: func(
			ctx context.Context,
			_ *models.ChannelAccount,
		) (channelapi.AccountValidationResult, error) {
			close(providerStarted)
			select {
			case <-releaseProvider:
			case <-ctx.Done():
				return channelapi.AccountValidationResult{}, ctx.Err()
			}
			return channelapi.AccountValidationResult{
				Valid: true, Status: string(models.ChannelAccountStatusActive),
				Capabilities: channelapi.Capabilities{Text: true, Replies: true},
				CheckedAt:    time.Now().UTC(),
			}, nil
		}}, nil
	}
	request := testutil.NewJSONRequest(t, map[string]any{})
	testutil.SetAuthContext(request, fixture.org.ID, fixture.user.ID)
	testutil.SetHeader(request, "X-Organization-ID", fixture.org.ID.String())
	testutil.SetPathParam(request, "id", fixture.account.ID.String())
	handlerDone := make(chan error, 1)
	go func() {
		handlerDone <- fixture.app.TestChannelAccount(request)
	}()
	return request, providerStarted, releaseProvider, handlerDone
}

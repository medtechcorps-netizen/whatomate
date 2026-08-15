package handlers

import (
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/metaregistry"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

func TestManagedInstagramAuthenticatedRegistryMutationRejectsBindingDriftWithoutSideEffects(t *testing.T) {
	tests := []struct {
		name  string
		drift func(*testing.T, *metaInstagramLifecycleFixture, *metaregistry.MutationRequest)
	}{
		{
			name: "foreign organization",
			drift: func(t *testing.T, fixture *metaInstagramLifecycleFixture, _ *metaregistry.MutationRequest) {
				foreign := testutil.CreateTestOrganization(t, fixture.db)
				fixture.app.Config.MetaInstagram.AllowedOrganizationID = foreign.ID.String()
			},
		},
		{
			name: "configured app",
			drift: func(t *testing.T, fixture *metaInstagramLifecycleFixture, _ *metaregistry.MutationRequest) {
				metadata := cloneJSONB(fixture.account.Metadata)
				metadata["meta_platform_app_id"] = "100000000000199"
				require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
					Where("id = ?", fixture.account.ID).Update("metadata", metadata).Error)
			},
		},
		{
			name: "configured development profile",
			drift: func(_ *testing.T, fixture *metaInstagramLifecycleFixture, _ *metaregistry.MutationRequest) {
				fixture.app.Config.MetaInstagram.DevelopmentTestProfileID = "700000000000199"
			},
		},
		{
			name: "profile identity",
			drift: func(t *testing.T, fixture *metaInstagramLifecycleFixture, _ *metaregistry.MutationRequest) {
				require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
					Where("id = ?", fixture.account.ID).
					Update("external_account_id", "9"+fixture.profileID).Error)
			},
		},
		{
			name: "authorizing identity",
			drift: func(t *testing.T, fixture *metaInstagramLifecycleFixture, _ *metaregistry.MutationRequest) {
				metadata := cloneJSONB(fixture.account.Metadata)
				metadata["meta_authorizing_user_id"] = "700000000000197"
				require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
					Where("id = ?", fixture.account.ID).Update("metadata", metadata).Error)
			},
		},
		{
			name: "authority asset",
			drift: func(t *testing.T, fixture *metaInstagramLifecycleFixture, _ *metaregistry.MutationRequest) {
				metadata := cloneJSONB(fixture.account.Metadata)
				metadata["meta_authority_asset_id"] = "700000000000196"
				require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
					Where("id = ?", fixture.account.ID).Update("metadata", metadata).Error)
			},
		},
		{
			name: "managed boolean marker",
			drift: func(t *testing.T, fixture *metaInstagramLifecycleFixture, _ *metaregistry.MutationRequest) {
				config := cloneJSONB(fixture.account.Config)
				config["meta_registry_managed"] = false
				require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
					Where("id = ?", fixture.account.ID).Update("config", config).Error)
			},
		},
		{
			name: "management mode marker",
			drift: func(t *testing.T, fixture *metaInstagramLifecycleFixture, _ *metaregistry.MutationRequest) {
				config := cloneJSONB(fixture.account.Config)
				delete(config, "meta_management_mode")
				require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
					Where("id = ?", fixture.account.ID).Update("config", config).Error)
			},
		},
		{
			name: "Instagram API mode",
			drift: func(t *testing.T, fixture *metaInstagramLifecycleFixture, _ *metaregistry.MutationRequest) {
				config := cloneJSONB(fixture.account.Config)
				config["instagram_api_mode"] = "facebook_login"
				require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
					Where("id = ?", fixture.account.ID).Update("config", config).Error)
			},
		},
		{
			name: "OAuth generation is no longer sane",
			drift: func(t *testing.T, fixture *metaInstagramLifecycleFixture, _ *metaregistry.MutationRequest) {
				expiresAt := time.Now().UTC().Add(30 * time.Second)
				require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
					Where("id = ?", fixture.oauth.ID).Update("expires_at", expiresAt).Error)
			},
		},
		{
			name: "request generation",
			drift: func(_ *testing.T, _ *metaInstagramLifecycleFixture, request *metaregistry.MutationRequest) {
				request.WebhookCredentialVersion++
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newMetaInstagramLifecycleFixture(t)
			fixture.app.Redis = testutil.SetupTestRedis(t)
			if fixture.app.Redis == nil {
				t.Skip("TEST_REDIS_URL is required for authenticated registry tests")
			}
			fixture.app.Config.MetaRegistry.ServiceSecret = metaRegistryTestServiceSecret
			fixture.app.Config.MetaRegistry.ReplayWindowSeconds = int(metaregistry.ReplayWindowFloor.Seconds())
			request := metaregistry.MutationRequest{
				ChannelAccountID:         fixture.account.ID,
				CredentialID:             fixture.oauth.ID,
				CredentialVersion:        fixture.oauth.Version,
				WebhookCredentialID:      fixture.webhook.ID,
				WebhookCredentialVersion: fixture.webhook.Version,
				Outcome:                  metaregistry.OwnershipStale,
				Reason:                   "synthetic_binding_drift",
				CheckedAt:                time.Now().UTC(),
			}
			test.drift(t, &fixture, &request)
			queued, _ := createMetaInstagramManualOutboxPair(t, fixture)

			beforeAccount, beforeCredentials, beforeAuditCount :=
				metaInstagramRegistryMutationSnapshot(t, fixture)
			httpRequest, nonce := newAuthenticatedMetaRegistryServiceMutationRequest(
				t, fixture, metaregistry.ReviewPath, request,
			)
			require.NoError(t, fixture.app.RecordMetaRegistryRevalidation(httpRequest))
			assertAuthenticatedMetaRegistryResponse(
				t, fixture, httpRequest, nonce, fasthttp.StatusNotFound,
			)
			assertMetaInstagramRegistryMutationUnchanged(
				t, fixture, beforeAccount, beforeCredentials, beforeAuditCount, queued,
			)
		})
	}
}

func TestManagedInstagramAuthenticatedRegistryMutationAllowsExactReleaseShutdown(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		outcome     string
		configure   func(metaInstagramLifecycleFixture)
		wantStatus  models.ChannelAccountStatus
		wantRevoked bool
	}{
		{
			name: "quarantine stale downgrade", path: metaregistry.ReviewPath,
			outcome: metaregistry.OwnershipStale,
			configure: func(fixture metaInstagramLifecycleFixture) {
				fixture.app.Config.MetaInstagram.QuarantineOnly = true
			},
			wantStatus: models.ChannelAccountStatusDegraded,
		},
		{
			name: "review downgrade revoke", path: metaregistry.RevokePath,
			outcome: metaregistry.OwnershipRevoked,
			configure: func(fixture metaInstagramLifecycleFixture) {
				fixture.app.Config.App.Environment = "development"
				fixture.app.Config.MetaInstagram.AppReviewStatus = "rejected"
				fixture.app.Config.MetaInstagram.DevelopmentTestProfileID = fixture.profileID
				fixture.app.Config.MetaInstagram.DevelopmentAppRole = "tester"
			},
			wantStatus: models.ChannelAccountStatusDisconnected, wantRevoked: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newMetaInstagramLifecycleFixture(t)
			fixture.app.Redis = testutil.SetupTestRedis(t)
			if fixture.app.Redis == nil {
				t.Skip("TEST_REDIS_URL is required for authenticated registry tests")
			}
			fixture.app.Config.MetaRegistry.ServiceSecret = metaRegistryTestServiceSecret
			fixture.app.Config.MetaRegistry.ReplayWindowSeconds = int(metaregistry.ReplayWindowFloor.Seconds())
			test.configure(fixture)
			queued, dispatching := createMetaInstagramManualOutboxPair(t, fixture)
			request := metaregistry.MutationRequest{
				ChannelAccountID:         fixture.account.ID,
				CredentialID:             fixture.oauth.ID,
				CredentialVersion:        fixture.oauth.Version,
				WebhookCredentialID:      fixture.webhook.ID,
				WebhookCredentialVersion: fixture.webhook.Version,
				Outcome:                  test.outcome, Reason: "synthetic_release_shutdown",
				CheckedAt: time.Now().UTC(),
			}
			httpRequest, nonce := newAuthenticatedMetaRegistryServiceMutationRequest(
				t, fixture, test.path, request,
			)
			var err error
			if test.path == metaregistry.RevokePath {
				err = fixture.app.RevokeMetaRegistryBinding(httpRequest)
			} else {
				err = fixture.app.RecordMetaRegistryRevalidation(httpRequest)
			}
			require.NoError(t, err)
			assertAuthenticatedMetaRegistryResponse(
				t, fixture, httpRequest, nonce, fasthttp.StatusOK,
			)

			var account models.ChannelAccount
			require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
			assert.Equal(t, test.wantStatus, account.Status)
			assert.Equal(t, test.outcome, stringConfigValue(account.Metadata, "meta_ownership_state"))
			assert.False(t, boolConfigValue(account.Config, "outbound_enabled"))
			assert.False(t, boolConfigValue(account.Config, "ai_reply_enabled"))
			var credentials []models.ChannelCredential
			require.NoError(t, fixture.db.Where("channel_account_id = ?", fixture.account.ID).
				Order("id").Find(&credentials).Error)
			require.Len(t, credentials, 2)
			for _, credential := range credentials {
				if test.wantRevoked {
					assert.Equal(t, models.ChannelCredentialStatusRevoked, credential.Status)
				} else {
					assert.Equal(t, models.ChannelCredentialStatusActive, credential.Status)
				}
			}
			assertMetaInstagramQueuedWorkFence(t, fixture, queued, dispatching)
		})
	}
}

func newAuthenticatedMetaRegistryServiceMutationRequest(
	t *testing.T,
	fixture metaInstagramLifecycleFixture,
	path string,
	mutation metaregistry.MutationRequest,
) (*fastglue.Request, string) {
	t.Helper()
	request := testutil.NewJSONRequest(t, mutation)
	request.RequestCtx.Request.Header.SetMethod(fasthttp.MethodPost)
	now := time.Now().UTC()
	nonce := "synthetic-instagram-mutation-fence-" + uuid.NewString()
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
			path,
			now,
			nonce,
			raw,
		),
	)
	return request, nonce
}

func assertAuthenticatedMetaRegistryResponse(
	t *testing.T,
	fixture metaInstagramLifecycleFixture,
	request *fastglue.Request,
	nonce string,
	wantStatus int,
) {
	t.Helper()
	status := request.RequestCtx.Response.StatusCode()
	body := request.RequestCtx.Response.Body()
	assert.Equal(t, wantStatus, status, string(body))
	require.NoError(t, metaregistry.VerifyResponse(
		fixture.app.Config.MetaRegistry.ServiceSecret,
		nonce,
		status,
		body,
		string(request.RequestCtx.Response.Header.Peek(metaregistry.ResponseHeader)),
	))
}

func metaInstagramRegistryMutationSnapshot(
	t *testing.T,
	fixture metaInstagramLifecycleFixture,
) (models.ChannelAccount, []models.ChannelCredential, int64) {
	t.Helper()
	var account models.ChannelAccount
	require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
	var credentials []models.ChannelCredential
	require.NoError(t, fixture.db.Where("channel_account_id = ?", fixture.account.ID).
		Order("id").Find(&credentials).Error)
	var auditCount int64
	require.NoError(t, fixture.db.Model(&models.AuditLog{}).Where(
		"organization_id = ? AND resource_type = ? AND resource_id = ?",
		fixture.org.ID, "meta_channel_registry", fixture.account.ID,
	).Count(&auditCount).Error)
	return account, credentials, auditCount
}

func assertMetaInstagramRegistryMutationUnchanged(
	t *testing.T,
	fixture metaInstagramLifecycleFixture,
	beforeAccount models.ChannelAccount,
	beforeCredentials []models.ChannelCredential,
	beforeAuditCount int64,
	queued models.OutboxJob,
) {
	t.Helper()
	afterAccount, afterCredentials, afterAuditCount :=
		metaInstagramRegistryMutationSnapshot(t, fixture)
	assert.Equal(t, beforeAccount.Status, afterAccount.Status)
	assert.Equal(t, beforeAccount.Config, afterAccount.Config)
	assert.Equal(t, beforeAccount.Metadata, afterAccount.Metadata)
	assert.Equal(t, beforeAccount.UpdatedAt, afterAccount.UpdatedAt)
	require.Len(t, afterCredentials, len(beforeCredentials))
	for index := range beforeCredentials {
		assert.Equal(t, beforeCredentials[index].ID, afterCredentials[index].ID)
		assert.Equal(t, beforeCredentials[index].Version, afterCredentials[index].Version)
		assert.Equal(t, beforeCredentials[index].Status, afterCredentials[index].Status)
		assert.Equal(t, beforeCredentials[index].RevokedAt, afterCredentials[index].RevokedAt)
	}
	assert.Equal(t, beforeAuditCount, afterAuditCount)

	require.NoError(t, fixture.db.First(&queued, "id = ?", queued.ID).Error)
	assert.Equal(t, models.OutboxJobStatusProcessing, queued.Status)
	assert.Empty(t, queued.LastErrorCode)
	require.NotNil(t, queued.MessageID)
	var message models.Message
	require.NoError(t, fixture.db.First(&message, "id = ?", *queued.MessageID).Error)
	assert.Equal(t, models.MessageStatusPending, message.Status)
}

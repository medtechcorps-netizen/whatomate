package handlers

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/metaregistry"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

const (
	metaInstagramCallbackTestProfessionalID = "700000000000191"
	metaInstagramCallbackTestOAuthSubjectID = "800000000000191"
	metaInstagramSeededMismatchedProfileID  = "799999999999991"
)

func TestExactManagedInstagramCallbackBindingRejectsEveryBindingMismatch(t *testing.T) {
	newAccount := func() models.ChannelAccount {
		return models.ChannelAccount{
			Channel:           models.ChannelInstagram,
			Provider:          channelapi.RelayProvider,
			ExternalAccountID: metaInstagramCallbackTestProfessionalID,
			Config: models.JSONB{
				"meta_registry_managed": true,
				"meta_management_mode":  metaregistry.ManagementModePlatformOAuth,
				"instagram_api_mode":    "instagram_login",
			},
			Metadata: models.JSONB{
				"meta_webhook_app":             "instagram_login",
				"meta_platform_app_id":         metaInstagramTestAppID,
				"meta_authority_asset_id":      metaInstagramCallbackTestProfessionalID,
				"meta_authorizing_user_id":     metaInstagramCallbackTestOAuthSubjectID,
				metaInstagramOAuthSubjectIDKey: metaInstagramCallbackTestOAuthSubjectID,
			},
		}
	}
	baseline := newAccount()
	assert.True(t, exactManagedInstagramCallbackBinding(
		&baseline, metaInstagramTestAppID, metaInstagramCallbackTestOAuthSubjectID,
	))
	assert.False(t, exactManagedInstagramCallbackBinding(
		&baseline, metaInstagramTestAppID, metaInstagramCallbackTestProfessionalID,
	), "a professional routing ID is not the signed app-scoped subject")

	for _, test := range []struct {
		name   string
		mutate func(*models.ChannelAccount)
	}{
		{"channel", func(account *models.ChannelAccount) { account.Channel = models.ChannelMessenger }},
		{"provider", func(account *models.ChannelAccount) { account.Provider = "seeded_wrong_provider" }},
		{"managed marker", func(account *models.ChannelAccount) { account.Config["meta_registry_managed"] = false }},
		{"management mode", func(account *models.ChannelAccount) { account.Config["meta_management_mode"] = "seeded_wrong_mode" }},
		{"Instagram API mode", func(account *models.ChannelAccount) { account.Config["instagram_api_mode"] = "facebook_login" }},
		{"webhook app", func(account *models.ChannelAccount) { account.Metadata["meta_webhook_app"] = "messenger" }},
		{"platform app", func(account *models.ChannelAccount) { account.Metadata["meta_platform_app_id"] = "100000000000199" }},
		{"external profile", func(account *models.ChannelAccount) {
			account.ExternalAccountID = metaInstagramSeededMismatchedProfileID
		}},
		{"authority asset", func(account *models.ChannelAccount) {
			account.Metadata["meta_authority_asset_id"] = metaInstagramSeededMismatchedProfileID
		}},
		{"authorizing user", func(account *models.ChannelAccount) {
			account.Metadata["meta_authorizing_user_id"] = metaInstagramSeededMismatchedProfileID
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			account := newAccount()
			test.mutate(&account)
			assert.False(t, exactManagedInstagramCallbackBinding(
				&account, metaInstagramTestAppID, metaInstagramCallbackTestOAuthSubjectID,
			))
		})
	}
}

func TestManagedInstagramCallbacksUseDistinctAppScopedSubjectAndProfessionalAsset(t *testing.T) {
	for _, callback := range []struct {
		name string
		run  func(*testing.T, metaInstagramLifecycleFixture, string)
	}{
		{
			name: "deauthorization",
			run: func(t *testing.T, fixture metaInstagramLifecycleFixture, signedRequest string) {
				request := newMetaInstagramDeauthorizationRequest(t, signedRequest)
				require.NoError(t, fixture.app.DeauthorizeMetaInstagram(request))
				assert.Equal(t, fasthttp.StatusOK, request.RequestCtx.Response.StatusCode())
			},
		},
		{
			name: "data deletion",
			run: func(t *testing.T, fixture metaInstagramLifecycleFixture, signedRequest string) {
				request := newMetaInstagramDeletionRequest(t, signedRequest)
				require.NoError(t, fixture.app.DeleteMetaInstagramUserData(request))
				assert.Equal(t, fasthttp.StatusOK, request.RequestCtx.Response.StatusCode())
			},
		},
	} {
		t.Run(callback.name, func(t *testing.T) {
			fixture := newMetaInstagramLifecycleFixture(t)
			fixture.app.Redis = testutil.SetupTestRedis(t)
			require.NotNil(t, fixture.app.Redis)
			oauthSubjectID := "8" + fixture.profileID
			metadata := cloneJSONB(fixture.account.Metadata)
			metadata["meta_authorizing_user_id"] = oauthSubjectID
			metadata[metaInstagramOAuthSubjectIDKey] = oauthSubjectID
			require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
				Where("id = ?", fixture.account.ID).Update("metadata", metadata).Error)
			require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
				Where("id = ?", fixture.oauth.ID).
				UpdateColumn("created_at", time.Now().UTC().Add(-time.Hour)).Error)

			binding, err := fixture.app.scopedApp(fixture.db, fixture.org.ID).loadMetaRegistryBinding(
				metaregistry.ResolveRequest{
					Channel: models.ChannelInstagram, ExternalAccountID: fixture.profileID,
					Purpose: metaregistry.ResolvePurposeHealth,
				}, time.Now().UTC(),
			)
			require.NoError(t, err)
			assert.Equal(t, fixture.profileID, binding.ExternalAccountID)

			issuedAt := time.Now().UTC().Truncate(time.Second)
			callback.run(t, fixture, signMetaInstagramLifecycleRequest(
				t, issuedAt, oauthSubjectID, metaInstagramTestAppSecret,
			))
			var account models.ChannelAccount
			require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
			assert.Equal(t, models.ChannelAccountStatusDisconnected, account.Status)
			assert.Equal(t, fixture.profileID, account.ExternalAccountID)
			assert.Equal(t, fixture.profileID, stringConfigValue(account.Metadata, "meta_authority_asset_id"))
			assert.Equal(t, oauthSubjectID, stringConfigValue(account.Metadata, "meta_authorizing_user_id"))
		})
	}
}

func TestManagedInstagramDeauthorizationRejectsSeededProfileAuthorityMismatchBeforeMutation(
	t *testing.T,
) {
	fixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Redis = testutil.SetupTestRedis(t)
	queued, dispatching := createMetaInstagramManualOutboxPair(t, fixture)
	issuedAt := time.Now().UTC().Truncate(time.Second)
	before := seedManagedInstagramCallbackIdentityMismatch(t, fixture, issuedAt)
	signedRequest := signMetaInstagramLifecycleRequest(
		t, issuedAt, fixture.profileID, metaInstagramTestAppSecret,
	)
	_, digest, err := verifyMetaMessengerSignedRequestSignature(
		signedRequest, metaInstagramTestAppSecret,
	)
	require.NoError(t, err)

	request := newMetaInstagramDeauthorizationRequest(t, signedRequest)
	require.NoError(t, fixture.app.DeauthorizeMetaInstagram(request))
	assert.Equal(t, fasthttp.StatusServiceUnavailable, request.RequestCtx.Response.StatusCode())
	assertManagedInstagramCallbackMadeNoTenantMutation(
		t, fixture, before, queued, dispatching,
	)

	var journal models.MetaDeauthorizationEvent
	require.NoError(t, fixture.db.First(&journal, "digest = ?", digest).Error)
	assert.Equal(t, "verified", journal.State)
	assert.Nil(t, journal.CompletedAt)
}

func TestManagedInstagramDeauthorizationRejectsInvalidFormEnvelopeBeforeAnyState(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Redis = testutil.SetupTestRedis(t)
	signedRequest := signMetaInstagramLifecycleRequest(
		t, time.Now().UTC().Truncate(time.Second), fixture.profileID, metaInstagramTestAppSecret,
	)
	for _, test := range []struct {
		name    string
		request func() *fastglue.Request
	}{
		{
			name: "oversized form body with tiny signed request",
			request: func() *fastglue.Request {
				request := testutil.NewRequest(t)
				request.RequestCtx.Request.Header.SetMethod(fasthttp.MethodPost)
				request.RequestCtx.Request.Header.SetContentType("application/x-www-form-urlencoded")
				request.RequestCtx.Request.SetBodyString(
					"signed_request=x&padding=" + strings.Repeat("a", metaDeauthorizationMaxSignedRequest),
				)
				return request
			},
		},
		{
			name: "query-only signed request",
			request: func() *fastglue.Request {
				request := testutil.NewRequest(t)
				request.RequestCtx.Request.Header.SetMethod(fasthttp.MethodPost)
				request.RequestCtx.Request.Header.SetContentType("application/x-www-form-urlencoded")
				request.RequestCtx.QueryArgs().Set("signed_request", signedRequest)
				request.RequestCtx.Request.SetBodyString("padding=x")
				return request
			},
		},
		{
			name: "wrong content type",
			request: func() *fastglue.Request {
				request := testutil.NewRequest(t)
				request.RequestCtx.Request.Header.SetMethod(fasthttp.MethodPost)
				request.RequestCtx.Request.Header.SetContentType("application/json")
				request.RequestCtx.Request.SetBodyString(`{"signed_request":"` + signedRequest + `"}`)
				return request
			},
		},
		{
			name: "ambiguous extra form argument",
			request: func() *fastglue.Request {
				request := testutil.NewRequest(t)
				request.RequestCtx.Request.Header.SetMethod(fasthttp.MethodPost)
				request.RequestCtx.Request.Header.SetContentType("application/x-www-form-urlencoded")
				request.RequestCtx.Request.SetBodyString(url.Values{
					"signed_request": {signedRequest}, "extra": {"x"},
				}.Encode())
				return request
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var before models.ChannelAccount
			require.NoError(t, fixture.db.First(&before, "id = ?", fixture.account.ID).Error)
			var journalCountBefore int64
			require.NoError(t, fixture.db.Model(&models.MetaDeauthorizationEvent{}).
				Count(&journalCountBefore).Error)
			keysBefore, err := fixture.app.Redis.Keys(t.Context(), "*").Result()
			require.NoError(t, err)
			request := test.request()
			require.NoError(t, fixture.app.DeauthorizeMetaInstagram(request))
			assert.Equal(t, fasthttp.StatusBadRequest, request.RequestCtx.Response.StatusCode())
			var journalCount int64
			require.NoError(t, fixture.db.Model(&models.MetaDeauthorizationEvent{}).Count(&journalCount).Error)
			assert.Equal(t, journalCountBefore, journalCount)
			var after models.ChannelAccount
			require.NoError(t, fixture.db.First(&after, "id = ?", fixture.account.ID).Error)
			assert.Equal(t, before.Status, after.Status)
			assert.Equal(t, before.Config, after.Config)
			assert.Equal(t, before.Metadata, after.Metadata)
			keys, err := fixture.app.Redis.Keys(t.Context(), "*").Result()
			require.NoError(t, err)
			assert.ElementsMatch(t, keysBefore, keys)
		})
	}
}

func TestManagedInstagramDataDeletionCompletesPrivacyForStaleMismatchWithoutTargetMutation(
	t *testing.T,
) {
	fixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Redis = testutil.SetupTestRedis(t)
	queued, dispatching := createMetaInstagramManualOutboxPair(t, fixture)
	issuedAt := time.Now().UTC().Truncate(time.Second)
	before := seedManagedInstagramCallbackIdentityMismatch(t, fixture, issuedAt)
	signedRequest := signMetaInstagramLifecycleRequest(
		t, issuedAt, fixture.profileID, metaInstagramTestAppSecret,
	)
	_, digest, err := verifyMetaMessengerSignedRequestSignature(
		signedRequest, metaInstagramTestAppSecret,
	)
	require.NoError(t, err)

	request := newMetaInstagramDeletionRequest(t, signedRequest)
	require.NoError(t, fixture.app.DeleteMetaInstagramUserData(request))
	assert.Equal(t, fasthttp.StatusOK, request.RequestCtx.Response.StatusCode())
	var firstResponse metaInstagramDeletionResponse
	require.NoError(t, json.Unmarshal(request.RequestCtx.Response.Body(), &firstResponse))
	assert.True(t, validMetaInstagramDeletionConfirmationCode(firstResponse.ConfirmationCode))
	assertManagedInstagramCallbackMadeNoTenantMutation(
		t, fixture, before, queued, dispatching,
	)
	assertCompletedMetaInstagramDeletionPrivacyWorkflow(
		t, fixture, digest, firstResponse.ConfirmationCode,
	)

	replay := newMetaInstagramDeletionRequest(t, signedRequest)
	require.NoError(t, fixture.app.DeleteMetaInstagramUserData(replay))
	assert.Equal(t, fasthttp.StatusOK, replay.RequestCtx.Response.StatusCode())
	var replayResponse metaInstagramDeletionResponse
	require.NoError(t, json.Unmarshal(replay.RequestCtx.Response.Body(), &replayResponse))
	assert.Equal(t, firstResponse, replayResponse)
	assertManagedInstagramCallbackMadeNoTenantMutation(
		t, fixture, before, queued, dispatching,
	)
	assertCompletedMetaInstagramDeletionPrivacyWorkflow(
		t, fixture, digest, firstResponse.ConfirmationCode,
	)
}

func TestManagedInstagramDataDeletionIgnoresOffAllowlistMatchingRow(
	t *testing.T,
) {
	fixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Redis = testutil.SetupTestRedis(t)
	issuedAt := time.Now().UTC().Truncate(time.Second)
	foreignFixture := seedOffAllowlistManagedInstagramDeletionTarget(
		t, fixture, issuedAt,
	)
	queued, dispatching := createMetaInstagramManualOutboxPair(t, foreignFixture)
	var before models.ChannelAccount
	require.NoError(t, fixture.db.First(
		&before, "id = ? AND organization_id = ?",
		foreignFixture.account.ID, foreignFixture.org.ID,
	).Error)
	signedRequest := signMetaInstagramLifecycleRequest(
		t, issuedAt, foreignFixture.profileID, metaInstagramTestAppSecret,
	)
	_, digest, err := verifyMetaMessengerSignedRequestSignature(
		signedRequest, metaInstagramTestAppSecret,
	)
	require.NoError(t, err)

	request := newMetaInstagramDeletionRequest(t, signedRequest)
	require.NoError(t, fixture.app.DeleteMetaInstagramUserData(request))
	assert.Equal(t, fasthttp.StatusOK, request.RequestCtx.Response.StatusCode())
	var firstResponse metaInstagramDeletionResponse
	require.NoError(t, json.Unmarshal(request.RequestCtx.Response.Body(), &firstResponse))
	assert.True(t, validMetaInstagramDeletionConfirmationCode(firstResponse.ConfirmationCode))
	assertManagedInstagramCallbackMadeNoTenantMutation(
		t, foreignFixture, before, queued, dispatching,
	)
	assertCompletedMetaInstagramDeletionPrivacyWorkflow(
		t, fixture, digest, firstResponse.ConfirmationCode,
	)
	var foreignPrivacyRequests int64
	require.NoError(t, fixture.db.Model(&models.PrivacyRequest{}).
		Where("organization_id = ?", foreignFixture.org.ID).
		Count(&foreignPrivacyRequests).Error)
	assert.Zero(t, foreignPrivacyRequests)

	replay := newMetaInstagramDeletionRequest(t, signedRequest)
	require.NoError(t, fixture.app.DeleteMetaInstagramUserData(replay))
	assert.Equal(t, fasthttp.StatusOK, replay.RequestCtx.Response.StatusCode())
	var replayResponse metaInstagramDeletionResponse
	require.NoError(t, json.Unmarshal(replay.RequestCtx.Response.Body(), &replayResponse))
	assert.Equal(t, firstResponse, replayResponse)
	assertManagedInstagramCallbackMadeNoTenantMutation(
		t, foreignFixture, before, queued, dispatching,
	)
	assertCompletedMetaInstagramDeletionPrivacyWorkflow(
		t, fixture, digest, firstResponse.ConfirmationCode,
	)
}

func seedManagedInstagramCallbackIdentityMismatch(
	t *testing.T,
	fixture metaInstagramLifecycleFixture,
	issuedAt time.Time,
) models.ChannelAccount {
	t.Helper()
	mismatchedProfileID := "9" + fixture.profileID
	metadata := cloneJSONB(fixture.account.Metadata)
	metadata["meta_authority_asset_id"] = mismatchedProfileID
	metadata[metaMessengerAuthorizationGrantedAtKey] = issuedAt.Add(-2 * time.Hour).Format(time.RFC3339Nano)
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).
		Where("id = ? AND organization_id = ?", fixture.account.ID, fixture.org.ID).
		Updates(map[string]any{
			"metadata": metadata,
		}).Error)
	require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).
		Where("id = ? AND organization_id = ?", fixture.oauth.ID, fixture.org.ID).
		UpdateColumn("created_at", issuedAt.Add(-time.Hour)).Error)

	var before models.ChannelAccount
	require.NoError(t, fixture.db.First(
		&before, "id = ? AND organization_id = ?", fixture.account.ID, fixture.org.ID,
	).Error)
	return before
}

func seedOffAllowlistManagedInstagramDeletionTarget(
	t *testing.T,
	fixture metaInstagramLifecycleFixture,
	issuedAt time.Time,
) metaInstagramLifecycleFixture {
	t.Helper()
	foreignOrganization := testutil.CreateTestOrganization(t, fixture.db)
	foreignProfileID := "8" + fixture.profileID
	foreignAccount := fixture.account
	foreignAccount.BaseModel = models.BaseModel{ID: uuid.New()}
	foreignAccount.OrganizationID = foreignOrganization.ID
	foreignAccount.Name = "Synthetic Off-Allowlist Instagram Profile"
	foreignAccount.ExternalAccountID = foreignProfileID
	foreignAccount.CreatedByID = nil
	foreignAccount.UpdatedByID = nil
	foreignAccount.Organization = nil
	foreignAccount.Credentials = nil
	foreignAccount.CreatedBy = nil
	foreignAccount.UpdatedBy = nil
	foreignAccount.Config = cloneJSONB(fixture.account.Config)
	foreignAccount.Config["rereply_webhook_url"] = "https://app.example.test/api/webhooks/channels/" + foreignAccount.ID.String()
	foreignAccount.Metadata = cloneJSONB(fixture.account.Metadata)
	foreignAccount.Metadata["meta_authorizing_user_id"] = foreignProfileID
	foreignAccount.Metadata[metaInstagramOAuthSubjectIDKey] = foreignProfileID
	foreignAccount.Metadata["meta_authority_asset_id"] = foreignProfileID
	foreignAccount.Metadata[metaMessengerAuthorizationGrantedAtKey] = issuedAt.Add(-2 * time.Hour).Format(time.RFC3339Nano)
	require.NoError(t, fixture.db.Create(&foreignAccount).Error)

	foreignOAuth := fixture.oauth
	foreignOAuth.BaseModel = models.BaseModel{ID: uuid.New()}
	foreignOAuth.OrganizationID = foreignOrganization.ID
	foreignOAuth.ChannelAccountID = foreignAccount.ID
	foreignOAuth.Organization = nil
	foreignOAuth.ChannelAccount = nil
	require.NoError(t, fixture.db.Create(&foreignOAuth).Error)
	foreignWebhook := fixture.webhook
	foreignWebhook.BaseModel = models.BaseModel{ID: uuid.New()}
	foreignWebhook.OrganizationID = foreignOrganization.ID
	foreignWebhook.ChannelAccountID = foreignAccount.ID
	foreignWebhook.Organization = nil
	foreignWebhook.ChannelAccount = nil
	require.NoError(t, fixture.db.Create(&foreignWebhook).Error)

	return metaInstagramLifecycleFixture{
		app: fixture.app, db: fixture.db, org: foreignOrganization,
		profileID: foreignProfileID, account: foreignAccount,
		oauth: foreignOAuth, webhook: foreignWebhook,
	}
}

func assertCompletedMetaInstagramDeletionPrivacyWorkflow(
	t *testing.T,
	fixture metaInstagramLifecycleFixture,
	digest, confirmationCode string,
) {
	t.Helper()
	var privacyRequests []models.PrivacyRequest
	require.NoError(t, fixture.db.Where(
		"organization_id = ?", fixture.org.ID,
	).Find(&privacyRequests).Error)
	require.Len(t, privacyRequests, 1)
	privacyRequest := privacyRequests[0]
	assert.Equal(t, confirmationCode, privacyRequest.RequestNumber)
	assert.Equal(t, models.PrivacyRequestStatusInProgress, privacyRequest.Status)
	assert.Equal(t, metaInstagramDeletionVerification, privacyRequest.VerificationMethod)

	var privacyEvents []models.PrivacyRequestEvent
	require.NoError(t, fixture.db.Where(
		"organization_id = ? AND privacy_request_id = ?",
		fixture.org.ID, privacyRequest.ID,
	).Find(&privacyEvents).Error)
	require.Len(t, privacyEvents, 1)
	assert.Equal(t, "request_created", privacyEvents[0].EventType)

	var journal models.MetaInstagramDataDeletionEvent
	require.NoError(t, fixture.db.First(&journal, "digest = ?", digest).Error)
	assert.Equal(t, "completed", journal.State)
	require.NotNil(t, journal.CompletedAt)
	assert.Equal(t, fixture.org.ID, journal.OrganizationID)
	require.NotNil(t, journal.PrivacyRequestID)
	assert.Equal(t, privacyRequest.ID, *journal.PrivacyRequestID)
	assert.Equal(t, confirmationCode, journal.RequestNumber)
}

func assertManagedInstagramCallbackMadeNoTenantMutation(
	t *testing.T,
	fixture metaInstagramLifecycleFixture,
	before models.ChannelAccount,
	queued, dispatching models.OutboxJob,
) {
	t.Helper()
	var after models.ChannelAccount
	require.NoError(t, fixture.db.First(
		&after, "id = ? AND organization_id = ?", fixture.account.ID, fixture.org.ID,
	).Error)
	assert.Equal(t, before.Status, after.Status)
	assert.Equal(t, before.ExternalAccountID, after.ExternalAccountID)
	assert.Equal(t, before.Config, after.Config)
	assert.Equal(t, before.Metadata, after.Metadata)
	assert.True(t, before.UpdatedAt.Equal(after.UpdatedAt))

	var credentials []models.ChannelCredential
	require.NoError(t, fixture.db.Where(
		"channel_account_id = ? AND organization_id = ?", fixture.account.ID, fixture.org.ID,
	).Find(&credentials).Error)
	require.Len(t, credentials, 2)
	for _, credential := range credentials {
		assert.Equal(t, models.ChannelCredentialStatusActive, credential.Status)
	}

	var queuedAfter models.OutboxJob
	require.NoError(t, fixture.db.First(&queuedAfter, "id = ?", queued.ID).Error)
	assert.Equal(t, models.OutboxJobStatusProcessing, queuedAfter.Status)
	assert.Empty(t, queuedAfter.LastErrorCode)
	var dispatchingAfter models.OutboxJob
	require.NoError(t, fixture.db.First(&dispatchingAfter, "id = ?", dispatching.ID).Error)
	assert.Equal(t, models.OutboxJobStatusDispatching, dispatchingAfter.Status)

	for _, job := range []models.OutboxJob{queuedAfter, dispatchingAfter} {
		require.NotNil(t, job.MessageID)
		var message models.Message
		require.NoError(t, fixture.db.First(&message, "id = ?", *job.MessageID).Error)
		assert.Equal(t, models.MessageStatusPending, message.Status)
	}
}

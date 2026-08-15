package handlers

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func TestThreadsDevelopmentTestingGateAllowsOnlyExactAppRoleProfile(t *testing.T) {
	app := newIntegrationHandlerTestApp(t, integrationTestEncryptionKey)
	app.Config.App.Environment = "staging"
	organization := testutil.CreateTestOrganization(t, app.DB)
	admin := integrationTestUser(
		t,
		app,
		organization.ID,
		models.ResourceSettingsIntegrations+":"+models.ActionRead,
		models.ResourceSettingsIntegrations+":"+models.ActionWrite,
	)
	grantThreadsOAuthReviewTestEntitlement(t, app, organization, admin.ID)
	const appID = "1987654321098700"
	const allowedProfileID = "1987654321098701"
	app.Config.ThreadsAppReview.DevelopmentTestingEnabled = true
	app.Config.ThreadsAppReview.DevelopmentOrganizationID = organization.ID.String()
	app.Config.ThreadsAppReview.DevelopmentAppID = appID
	app.Config.ThreadsAppReview.DevelopmentProfileID = allowedProfileID

	configure := testutil.NewJSONRequest(t, map[string]any{
		"enabled": true,
		"config": map[string]any{
			"app_id":            appID,
			"redirect_uri":      "https://app.example.test/api/integrations/threads/callback",
			"app_review_status": "pending",
		},
		"credentials": map[string]any{
			"app_secret":           "synthetic-development-app-secret",
			"webhook_verify_token": "synthetic-development-webhook-token",
		},
	})
	testutil.SetAuthContext(configure, organization.ID, admin.ID)
	testutil.SetPathParam(configure, "provider", integrationProviderThreads)
	require.NoError(t, app.UpdateIntegration(configure))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(configure))
	var response IntegrationResponse
	testutil.ParseEnvelopeResponse(t, configure, &response)
	assert.True(t, response.Enabled)
	assert.Equal(t, "development_testing", response.Config["app_review_access_mode"])

	snapshot, err := app.loadThreadsIntegrationSnapshot(organization.ID, true)
	require.NoError(t, err)
	assert.Equal(t, "development_testing", snapshot.ReviewAccessMode)
	assert.Equal(t, allowedProfileID, snapshot.ExpectedProfileID)

	providerCalls := 0
	app.HTTPClient = &http.Client{Transport: threadsOAuthEndpointRoundTripFunc(
		func(*http.Request) (*http.Response, error) {
			providerCalls++
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body: io.NopCloser(strings.NewReader(
					`{"access_token":"short-token","user_id":"1987654321098799"}`,
				)),
			}, nil
		},
	)}
	err = app.completeThreadsOAuth(threadsOAuthState{
		OrganizationID:         organization.ID.String(),
		UserID:                 admin.ID.String(),
		Nonce:                  "development-profile-mismatch",
		IntegrationID:          snapshot.IntegrationID.String(),
		IntegrationFingerprint: snapshot.Fingerprint,
		ExpiresAt:              time.Now().UTC().Add(time.Minute),
	}, "provider-authorization-code")
	require.ErrorIs(t, err, errThreadsOAuthForbidden)
	assert.Equal(t, 1, providerCalls, "profile mismatch must stop before long-token exchange or persistence")
	var accountCount int64
	require.NoError(t, app.DB.Model(&models.ChannelAccount{}).
		Where("organization_id = ?", organization.ID).
		Count(&accountCount).Error)
	assert.Zero(t, accountCount)
}

func TestThreadsAppReviewApprovalRequiresExactApprovedStatus(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		status   any
		approved bool
	}{
		{name: "missing"},
		{name: "pending", status: "pending"},
		{name: "case variant", status: "Approved"},
		{name: "wrong type", status: true},
		{name: "status without evidence", status: "approved"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			config := models.JSONB{}
			if testCase.status != nil {
				config["app_review_status"] = testCase.status
			}
			assert.Equal(t, testCase.approved, threadsAppReviewApproved(config))
		})
	}
	assert.True(t, threadsAppReviewApproved(approvedThreadsTestConfig(
		t,
		models.JSONB{"app_id": "123456789012345"},
		"123456789012345",
	)))
}

func TestThreadsOAuthReviewDowngradeBeforeCallbackDoesNoProviderIOOrPersistence(t *testing.T) {
	app := newIntegrationHandlerTestApp(t, integrationTestEncryptionKey)
	organization := testutil.CreateTestOrganization(t, app.DB)
	admin := integrationTestUser(
		t,
		app,
		organization.ID,
		models.ResourceSettingsIntegrations+":"+models.ActionRead,
		models.ResourceSettingsIntegrations+":"+models.ActionWrite,
	)
	grantThreadsOAuthReviewTestEntitlement(t, app, organization, admin.ID)

	providerCalls := 0
	app.HTTPClient = &http.Client{Transport: threadsOAuthEndpointRoundTripFunc(
		func(*http.Request) (*http.Response, error) {
			providerCalls++
			return nil, errors.New("provider request must not run after review downgrade")
		},
	)}

	const appID = "1987654321098765"
	appSecret, err := appcrypto.Encrypt("threads-review-test-app-secret", integrationTestEncryptionKey)
	require.NoError(t, err)
	verifyToken, err := appcrypto.Encrypt("threads-review-test-verify-token", integrationTestEncryptionKey)
	require.NoError(t, err)
	binding := appID
	integration := models.ProviderIntegration{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organization.ID,
		Provider:       integrationProviderThreads,
		ThreadsAppID:   &binding,
		Enabled:        true,
		Config: approvedThreadsTestConfig(t, models.JSONB{
			"app_id":       appID,
			"redirect_uri": "https://app.example.test/api/integrations/threads/callback",
		}, appID),
		CredentialData: models.JSONB{
			"app_secret":           appSecret,
			"webhook_verify_token": verifyToken,
		},
		CreatedByID: &admin.ID,
		UpdatedByID: &admin.ID,
	}
	require.NoError(t, app.DB.Create(&integration).Error)
	approvedSnapshot, err := app.loadThreadsIntegrationSnapshot(organization.ID, true)
	require.NoError(t, err)

	downgrade := testutil.NewJSONRequest(t, map[string]any{
		"enabled": false,
		"config": map[string]any{
			"app_review_status": "pending",
		},
	})
	testutil.SetAuthContext(downgrade, organization.ID, admin.ID)
	testutil.SetPathParam(downgrade, "provider", integrationProviderThreads)
	require.NoError(t, app.UpdateIntegration(downgrade))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(downgrade))
	var downgraded models.ProviderIntegration
	require.NoError(t, app.DB.First(&downgraded, "id = ?", integration.ID).Error)
	assert.False(t, downgraded.Enabled)
	assert.Equal(t, "pending", stringJSONValue(downgraded.Config, "app_review_status"))

	_, err = app.loadThreadsIntegrationSnapshot(organization.ID, true)
	require.ErrorIs(t, err, errThreadsAppReviewApprovalRequired)
	redisClient := redis.NewClient(&redis.Options{Addr: "redis.invalid:6379", MaxRetries: -1})
	t.Cleanup(func() { _ = redisClient.Close() })
	app.Redis = redisClient
	startRequest := testutil.NewRequest(t)
	require.NoError(t, app.startThreadsOAuth(startRequest, organization.ID, admin.ID))
	testutil.AssertErrorResponse(
		t,
		startRequest,
		fasthttp.StatusConflict,
		"App Review approval is required",
	)
	assert.Contains(t, string(testutil.GetResponseBody(startRequest)), `"status":"approval_required"`)
	assert.Contains(t, string(testutil.GetResponseBody(startRequest)), `"available":false`)

	err = app.completeThreadsOAuth(threadsOAuthState{
		OrganizationID:         organization.ID.String(),
		UserID:                 admin.ID.String(),
		Nonce:                  "consumed-before-completion",
		IntegrationID:          integration.ID.String(),
		IntegrationFingerprint: approvedSnapshot.Fingerprint,
		ExpiresAt:              time.Now().UTC().Add(time.Minute),
	}, "provider-authorization-code")
	require.ErrorIs(t, err, errThreadsAppReviewApprovalRequired)
	assert.Zero(t, providerCalls, "review downgrade must stop before the first Meta request")

	var accountCount int64
	require.NoError(t, app.DB.Model(&models.ChannelAccount{}).
		Where(
			"organization_id = ? AND channel = ? AND provider = ?",
			organization.ID,
			models.ChannelThreads,
			channelapi.ThreadsProvider,
		).
		Count(&accountCount).Error)
	assert.Zero(t, accountCount)
	var credentialCount int64
	require.NoError(t, app.DB.Model(&models.ChannelCredential{}).
		Where("organization_id = ?", organization.ID).
		Count(&credentialCount).Error)
	assert.Zero(t, credentialCount)
}

func TestThreadsOAuthReviewDowngradeDuringProviderIOCannotCrossPersistenceFence(t *testing.T) {
	app := newIntegrationHandlerTestApp(t, integrationTestEncryptionKey)
	organization := testutil.CreateTestOrganization(t, app.DB)
	admin := integrationTestUser(
		t,
		app,
		organization.ID,
		models.ResourceSettingsIntegrations+":"+models.ActionRead,
		models.ResourceSettingsIntegrations+":"+models.ActionWrite,
	)
	grantThreadsOAuthReviewTestEntitlement(t, app, organization, admin.ID)
	const appID = "1987654321098766"
	const profileID = "1987654321098767"
	appSecret, err := appcrypto.Encrypt("threads-review-race-app-secret", integrationTestEncryptionKey)
	require.NoError(t, err)
	verifyToken, err := appcrypto.Encrypt("threads-review-race-verify-token", integrationTestEncryptionKey)
	require.NoError(t, err)
	binding := appID
	integration := models.ProviderIntegration{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: organization.ID,
		Provider:       integrationProviderThreads,
		ThreadsAppID:   &binding,
		Enabled:        true,
		Config: approvedThreadsTestConfig(t, models.JSONB{
			"app_id":       appID,
			"redirect_uri": "https://app.example.test/api/integrations/threads/callback",
		}, appID),
		CredentialData: models.JSONB{
			"app_secret":           appSecret,
			"webhook_verify_token": verifyToken,
		},
		CreatedByID: &admin.ID,
		UpdatedByID: &admin.ID,
	}
	require.NoError(t, app.DB.Create(&integration).Error)
	snapshot, err := app.loadThreadsIntegrationSnapshot(organization.ID, true)
	require.NoError(t, err)

	providerCalls := 0
	app.HTTPClient = &http.Client{Transport: threadsOAuthEndpointRoundTripFunc(
		func(request *http.Request) (*http.Response, error) {
			providerCalls++
			payload := "{}"
			switch request.URL.Path {
			case "/oauth/access_token":
				payload = `{"access_token":"short-token","user_id":"` + profileID + `"}`
			case "/access_token":
				payload = `{"access_token":"long-token","expires_in":5184000}`
			case "/v1.0/me":
				payload = `{"id":"` + profileID + `","username":"synthetic_profile"}`
			case "/v1.0/debug_token":
				require.NoError(t, app.DB.Model(&models.ProviderIntegration{}).
					Where("id = ?", integration.ID).
					Updates(map[string]any{
						"enabled": true,
						"config": models.JSONB{
							"app_id":            appID,
							"redirect_uri":      "https://app.example.test/api/integrations/threads/callback",
							"app_review_status": "pending",
						},
					}).Error)
				payload = `{"data":{"type":"USER","is_valid":true,"user_id":"` + profileID +
					`","scopes":["threads_basic","threads_read_replies","threads_manage_replies","threads_content_publish","threads_manage_mentions"],"expires_at":1999999999,"data_access_expires_at":1999999999}}`
			default:
				return &http.Response{
					StatusCode: http.StatusNotFound,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"error":{"code":404}}`)),
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(payload)),
			}, nil
		},
	)}
	err = app.completeThreadsOAuth(threadsOAuthState{
		OrganizationID:         organization.ID.String(),
		UserID:                 admin.ID.String(),
		Nonce:                  "review-downgrade-during-provider-io",
		IntegrationID:          integration.ID.String(),
		IntegrationFingerprint: snapshot.Fingerprint,
		ExpiresAt:              time.Now().UTC().Add(time.Minute),
	}, "provider-authorization-code")
	require.ErrorIs(t, err, errThreadsOAuthStale)
	assert.Equal(t, 4, providerCalls)
	var accountCount int64
	require.NoError(t, app.DB.Model(&models.ChannelAccount{}).
		Where("organization_id = ?", organization.ID).
		Count(&accountCount).Error)
	assert.Zero(t, accountCount)
	var credentialCount int64
	require.NoError(t, app.DB.Model(&models.ChannelCredential{}).
		Where("organization_id = ?", organization.ID).
		Count(&credentialCount).Error)
	assert.Zero(t, credentialCount)
}

func grantThreadsOAuthReviewTestEntitlement(
	t *testing.T,
	app *App,
	organization *models.Organization,
	actorID uuid.UUID,
) {
	t.Helper()
	plan := models.Plan{
		BaseModel: models.BaseModel{ID: uuid.New()},
		ScopeKey:  "platform",
		Code:      "threads-oauth-review-" + organization.ID.String(),
		Name:      "Threads OAuth Review Test",
		Status:    models.CommercialPlanStatusActive,
		Vertical:  "general",
		Metadata:  models.JSONB{},
	}
	require.NoError(t, app.DB.Create(&plan).Error)
	billingAccount := models.BillingAccount{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  organization.ID,
		Provider:        models.BillingProviderManual,
		Status:          models.BillingAccountStatusActive,
		DefaultCurrency: "MYR",
		BillingProfile:  models.JSONB{},
		ProviderData:    models.JSONB{},
		Metadata:        models.JSONB{},
	}
	require.NoError(t, app.DB.Create(&billingAccount).Error)
	now := time.Now().UTC()
	periodEnd := now.Add(24 * time.Hour)
	subscription := models.Subscription{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   organization.ID,
		BillingAccountID: billingAccount.ID,
		PlanID:           plan.ID,
		Provider:         models.BillingProviderManual,
		Status:           models.SubscriptionStatusActive,
		Quantity:         1,
		CollectionMethod: "manual",
		EntitlementsSnapshot: models.JSONB{
			channelapi.OmnichannelEntitlementKey:             true,
			channelapi.ThreadsPublicEngagementEntitlementKey: true,
		},
		ProviderData:       models.JSONB{},
		CurrentPeriodStart: &now,
		CurrentPeriodEnd:   &periodEnd,
		CreatedByID:        &actorID,
	}
	require.NoError(t, app.DB.Create(&subscription).Error)
}

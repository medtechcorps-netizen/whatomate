package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/config"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/threadsplatform"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"gorm.io/gorm"
)

const (
	managedThreadsTestAppID        = "123456789012345"
	managedThreadsTestSubjectID    = "200000000000001"
	managedThreadsTestProfileID    = "300000000000001"
	managedThreadsTestAppSecret    = "synthetic-managed-threads-app-secret-000001"
	managedThreadsTestWebhookToken = "synthetic-managed-threads-webhook-token-001"
	managedThreadsTestLongToken    = "synthetic-managed-threads-long-token-001"
)

type managedThreadsTestFixture struct {
	app          *App
	organization *models.Organization
	user         *models.User
	transport    *managedThreadsProviderTransport
}

type managedThreadsStartResponse struct {
	AuthorizationURL string    `json:"authorization_url"`
	Reconnect        bool      `json:"reconnect"`
	ExpiresAt        time.Time `json:"expires_at"`
}

type managedThreadsProviderTransport struct {
	t            *testing.T
	calls        atomic.Int32
	missingScope bool
	longToken    string
}

func (transport *managedThreadsProviderTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.t.Helper()
	transport.calls.Add(1)
	require.Equal(transport.t, "graph.threads.net", request.URL.Host)
	require.Equal(transport.t, "https", request.URL.Scheme)

	var payload any
	switch request.URL.Path {
	case "/oauth/access_token":
		require.Equal(transport.t, http.MethodPost, request.Method)
		require.NoError(transport.t, request.ParseForm())
		assert.Equal(transport.t, managedThreadsTestAppID, request.Form.Get("client_id"))
		assert.Equal(transport.t, managedThreadsTestAppSecret, request.Form.Get("client_secret"))
		assert.Equal(transport.t, "managed-authorization-code", request.Form.Get("code"))
		assert.Equal(
			transport.t,
			"https://threads.test.example/api/integrations/threads/managed/callback",
			request.Form.Get("redirect_uri"),
		)
		payload = map[string]any{
			"access_token": "synthetic-managed-threads-short-token",
			"user_id":      managedThreadsTestSubjectID,
		}
	case "/access_token":
		require.Equal(transport.t, http.MethodGet, request.Method)
		assert.Equal(transport.t, "th_exchange_token", request.URL.Query().Get("grant_type"))
		assert.Equal(transport.t, managedThreadsTestAppSecret, request.URL.Query().Get("client_secret"))
		payload = map[string]any{
			"access_token": transport.longToken,
			"expires_in":   int64(3600),
		}
	case "/v1.0/me":
		require.Equal(transport.t, http.MethodGet, request.Method)
		assert.Equal(transport.t, "Bearer "+transport.longToken, request.Header.Get("Authorization"))
		payload = map[string]any{
			"id":                          managedThreadsTestProfileID,
			"username":                    "managed_clinic",
			"name":                        "Managed Clinic",
			"threads_profile_picture_url": "https://cdn.example.test/profile.png",
		}
	case "/v1.0/debug_token":
		require.Equal(transport.t, http.MethodGet, request.Method)
		scopes := channelapi.RequiredThreadsScopes()
		if transport.missingScope {
			scopes = []string{"threads_basic"}
		}
		payload = map[string]any{
			"data": map[string]any{
				"app_id":                 managedThreadsTestAppID,
				"type":                   "USER",
				"is_valid":               true,
				"user_id":                managedThreadsTestSubjectID,
				"scopes":                 scopes,
				"expires_at":             time.Now().UTC().Add(time.Hour).Unix(),
				"data_access_expires_at": time.Now().UTC().Add(2 * time.Hour).Unix(),
			},
		}
	default:
		transport.t.Fatalf("unexpected managed Threads provider path %q", request.URL.Path)
	}
	body, err := json.Marshal(payload)
	require.NoError(transport.t, err)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(body))),
		Request:    request,
	}, nil
}

func newManagedThreadsTestFixture(t *testing.T) managedThreadsTestFixture {
	t.Helper()
	db := testutil.SetupTestDB(t)
	redisClient := testutil.SetupTestRedis(t)
	if redisClient == nil {
		t.Skip("TEST_REDIS_URL not set, skipping managed Threads OAuth integration test")
	}
	organization := testutil.CreateTestOrganization(t, db)
	app := &App{
		Config: &config.Config{
			App: config.AppConfig{
				Environment:   "staging",
				EncryptionKey: integrationTestEncryptionKey,
			},
		},
		DB:    db,
		Redis: redisClient,
		Log:   testutil.NopLogger(),
	}
	user := integrationTestUser(
		t,
		app,
		organization.ID,
		models.ResourceSettingsIntegrations+":"+models.ActionRead,
		models.ResourceSettingsIntegrations+":"+models.ActionWrite,
		models.ResourceChannelAccounts+":"+models.ActionRead,
		models.ResourceChannelAccounts+":"+models.ActionWrite,
	)
	enableBookingCommerceTestEntitlement(
		t,
		db,
		organization.ID,
		user.ID,
		channelapi.ThreadsPublicEngagementEntitlementKey,
	)

	var reseller models.Reseller
	err := db.Where("slug = ?", database.PlatformResellerSlug).First(&reseller).Error
	if err != nil {
		require.ErrorIs(t, err, gorm.ErrRecordNotFound)
		reseller = models.Reseller{
			BaseModel:        models.BaseModel{ID: uuid.New()},
			Name:             "Synthetic Platform Direct",
			Slug:             database.PlatformResellerSlug,
			Status:           models.ResellerStatusActive,
			Plan:             models.ResellerPlanEnterprise,
			MaxOrganizations: 100,
			Settings:         models.JSONB{},
		}
		require.NoError(t, db.Create(&reseller).Error)
	}
	complianceOrganization := models.Organization{
		BaseModel:  models.BaseModel{ID: uuid.New()},
		ResellerID: &reseller.ID,
		Name:       "Synthetic Threads Compliance",
		Slug:       "synthetic-threads-compliance-" + uuid.NewString()[:8],
		Settings: models.JSONB{
			threadsplatform.ComplianceOrganizationMarkerKey: true,
		},
	}
	require.NoError(t, db.Create(&complianceOrganization).Error)
	managed := config.ThreadsManagedConfig{
		Enabled:                  true,
		AllowedOrganizationIDs:   organization.ID.String(),
		ComplianceOrganizationID: complianceOrganization.ID.String(),
		ReReplyBaseURL:           "https://threads.test.example",
		PlatformApps: []config.ThreadsPlatformAppConfig{{
			PlatformAppKey:          "primary",
			AppID:                   managedThreadsTestAppID,
			AppSecret:               managedThreadsTestAppSecret,
			WebhookVerifyToken:      managedThreadsTestWebhookToken,
			AppReviewStatus:         "approved",
			AppReviewEvidenceSHA256: strings.Repeat("a", 64),
			AppReviewApprovedAt:     "2026-01-02T03:04:05Z",
			ConfigurationGeneration: 7,
		}},
	}
	runtime, err := threadsplatform.NewRuntime(
		managed,
		config.ThreadsAppReviewConfig{},
		"staging",
	)
	require.NoError(t, err)
	app.ThreadsManagedRuntime, err = runtime.ValidateComplianceOrganization(t.Context(), db)
	require.NoError(t, err)
	transport := &managedThreadsProviderTransport{
		t:         t,
		longToken: managedThreadsTestLongToken,
	}
	app.HTTPClient = &http.Client{Transport: transport}
	return managedThreadsTestFixture{
		app: app, organization: organization, user: user, transport: transport,
	}
}

func startManagedThreadsTestOAuth(
	t *testing.T,
	fixture managedThreadsTestFixture,
) (string, string, managedThreadsStartResponse) {
	t.Helper()
	request := testutil.NewJSONRequest(t, map[string]any{})
	testutil.SetAuthContext(request, fixture.organization.ID, fixture.user.ID)
	testutil.SetHeader(request, "X-Organization-ID", fixture.organization.ID.String())
	require.NoError(t, fixture.app.StartManagedThreadsOAuth(request))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))
	assertManagedThreadsOAuthPrivacyHeaders(t, request.RequestCtx)
	var response managedThreadsStartResponse
	testutil.ParseEnvelopeResponse(t, request, &response)
	authorizationURL, err := url.Parse(response.AuthorizationURL)
	require.NoError(t, err)
	assert.Equal(t, "https", authorizationURL.Scheme)
	assert.Equal(t, "www.threads.net", authorizationURL.Host)
	assert.Equal(t, "/oauth/authorize", authorizationURL.Path)
	assert.Equal(t, managedThreadsTestAppID, authorizationURL.Query().Get("client_id"))
	assert.Equal(
		t,
		"https://threads.test.example/api/integrations/threads/managed/callback",
		authorizationURL.Query().Get("redirect_uri"),
	)
	assert.Equal(t, strings.Join(channelapi.RequiredThreadsScopes(), ","), authorizationURL.Query().Get("scope"))
	nonce := authorizationURL.Query().Get("state")
	require.NotEmpty(t, nonce)
	verifier := testutil.GetResponseCookie(request, managedThreadsOAuthBrowserCookieName(nonce))
	require.Len(t, verifier, managedThreadsOAuthBrowserSecretSize)
	cookie := readFastHTTPResponseCookie(t, request, managedThreadsOAuthBrowserCookieName(nonce))
	assert.True(t, cookie.HTTPOnly)
	assert.True(t, cookie.Secure)
	assert.Equal(t, fasthttp.CookieSameSiteLaxMode, cookie.SameSite)
	assert.Equal(t, "/", cookie.Path)
	return nonce, verifier, response
}

func callbackManagedThreadsTestOAuth(
	t *testing.T,
	fixture managedThreadsTestFixture,
	nonce, verifier string,
) *fasthttp.RequestCtx {
	t.Helper()
	request := testutil.NewGETRequest(t)
	request.RequestCtx.QueryArgs().Set("state", nonce)
	request.RequestCtx.QueryArgs().Set("code", "managed-authorization-code")
	if verifier != "" {
		request.RequestCtx.Request.Header.SetCookie(
			managedThreadsOAuthBrowserCookieName(nonce),
			verifier,
		)
	}
	require.NoError(t, fixture.app.CallbackManagedThreads(request))
	assertManagedThreadsOAuthPrivacyHeaders(t, request.RequestCtx)
	return request.RequestCtx
}

func assertManagedThreadsOAuthPrivacyHeaders(t *testing.T, requestCtx *fasthttp.RequestCtx) {
	t.Helper()
	require.NotNil(t, requestCtx)
	assert.Equal(t, "no-store", string(requestCtx.Response.Header.Peek("Cache-Control")))
	assert.Equal(t, "no-cache", string(requestCtx.Response.Header.Peek("Pragma")))
	assert.Equal(t, "no-referrer", string(requestCtx.Response.Header.Peek("Referrer-Policy")))
}

func TestManagedThreadsOAuthIsBrowserBoundAndPersistsOnlyPendingDistinctClaim(t *testing.T) {
	fixture := newManagedThreadsTestFixture(t)
	nonce, verifier, start := startManagedThreadsTestOAuth(t, fixture)
	assert.False(t, start.Reconnect)

	stateJSON, err := fixture.app.Redis.Get(
		t.Context(), managedThreadsOAuthStateKey(nonce),
	).Bytes()
	require.NoError(t, err)
	assert.NotContains(t, string(stateJSON), verifier)
	assert.NotContains(t, string(stateJSON), managedThreadsTestAppSecret)
	assert.NotContains(t, string(stateJSON), managedThreadsTestWebhookToken)
	var state managedThreadsOAuthState
	require.NoError(t, json.Unmarshal(stateJSON, &state))
	assert.Equal(t, managedThreadsOAuthBrowserBindingDigest(state, verifier), state.BrowserBindingDigest)

	missingBrowser := callbackManagedThreadsTestOAuth(t, fixture, nonce, "")
	assert.Equal(t, fasthttp.StatusSeeOther, missingBrowser.Response.StatusCode())
	assert.Contains(t, string(missingBrowser.Response.Header.Peek("Location")), "threads_managed=error")
	assert.Zero(t, fixture.transport.calls.Load())
	assert.NoError(t, fixture.app.Redis.Exists(
		t.Context(), managedThreadsOAuthStateKey(nonce),
	).Err(), "a missing browser proof must not consume the server state")

	callback := callbackManagedThreadsTestOAuth(t, fixture, nonce, verifier)
	assert.Equal(t, fasthttp.StatusSeeOther, callback.Response.StatusCode())
	assert.Contains(t, string(callback.Response.Header.Peek("Location")), "threads_managed=pending")
	assert.Equal(t, int32(4), fixture.transport.calls.Load())
	assert.Zero(t, fixture.app.Redis.Exists(
		t.Context(), managedThreadsOAuthStateKey(nonce),
	).Val())

	var integration models.ProviderIntegration
	require.NoError(t, fixture.app.DB.Where(
		"organization_id = ? AND provider = ?",
		fixture.organization.ID,
		integrationProviderThreads,
	).First(&integration).Error)
	assert.Equal(t, models.ThreadsManagementModePlatformManaged, integration.ManagementMode)
	assert.False(t, integration.Enabled)
	assert.Nil(t, integration.ThreadsAppID)
	assert.Empty(t, integration.CredentialData)
	assert.Equal(t, managedThreadsPendingActivationState, stringJSONValue(integration.Config, "authorization_state"))
	assert.False(t, boolConfigValue(integration.Config, "routing_enabled"))
	assert.False(t, boolConfigValue(integration.Config, "outbound_enabled"))
	assert.False(t, boolConfigValue(integration.Config, "activation_available"))

	var accounts []models.ChannelAccount
	require.NoError(t, fixture.app.DB.Where(
		"organization_id = ? AND channel = ? AND provider = ?",
		fixture.organization.ID,
		models.ChannelThreads,
		channelapi.ThreadsProvider,
	).Find(&accounts).Error)
	require.Len(t, accounts, 1)
	account := accounts[0]
	assert.Equal(t, managedThreadsTestProfileID, account.ExternalAccountID)
	assert.Equal(t, models.ChannelAccountStatusPending, account.Status)
	assert.False(t, account.IsDefaultIncoming)
	assert.False(t, account.IsDefaultOutgoing)
	assert.Nil(t, account.ConnectedAt)
	assert.False(t, boolConfigValue(account.Config, "webhook_enabled"))
	assert.False(t, boolConfigValue(account.Config, "routing_enabled"))
	assert.False(t, boolConfigValue(account.Config, "outbound_enabled"))
	assert.False(t, boolConfigValue(account.Config, "activation_available"))
	for _, capability := range []string{"text", "replies", "public_replies", "mentions"} {
		assert.False(t, boolConfigValue(account.Capabilities, capability))
	}
	assert.NotContains(t, account.Metadata, "app_id")
	assert.NotContains(t, account.Metadata, "oauth_subject_id")

	var credentials []models.ChannelCredential
	require.NoError(t, fixture.app.DB.Where(
		"organization_id = ? AND channel_account_id = ?",
		fixture.organization.ID,
		account.ID,
	).Order("version ASC").Find(&credentials).Error)
	require.Len(t, credentials, 1)
	storedToken, _ := credentials[0].CredentialBlob["access_token"].(string)
	assert.True(t, appcrypto.IsEncrypted(storedToken))
	plaintext, err := appcrypto.Decrypt(storedToken, integrationTestEncryptionKey)
	require.NoError(t, err)
	assert.Equal(t, managedThreadsTestLongToken, plaintext)
	assert.NotEqual(t, managedThreadsTestLongToken, storedToken)
	assert.Equal(t, models.ChannelCredentialStatusActive, credentials[0].Status)

	var binding models.ThreadsPlatformBinding
	require.NoError(t, fixture.app.DB.Where(
		"organization_id = ? AND integration_id = ?",
		fixture.organization.ID,
		integration.ID,
	).First(&binding).Error)
	assert.Equal(t, managedThreadsTestSubjectID, binding.OAuthSubjectID)
	assert.Equal(t, managedThreadsTestProfileID, binding.AuthorityAssetID)
	assert.NotEqual(t, binding.OAuthSubjectID, binding.AuthorityAssetID)
	assert.Equal(t, models.ThreadsPlatformBindingStatusPending, binding.Status)
	assert.Equal(t, uint64(1), binding.AuthorizationGeneration)

	var webhookCredentials int64
	require.NoError(t, fixture.app.DB.Model(&models.ChannelCredential{}).Where(
		"organization_id = ? AND channel_account_id = ? AND kind = ?",
		fixture.organization.ID,
		account.ID,
		models.ChannelCredentialKindWebhook,
	).Count(&webhookCredentials).Error)
	assert.Zero(t, webhookCredentials)

	get := testutil.NewGETRequest(t)
	testutil.SetAuthContext(get, fixture.organization.ID, fixture.user.ID)
	require.NoError(t, fixture.app.GetIntegrations(get))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(get))
	body := string(testutil.GetResponseBody(get))
	assert.NotContains(t, body, managedThreadsTestAppID)
	assert.NotContains(t, body, managedThreadsTestAppSecret)
	assert.NotContains(t, body, managedThreadsTestWebhookToken)
	assert.NotContains(t, body, managedThreadsTestSubjectID)
	assert.NotContains(t, body, managedThreadsTestProfileID)
	var catalog struct {
		Integrations []IntegrationResponse `json:"integrations"`
	}
	testutil.ParseEnvelopeResponse(t, get, &catalog)
	var managedResponse *IntegrationResponse
	for index := range catalog.Integrations {
		if catalog.Integrations[index].Provider == integrationProviderThreads {
			managedResponse = &catalog.Integrations[index]
			break
		}
	}
	require.NotNil(t, managedResponse)
	assert.Equal(t, integrationStatusPending, managedResponse.Status)
	assert.False(t, managedResponse.Enabled)
	assert.True(t, managedResponse.Configured)
	assert.Empty(t, managedResponse.Credentials)
	assert.Equal(t, "managed_oauth", managedResponse.OAuth.Mode)
	assert.True(t, managedResponse.OAuth.Available)
	assert.Equal(t, true, managedResponse.Config["reconnect_available"])
	assert.NotContains(t, managedResponse.Config, "app_id")
	assert.NotContains(t, managedResponse.Config, "app_review_status")
	assert.NotContains(t, managedResponse.Config, "redirect_uri")

	mutation := testutil.NewJSONRequest(t, map[string]any{"enabled": true})
	testutil.SetAuthContext(mutation, fixture.organization.ID, fixture.user.ID)
	testutil.SetPathParam(mutation, "provider", integrationProviderThreads)
	require.NoError(t, fixture.app.UpdateIntegration(mutation))
	testutil.AssertErrorResponse(
		t,
		mutation,
		fasthttp.StatusConflict,
		"controlled by platform onboarding",
	)

	fixture.transport.longToken = "synthetic-managed-threads-long-token-002"
	reconnectNonce, reconnectVerifier, reconnectStart := startManagedThreadsTestOAuth(t, fixture)
	assert.True(t, reconnectStart.Reconnect)
	reconnectStateJSON, err := fixture.app.Redis.Get(
		t.Context(), managedThreadsOAuthStateKey(reconnectNonce),
	).Bytes()
	require.NoError(t, err)
	var reconnectState managedThreadsOAuthState
	require.NoError(t, json.Unmarshal(reconnectStateJSON, &reconnectState))
	foreignOrganization := testutil.CreateTestOrganization(t, fixture.app.DB)
	foreignAppKey := "primary"
	foreignIntegration := models.ProviderIntegration{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: foreignOrganization.ID,
		Provider:       integrationProviderThreads,
		ManagementMode: models.ThreadsManagementModePlatformManaged,
		PlatformAppKey: &foreignAppKey,
		Enabled:        false,
		Config:         models.JSONB{},
		CredentialData: models.JSONB{},
	}
	require.NoError(t, fixture.app.DB.Create(&foreignIntegration).Error)
	foreignAccount := models.ChannelAccount{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    foreignOrganization.ID,
		Channel:           models.ChannelThreads,
		Provider:          channelapi.ThreadsProvider,
		Name:              "Foreign managed Threads",
		ExternalAccountID: "300000000000099",
		Status:            models.ChannelAccountStatusPending,
		Capabilities:      models.JSONB{},
		Config: models.JSONB{
			"management_mode": models.ThreadsManagementModePlatformManaged,
		},
		Metadata: models.JSONB{},
	}
	require.NoError(t, fixture.app.DB.Create(&foreignAccount).Error)
	foreignBinding, err := threadsplatform.NewPostgresStore(fixture.app.DB).ClaimBinding(
		context.Background(),
		threadsplatform.BindingClaim{
			OrganizationID:          foreignOrganization.ID,
			IntegrationID:           foreignIntegration.ID,
			ChannelAccountID:        &foreignAccount.ID,
			PlatformAppKey:          foreignAppKey,
			PlatformAppID:           managedThreadsTestAppID,
			OAuthSubjectID:          "200000000000099",
			AuthorityAssetID:        foreignAccount.ExternalAccountID,
			ConfigurationGeneration: 7,
			AuthorizationGeneration: 1,
		},
	)
	require.NoError(t, err)
	foreignState := reconnectState
	foreignState.ReconnectBindingID = foreignBinding.ID.String()
	foreignState.ReconnectAccountID = foreignAccount.ID.String()
	err = fixture.app.WithCommittedTenantApp(
		fixture.organization.ID,
		func(scoped *App) error {
			return scoped.authorizeManagedThreadsStateTx(foreignState, true)
		},
	)
	assert.ErrorIs(t, err, errManagedThreadsStale)
	assert.Equal(t, int32(4), fixture.transport.calls.Load(), "a foreign binding must fail before Graph")
	require.NoError(t, fixture.app.WithCommittedTenantApp(
		fixture.organization.ID,
		func(scoped *App) error {
			return scoped.authorizeManagedThreadsStateTx(reconnectState, true)
		},
	), "a reconnect snapshot must survive an immediate server-side recheck")
	reconnectCallback := callbackManagedThreadsTestOAuth(
		t,
		fixture,
		reconnectNonce,
		reconnectVerifier,
	)
	assert.Contains(t, string(reconnectCallback.Response.Header.Peek("Location")), "threads_managed=pending")
	assert.Equal(t, int32(8), fixture.transport.calls.Load())

	require.NoError(t, fixture.app.DB.Where(
		"organization_id = ? AND integration_id = ?",
		fixture.organization.ID,
		integration.ID,
	).First(&binding).Error)
	assert.Equal(t, uint64(2), binding.AuthorizationGeneration)
	assert.Equal(t, models.ThreadsPlatformBindingStatusPending, binding.Status)
	require.NoError(t, fixture.app.DB.Where(
		"organization_id = ? AND channel_account_id = ?",
		fixture.organization.ID,
		account.ID,
	).Order("version ASC").Find(&credentials).Error)
	require.Len(t, credentials, 2)
	assert.Equal(t, models.ChannelCredentialStatusRevoked, credentials[0].Status)
	assert.Equal(t, models.ChannelCredentialStatusActive, credentials[1].Status)
	rotated, err := appcrypto.Decrypt(
		credentials[1].CredentialBlob["access_token"].(string),
		integrationTestEncryptionKey,
	)
	require.NoError(t, err)
	assert.Equal(t, fixture.transport.longToken, rotated)

	replay := callbackManagedThreadsTestOAuth(t, fixture, reconnectNonce, reconnectVerifier)
	assert.Contains(t, string(replay.Response.Header.Peek("Location")), "threads_managed=error")
	assert.Equal(t, int32(8), fixture.transport.calls.Load(), "one-time state must prevent provider replay")
}

func TestManagedThreadsCallbackRejectsStaleGenerationBeforeProvider(t *testing.T) {
	fixture := newManagedThreadsTestFixture(t)
	nonce, verifier, _ := startManagedThreadsTestOAuth(t, fixture)
	var integration models.ProviderIntegration
	require.NoError(t, fixture.app.DB.Where(
		"organization_id = ? AND provider = ?",
		fixture.organization.ID,
		integrationProviderThreads,
	).First(&integration).Error)
	configCopy := cloneJSONB(integration.Config)
	configCopy["configuration_generation"] = uint64(8)
	require.NoError(t, fixture.app.DB.Model(&models.ProviderIntegration{}).Where(
		"id = ? AND organization_id = ?",
		integration.ID,
		fixture.organization.ID,
	).Update("config", configCopy).Error)

	callback := callbackManagedThreadsTestOAuth(t, fixture, nonce, verifier)
	assert.Contains(t, string(callback.Response.Header.Peek("Location")), "threads_managed=error")
	assert.Zero(t, fixture.transport.calls.Load(), "stale server-owned generation must fail before Graph")
	var accountCount, bindingCount int64
	require.NoError(t, fixture.app.DB.Model(&models.ChannelAccount{}).Where(
		"organization_id = ? AND channel = ?",
		fixture.organization.ID,
		models.ChannelThreads,
	).Count(&accountCount).Error)
	require.NoError(t, fixture.app.DB.Model(&models.ThreadsPlatformBinding{}).Where(
		"organization_id = ?",
		fixture.organization.ID,
	).Count(&bindingCount).Error)
	assert.Zero(t, accountCount)
	assert.Zero(t, bindingCount)
}

func TestManagedThreadsCallbackRequiresEveryScopeBeforePersistence(t *testing.T) {
	fixture := newManagedThreadsTestFixture(t)
	fixture.transport.missingScope = true
	nonce, verifier, _ := startManagedThreadsTestOAuth(t, fixture)
	callback := callbackManagedThreadsTestOAuth(t, fixture, nonce, verifier)
	assert.Contains(t, string(callback.Response.Header.Peek("Location")), "threads_managed=error")
	assert.Equal(t, int32(4), fixture.transport.calls.Load())

	var accountCount, bindingCount, credentialCount int64
	require.NoError(t, fixture.app.DB.Model(&models.ChannelAccount{}).Where(
		"organization_id = ? AND channel = ?",
		fixture.organization.ID,
		models.ChannelThreads,
	).Count(&accountCount).Error)
	require.NoError(t, fixture.app.DB.Model(&models.ThreadsPlatformBinding{}).Where(
		"organization_id = ?",
		fixture.organization.ID,
	).Count(&bindingCount).Error)
	require.NoError(t, fixture.app.DB.Model(&models.ChannelCredential{}).Where(
		"organization_id = ?",
		fixture.organization.ID,
	).Count(&credentialCount).Error)
	assert.Zero(t, accountCount)
	assert.Zero(t, bindingCount)
	assert.Zero(t, credentialCount)
}

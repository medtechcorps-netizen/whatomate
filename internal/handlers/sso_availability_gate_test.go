package handlers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	appcrypto "github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

const optCUnavailable = "Single sign-on is temporarily unavailable."

// optCSetEnvironment sets the app's environment, which decides whether SSO is
// offered ("development" and "test" only).
func optCSetEnvironment(app *handlers.App, environment string) {
	app.Config.App.Environment = environment
}

// optCCountingTransport counts every outbound request the app makes to the
// identity provider.
type optCCountingTransport struct {
	inner http.RoundTripper
	calls atomic.Int64
}

func (c *optCCountingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	c.calls.Add(1)
	return c.inner.RoundTrip(request)
}

// optCCountProviderCalls wraps the app's provider client (installed by
// createCustomSSOProvider) so a test can assert that no call was made.
func optCCountProviderCalls(app *handlers.App) *optCCountingTransport {
	counter := &optCCountingTransport{inner: app.HTTPClient.Transport}
	app.HTTPClient = &http.Client{Transport: counter}
	return counter
}

// optCKeys lists the Redis keys matching pattern, sorted.
func optCKeys(t *testing.T, app *handlers.App, pattern string) []string {
	t.Helper()
	ctx := context.Background()
	var keys []string
	iter := app.Redis.Scan(ctx, 0, pattern, 100).Iterator()
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	require.NoError(t, iter.Err())
	sort.Strings(keys)
	return keys
}

func optCKeyExists(t *testing.T, app *handlers.App, key string) bool {
	t.Helper()
	n, err := app.Redis.Exists(context.Background(), key).Result()
	require.NoError(t, err)
	return n == 1
}

// optCIssueState stores a valid SSO state, as InitSSO would have before the
// gate applied.
func optCIssueState(t *testing.T, app *handlers.App, orgID uuid.UUID) string {
	t.Helper()
	nonce := "optc-" + uuid.NewString()
	state := handlers.SSOState{
		OrgID: orgID.String(), Provider: "custom", Nonce: nonce, ExpiresAt: time.Now().Add(5 * time.Minute),
	}
	stored, err := json.Marshal(state)
	require.NoError(t, err)
	require.NoError(t, app.Redis.Set(context.Background(), "sso:state:"+nonce, stored, 5*time.Minute).Err())
	return nonce
}

func optCCallback(t *testing.T, app *handlers.App, nonce string) *fastglue.Request {
	t.Helper()
	req := testutil.NewGETRequest(t)
	testutil.SetPathParam(req, "provider", "custom")
	testutil.SetQueryParam(req, "code", "auth-code-optc")
	testutil.SetQueryParam(req, "state", nonce)
	require.NoError(t, app.CallbackSSO(req))
	return req
}

// optCAssertUnavailableRedirect asserts the callback sent the browser to the
// login page with the generic SSO error.
func optCAssertUnavailableRedirect(t *testing.T, req *fastglue.Request) {
	t.Helper()
	require.Equal(t, fasthttp.StatusTemporaryRedirect, testutil.GetResponseStatusCode(req))
	location, err := url.Parse(string(req.RequestCtx.Response.Header.Peek("Location")))
	require.NoError(t, err)
	assert.Equal(t, "/login", location.Path)
	assert.Equal(t, optCUnavailable, location.Query().Get("sso_error"))
	assert.Empty(t, location.Query().Get("code"))
}

func TestOptC_PublicProviderListIsEmptyWhereSSOIsUnavailable(t *testing.T) {
	app := newSSOApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	createCustomSSOProvider(t, app, org.ID, newFakeOAuth(t))

	list := func() (*fastglue.Request, []handlers.SSOProviderPublic) {
		req := testutil.NewGETRequest(t)
		testutil.SetQueryParam(req, "organization", org.Slug)
		require.NoError(t, app.GetPublicSSOProviders(req))
		require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
		var providers []handlers.SSOProviderPublic
		testutil.ParseEnvelopeResponse(t, req, &providers)
		return req, providers
	}

	_, available := list()
	require.Len(t, available, 1, "the enabled provider is listed where SSO is available")

	optCSetEnvironment(app, "production")
	req, providers := list()
	assert.Empty(t, providers)
	assert.Contains(t, string(testutil.GetResponseBody(req)), `"data":[]`, "an empty list, not null")
	assert.Equal(t, "no-store", string(req.RequestCtx.Response.Header.Peek("Cache-Control")))
}

func TestOptC_InitSSOStartsNothingWhereSSOIsUnavailable(t *testing.T) {
	app := newSSOApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	createCustomSSOProvider(t, app, org.ID, newFakeOAuth(t))
	counter := optCCountProviderCalls(app)

	initSSO := func(provider string) *fastglue.Request {
		req := testutil.NewGETRequest(t)
		testutil.SetPathParam(req, "provider", provider)
		testutil.SetQueryParam(req, "organization", org.Slug)
		require.NoError(t, app.InitSSO(req))
		return req
	}

	optCSetEnvironment(app, "production")
	before := optCKeys(t, app, "sso:state:*")
	for _, provider := range []string{"custom", "google", "microsoft", "github", "facebook", "not-a-provider"} {
		req := initSSO(provider)
		testutil.AssertErrorResponse(t, req, fasthttp.StatusServiceUnavailable, optCUnavailable)
		assert.Empty(t, req.RequestCtx.Response.Header.Peek("Location"), provider)
	}
	assert.Equal(t, before, optCKeys(t, app, "sso:state:*"), "no SSO state was stored")
	assert.Zero(t, counter.calls.Load(), "the provider was not contacted")

	// The same request starts a sign-in where SSO is available.
	optCSetEnvironment(app, "test")
	started := initSSO("custom")
	require.Equal(t, fasthttp.StatusTemporaryRedirect, testutil.GetResponseStatusCode(started))
	assert.Len(t, optCKeys(t, app, "sso:state:*"), len(before)+1)
}

func TestOptC_CallbackCompletesNoIssuedStateWhereSSOIsUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name     string
		existing bool
	}{
		{name: "existing user", existing: true},
		{name: "auto-create", existing: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := newSSOApp(t)
			fake := newFakeOAuth(t)
			org := testutil.CreateTestOrganization(t, app.DB)
			createCustomSSOProvider(t, app, org.ID, fake) // enabled, auto-create on
			counter := optCCountProviderCalls(app)

			email := testutil.UniqueEmail("optc-callback")
			fake.UserEmail = email
			var user models.User
			if tc.existing {
				created := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(email))
				// Read back, so later comparisons use the stored precision.
				require.NoError(t, app.DB.First(&user, "id = ?", created.ID).Error)
			}
			countUsers := func() int64 {
				var n int64
				require.NoError(t, app.DB.Model(&models.User{}).Where("email = ?", email).Count(&n).Error)
				return n
			}
			countMemberships := func() int64 {
				var n int64
				require.NoError(t, app.DB.Model(&models.UserOrganization{}).
					Where("organization_id = ?", org.ID).Count(&n).Error)
				return n
			}
			usersBefore, membershipsBefore := countUsers(), countMemberships()
			grantsBefore := optCKeys(t, app, "sso:exchange:*")
			nonce := optCIssueState(t, app, org.ID)

			optCSetEnvironment(app, "staging")
			req := optCCallback(t, app, nonce)

			optCAssertUnavailableRedirect(t, req)
			assert.True(t, optCKeyExists(t, app, "sso:state:"+nonce), "the issued state was not redeemed")
			assert.Zero(t, counter.calls.Load(), "the provider was not contacted")
			assert.Equal(t, usersBefore, countUsers(), "no user was created")
			assert.Equal(t, membershipsBefore, countMemberships(), "no membership was created")
			assert.Equal(t, grantsBefore, optCKeys(t, app, "sso:exchange:*"), "no exchange grant was stored")
			laneAAssertNoBindingTouched(t, req)
			assert.Empty(t, laneASessionCookies(req), "no session cookie was set")
			if tc.existing {
				var stored models.User
				require.NoError(t, app.DB.First(&stored, "id = ?", user.ID).Error)
				assert.Empty(t, stored.SSOProvider, "no SSO identity was written")
				assert.Empty(t, stored.SSOProviderID, "no SSO identity was written")
				assert.True(t, stored.UpdatedAt.Equal(user.UpdatedAt), "the user row was not written")
				laneAAssertNoGrantFor(t, app, user.ID)

				// The same state completes the sign-in where SSO is available.
				optCSetEnvironment(app, "test")
				done := optCCallback(t, app, nonce)
				require.Equal(t, fasthttp.StatusTemporaryRedirect, testutil.GetResponseStatusCode(done))
				location, err := url.Parse(string(done.RequestCtx.Response.Header.Peek("Location")))
				require.NoError(t, err)
				assert.NotEmpty(t, location.Query().Get("code"))
				assert.NotZero(t, counter.calls.Load(), "the counter sees provider calls")
			}
		})
	}
}

func TestOptC_ExchangeRedeemsNoEarlierGrantWhereSSOIsUnavailable(t *testing.T) {
	app := newSSOApp(t)
	attempt := laneASSOCallback(t, app) // granted while SSO was available
	grantKey := "sso:exchange:" + attempt.code
	require.True(t, optCKeyExists(t, app, grantKey))
	refreshBefore := optCKeys(t, app, "refresh:*")

	optCSetEnvironment(app, "production")
	req := laneASSOExchange(t, app, attempt.code, map[string]string{attempt.bindName: attempt.binding})
	testutil.AssertErrorResponse(t, req, fasthttp.StatusServiceUnavailable, optCUnavailable)
	assert.True(t, optCKeyExists(t, app, grantKey), "the grant was not redeemed")
	assert.Empty(t, laneASessionCookies(req), "no session cookie was set")
	laneAAssertNoBindingTouched(t, req)
	assert.Equal(t, refreshBefore, optCKeys(t, app, "refresh:*"), "no refresh token was issued")

	// The gate, not the grant, refused: the same grant exchanges where SSO is
	// available.
	optCSetEnvironment(app, "test")
	signedIn := laneASSOExchange(t, app, attempt.code, map[string]string{attempt.bindName: attempt.binding})
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(signedIn))
	assert.ElementsMatch(t, []string{"whm_access", "whm_refresh", "whm_csrf"}, laneASessionCookies(signedIn))
}

func TestOptC_EnablingAProviderIsRefusedWhereSSOIsUnavailable(t *testing.T) {
	app := newSSOApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	admin := createAdminUser(t, app, org.ID)
	optCSetEnvironment(app, "production")

	put := func(provider string, body map[string]any) *fastglue.Request {
		req := testutil.NewJSONRequest(t, body)
		testutil.SetAuthContext(req, org.ID, admin.ID)
		testutil.SetPathParam(req, "provider", provider)
		require.NoError(t, app.UpdateSSOProvider(req))
		return req
	}
	load := func(provider string) (models.SSOProvider, bool) {
		var row models.SSOProvider
		err := app.DB.Where("organization_id = ? AND provider = ?", org.ID, provider).First(&row).Error
		return row, err == nil
	}
	seed := func(provider string, enabled bool) {
		secret, err := appcrypto.Encrypt("seed-secret-"+provider, app.Config.App.EncryptionKey)
		require.NoError(t, err)
		row := models.SSOProvider{
			BaseModel: models.BaseModel{ID: uuid.New()}, OrganizationID: org.ID, Provider: provider,
			ClientID: "seed-id-" + provider, ClientSecret: secret, IsEnabled: enabled, DefaultRoleName: "agent",
		}
		require.NoError(t, app.DB.Create(&row).Error)
	}
	enable := map[string]any{"client_id": "new-id", "client_secret": "new-secret", "is_enabled": true}

	t.Run("create enabled", func(t *testing.T) {
		testutil.AssertErrorResponse(t, put("facebook", enable), fasthttp.StatusServiceUnavailable, optCUnavailable)
		_, exists := load("facebook")
		assert.False(t, exists, "no provider row was created")
	})
	for _, tc := range []struct {
		name     string
		provider string
		enabled  bool
	}{
		{name: "re-enable", provider: "google", enabled: false},
		{name: "already-enabled upsert", provider: "microsoft", enabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seed(tc.provider, tc.enabled)
			before, _ := load(tc.provider) // stored precision
			testutil.AssertErrorResponse(t, put(tc.provider, enable), fasthttp.StatusServiceUnavailable, optCUnavailable)
			after, exists := load(tc.provider)
			require.True(t, exists)
			assert.Equal(t, before.IsEnabled, after.IsEnabled)
			assert.Equal(t, before.ClientID, after.ClientID)
			assert.Equal(t, before.ClientSecret, after.ClientSecret, "the stored secret is unchanged")
			assert.True(t, before.UpdatedAt.Equal(after.UpdatedAt), "the row was not saved")
		})
	}
	t.Run("disabled update still saves", func(t *testing.T) {
		before, exists := load("google")
		require.True(t, exists)
		req := put("google", map[string]any{"client_id": "disabled-edit", "is_enabled": false})
		require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))
		after, _ := load("google")
		assert.Equal(t, "disabled-edit", after.ClientID)
		assert.False(t, after.IsEnabled)
		assert.Equal(t, before.ClientSecret, after.ClientSecret)
	})
	t.Run("disabled validation answers as before", func(t *testing.T) {
		testutil.AssertErrorResponse(t, put("google", map[string]any{"client_id": " ", "is_enabled": false}),
			fasthttp.StatusBadRequest, "client_id is required")
		testutil.AssertErrorResponse(t, put("custom", map[string]any{"client_id": "x", "is_enabled": false}),
			fasthttp.StatusBadRequest, "auth_url, token_url, and user_info_url")
		testutil.AssertErrorResponse(t, put("not-a-provider", map[string]any{"client_id": "x", "is_enabled": true}),
			fasthttp.StatusBadRequest, "Invalid provider")
	})
	t.Run("settings read and delete still work", func(t *testing.T) {
		read := testutil.NewGETRequest(t)
		testutil.SetAuthContext(read, org.ID, admin.ID)
		require.NoError(t, app.GetSSOSettings(read))
		require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(read))
		var settings []handlers.SSOProviderResponse
		testutil.ParseEnvelopeResponse(t, read, &settings)
		assert.Len(t, settings, 2)

		del := testutil.NewJSONRequest(t, nil)
		testutil.SetAuthContext(del, org.ID, admin.ID)
		testutil.SetPathParam(del, "provider", "microsoft")
		require.NoError(t, app.DeleteSSOProvider(del))
		require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(del))
		_, exists := load("microsoft")
		assert.False(t, exists)
	})
}

// Password sign-in and the session routes are untouched where SSO is
// unavailable: login, refresh with rotation, revoke and logout.
func TestOptC_PasswordSessionsWorkWhereSSOIsUnavailable(t *testing.T) {
	app := newSSOApp(t)
	optCSetEnvironment(app, "production")
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("optc-password")))

	login := laneALogin(t, app, user.Email)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(login), string(testutil.GetResponseBody(login)))
	firstRefresh := testutil.GetResponseCookie(login, "whm_refresh")
	require.NotEmpty(t, firstRefresh)

	refreshed := laneARefresh(t, app, firstRefresh)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(refreshed))
	rotated := testutil.GetResponseCookie(refreshed, "whm_refresh")
	require.NotEmpty(t, rotated)
	assert.NotEqual(t, firstRefresh, rotated, "the refresh token rotates")

	csrf := testutil.GetResponseCookie(refreshed, "whm_csrf")
	revoked := laneARevoke(t, app, rotated, csrf, csrf)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(revoked), string(testutil.GetResponseBody(revoked)))
	afterRevoke := laneARefresh(t, app, rotated)
	assert.Equal(t, fasthttp.StatusUnauthorized, testutil.GetResponseStatusCode(afterRevoke))

	second := laneALogin(t, app, user.Email)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(second))
	logout := testutil.NewJSONRequest(t, map[string]string{})
	logout.RequestCtx.Request.Header.SetCookie("whm_refresh", testutil.GetResponseCookie(second, "whm_refresh"))
	require.NoError(t, app.Logout(logout))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(logout))
}

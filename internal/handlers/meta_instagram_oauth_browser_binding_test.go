package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
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

type metaInstagramOAuthStartResponse struct {
	AuthorizationURL string `json:"authorization_url"`
}

func startMetaInstagramOAuthBrowserBinding(
	t *testing.T,
	fixture metaInstagramLifecycleFixture,
) (string, string, string, *fastHTTPResponseCookie) {
	t.Helper()
	request := testutil.NewJSONRequest(t, map[string]any{})
	testutil.SetAuthContext(request, fixture.org.ID, fixture.user.ID)
	testutil.SetHeader(request, "X-Organization-ID", fixture.org.ID.String())
	require.NoError(t, fixture.app.StartMetaInstagramOnboarding(request))
	require.Equal(t, fasthttp.StatusOK, request.RequestCtx.Response.StatusCode())
	var response metaInstagramOAuthStartResponse
	testutil.ParseEnvelopeResponse(t, request, &response)
	providerURL, err := url.Parse(response.AuthorizationURL)
	require.NoError(t, err)
	nonce := providerURL.Query().Get("state")
	require.NotEmpty(t, nonce)
	cookieName := metaInstagramOAuthBrowserCookieName(nonce)
	verifier := testutil.GetResponseCookie(request, cookieName)
	require.Len(t, verifier, metaInstagramOAuthBrowserSecretSize)
	cookie := readFastHTTPResponseCookie(t, request, cookieName)
	return nonce, verifier, response.AuthorizationURL, cookie
}

// fastHTTPResponseCookie is a copyable test projection. fasthttp.Cookie itself
// must not be copied after parsing.
type fastHTTPResponseCookie struct {
	HTTPOnly bool
	Secure   bool
	SameSite fasthttp.CookieSameSite
	Path     string
	MaxAge   int
}

func readFastHTTPResponseCookie(
	t *testing.T,
	request *fastglue.Request,
	name string,
) *fastHTTPResponseCookie {
	t.Helper()
	var result *fastHTTPResponseCookie
	request.RequestCtx.Response.Header.VisitAllCookie(func(_, value []byte) {
		cookie := fasthttp.AcquireCookie()
		defer fasthttp.ReleaseCookie(cookie)
		if cookie.ParseBytes(value) != nil || string(cookie.Key()) != name {
			return
		}
		result = &fastHTTPResponseCookie{
			HTTPOnly: cookie.HTTPOnly(), Secure: cookie.Secure(), SameSite: cookie.SameSite(),
			Path: string(cookie.Path()), MaxAge: cookie.MaxAge(),
		}
	})
	require.NotNil(t, result)
	return result
}

func TestManagedInstagramOAuthStartSetsBoundBrowserOnlyProof(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Redis = testutil.SetupTestRedis(t)
	require.NotNil(t, fixture.app.Redis)

	nonce, verifier, authorizationURL, cookie := startMetaInstagramOAuthBrowserBinding(t, fixture)
	assert.True(t, cookie.HTTPOnly)
	assert.True(t, cookie.Secure)
	assert.Equal(t, fasthttp.CookieSameSiteLaxMode, cookie.SameSite)
	assert.Equal(t, "/", cookie.Path)
	assert.Equal(t, int(metaInstagramOAuthStateTTL/time.Second), cookie.MaxAge)

	stateJSON, err := fixture.app.Redis.Get(
		t.Context(), metaInstagramOAuthStateKey(nonce),
	).Bytes()
	require.NoError(t, err)
	var state metaInstagramOAuthState
	require.NoError(t, json.Unmarshal(stateJSON, &state))
	assert.Equal(t, fixture.org.ID.String(), state.OrganizationID)
	assert.Equal(t, fixture.user.ID.String(), state.UserID)
	assert.Equal(t, nonce, state.Nonce)
	assert.Equal(
		t, fixture.app.metaInstagramRuntimeFingerprint(fixture.app.Config.MetaInstagram),
		state.ConfigFingerprint,
	)
	assert.Equal(t, metaInstagramOAuthBrowserBindingDigest(state, verifier), state.BrowserBindingDigest)
	assert.NotContains(t, string(stateJSON), verifier, "Redis stores only the bound digest")

	// The only browser-correlating value sent through Meta is the public state
	// nonce. The independent verifier stays in the initiator's HttpOnly cookie.
	assert.NotContains(t, authorizationURL, verifier)
}

func TestManagedInstagramOAuthBrowserBindingRejectsMissingWrongAndSwitchedStateBeforeSideEffects(t *testing.T) {
	for _, test := range []struct {
		name          string
		cookie        func(string) string
		mutateState   func(*metaInstagramOAuthState)
		mutateRuntime func(*metaInstagramLifecycleFixture)
	}{
		{name: "missing cookie", cookie: func(string) string { return "" }},
		{name: "wrong browser", cookie: func(string) string {
			return generateRandomString(metaInstagramOAuthBrowserSecretSize)
		}},
		{name: "organization switched", cookie: func(value string) string { return value }, mutateState: func(state *metaInstagramOAuthState) {
			state.OrganizationID = uuid.NewString()
		}},
		{name: "user switched", cookie: func(value string) string { return value }, mutateState: func(state *metaInstagramOAuthState) {
			state.UserID = uuid.NewString()
		}},
		{name: "stored config switched", cookie: func(value string) string { return value }, mutateState: func(state *metaInstagramOAuthState) {
			state.ConfigFingerprint = strings.Repeat("f", 64)
		}},
		{name: "runtime config switched", cookie: func(value string) string { return value }, mutateRuntime: func(fixture *metaInstagramLifecycleFixture) {
			fixture.app.Config.MetaInstagram.RelayBaseURL = "https://changed-relay.example.test"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newMetaInstagramLifecycleFixture(t)
			fixture.app.Redis = testutil.SetupTestRedis(t)
			require.NotNil(t, fixture.app.Redis)
			nonce, verifier, _, _ := startMetaInstagramOAuthBrowserBinding(t, fixture)
			if test.mutateState != nil {
				stateJSON, err := fixture.app.Redis.Get(
					t.Context(), metaInstagramOAuthStateKey(nonce),
				).Bytes()
				require.NoError(t, err)
				var state metaInstagramOAuthState
				require.NoError(t, json.Unmarshal(stateJSON, &state))
				test.mutateState(&state)
				stateJSON, err = json.Marshal(state)
				require.NoError(t, err)
				require.NoError(t, fixture.app.Redis.Set(
					t.Context(), metaInstagramOAuthStateKey(nonce), stateJSON,
					metaInstagramOAuthStateTTL,
				).Err())
			}
			if test.mutateRuntime != nil {
				test.mutateRuntime(&fixture)
			}
			var providerCalls atomic.Int32
			fixture.app.HTTPClient = &http.Client{Transport: metaInstagramRoundTripFunc(
				func(*http.Request) (*http.Response, error) {
					providerCalls.Add(1)
					return nil, errors.New("browser binding failure reached provider")
				},
			)}
			var accountsBefore, credentialsBefore int64
			require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).Count(&accountsBefore).Error)
			require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).Count(&credentialsBefore).Error)

			callback := testutil.NewRequest(t)
			callback.RequestCtx.QueryArgs().Set("state", nonce)
			callback.RequestCtx.QueryArgs().Set("code", "synthetic-authorization-code")
			if cookie := test.cookie(verifier); cookie != "" {
				callback.RequestCtx.Request.Header.SetCookie(
					metaInstagramOAuthBrowserCookieName(nonce), cookie,
				)
			}
			require.NoError(t, fixture.app.CallbackMetaInstagram(callback))
			assert.Equal(t, fasthttp.StatusSeeOther, callback.RequestCtx.Response.StatusCode())
			cleared := readFastHTTPResponseCookie(
				t, callback, metaInstagramOAuthBrowserCookieName(nonce),
			)
			assert.LessOrEqual(t, cleared.MaxAge, 0, "terminal callback expires the browser verifier")
			assert.Zero(t, providerCalls.Load())
			var accountsAfter, credentialsAfter int64
			require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).Count(&accountsAfter).Error)
			require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).Count(&credentialsAfter).Error)
			assert.Equal(t, accountsBefore, accountsAfter)
			assert.Equal(t, credentialsBefore, credentialsAfter)
		})
	}
}

func TestManagedInstagramOAuthExactBrowserBindingProceedsOnceAndReplayStopsBeforeProvider(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Redis = testutil.SetupTestRedis(t)
	require.NotNil(t, fixture.app.Redis)
	nonce, verifier, _, _ := startMetaInstagramOAuthBrowserBinding(t, fixture)

	var providerCalls atomic.Int32
	fixture.app.HTTPClient = &http.Client{Transport: metaInstagramRoundTripFunc(
		func(request *http.Request) (*http.Response, error) {
			providerCalls.Add(1)
			assert.Equal(t, http.MethodPost, request.Method)
			assert.Equal(t, "/oauth/access_token", request.URL.Path)
			return nil, errors.New("synthetic provider stop after browser proof")
		},
	)}
	callback := func() *fasthttp.RequestCtx {
		request := testutil.NewRequest(t)
		request.RequestCtx.QueryArgs().Set("state", nonce)
		request.RequestCtx.QueryArgs().Set("code", "synthetic-authorization-code")
		request.RequestCtx.Request.Header.SetCookie(
			metaInstagramOAuthBrowserCookieName(nonce), verifier,
		)
		require.NoError(t, fixture.app.CallbackMetaInstagram(request))
		assert.Equal(t, fasthttp.StatusSeeOther, request.RequestCtx.Response.StatusCode())
		cleared := readFastHTTPResponseCookie(
			t, request, metaInstagramOAuthBrowserCookieName(nonce),
		)
		assert.LessOrEqual(t, cleared.MaxAge, 0, "terminal callback expires the browser verifier")
		return request.RequestCtx
	}
	_ = callback()
	assert.Equal(t, int32(1), providerCalls.Load(), "exact proof reaches the code exchange")
	assert.Zero(t, fixture.app.Redis.Exists(
		t.Context(), metaInstagramOAuthStateKey(nonce),
	).Val(), "exact proof atomically consumes the state")

	_ = callback()
	assert.Equal(t, int32(1), providerCalls.Load(), "replay fails before a second provider call")
}

func TestManagedInstagramOAuthPersistsDistinctSubjectAndProfessionalIdentity(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	const oauthSubjectID = "800000000000271"
	const professionalAccountID = "700000000000271"
	fixture.app.Config.App.Environment = "development"
	fixture.app.Config.MetaInstagram.AppReviewStatus = "not_submitted"
	fixture.app.Config.MetaInstagram.DevelopmentTestOAuthSubjectID = oauthSubjectID
	fixture.app.Config.MetaInstagram.DevelopmentTestProfileID = professionalAccountID
	fixture.app.Config.MetaInstagram.DevelopmentAppRole = "tester"
	fixture.app.Redis = testutil.SetupTestRedis(t)
	require.NotNil(t, fixture.app.Redis)
	nonce, verifier, _, _ := startMetaInstagramOAuthBrowserBinding(t, fixture)
	expiresAt := time.Now().UTC().Add(2 * time.Hour).Unix()
	var providerCalls atomic.Int32
	fixture.app.HTTPClient = &http.Client{Transport: metaInstagramRoundTripFunc(
		func(request *http.Request) (*http.Response, error) {
			providerCalls.Add(1)
			body := ""
			switch {
			case request.Method == http.MethodPost && request.URL.Path == "/oauth/access_token":
				body = `{"data":[{"access_token":"synthetic-short-token","user_id":"` + oauthSubjectID + `","permissions":"instagram_business_basic,instagram_business_manage_messages"}]}`
			case request.Method == http.MethodGet && request.URL.Path == "/access_token":
				body = `{"access_token":"synthetic-long-token","token_type":"bearer","expires_in":7200}`
			case request.Method == http.MethodGet && request.URL.Path == "/debug_token":
				body = `{"data":{"app_id":"` + metaInstagramTestAppID + `","is_valid":true,"user_id":"` + oauthSubjectID + `","scopes":["instagram_business_basic","instagram_business_manage_messages"],"expires_at":` + strconv.FormatInt(expiresAt, 10) + `}}`
			case request.Method == http.MethodGet && request.URL.Path == "/v25.0/me":
				body = `{"data":[{"id":"` + oauthSubjectID + `","user_id":"` + professionalAccountID + `","username":"synthetic_distinct_identity","account_type":"MEDIA_CREATOR"}]}`
			case request.Method == http.MethodPost && request.URL.Path == "/v25.0/"+professionalAccountID+"/subscribed_apps":
				body = `{"success":true}`
			case request.Method == http.MethodGet && request.URL.Path == "/v25.0/"+professionalAccountID+"/subscribed_apps":
				body = `{"data":[{"id":"` + metaInstagramTestAppID + `","subscribed_fields":["messages"]}]}`
			default:
				return nil, errors.New("unexpected Instagram OAuth provider call")
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    request,
			}, nil
		},
	)}

	callback := testutil.NewRequest(t)
	callback.RequestCtx.QueryArgs().Set("state", nonce)
	callback.RequestCtx.QueryArgs().Set("code", "synthetic-authorization-code")
	callback.RequestCtx.Request.Header.SetCookie(
		metaInstagramOAuthBrowserCookieName(nonce), verifier,
	)
	require.NoError(t, fixture.app.CallbackMetaInstagram(callback))
	assert.Equal(t, fasthttp.StatusSeeOther, callback.RequestCtx.Response.StatusCode())
	assert.Contains(t, string(callback.RequestCtx.Response.Header.Peek("Location")), "managed_instagram=pending")
	assert.Equal(t, int32(6), providerCalls.Load())

	var account models.ChannelAccount
	require.NoError(t, fixture.db.Where(
		"organization_id = ? AND channel = ? AND external_account_id = ?",
		fixture.org.ID, models.ChannelInstagram, professionalAccountID,
	).First(&account).Error)
	assert.Equal(t, professionalAccountID, account.ExternalAccountID)
	assert.Equal(t, professionalAccountID, stringConfigValue(account.Metadata, "meta_authority_asset_id"))
	assert.Equal(t, oauthSubjectID, stringConfigValue(account.Metadata, "meta_authorizing_user_id"))
	assert.Equal(t, oauthSubjectID, stringConfigValue(account.Metadata, metaInstagramOAuthSubjectIDKey))
	assert.Equal(t, professionalAccountID, stringConfigValue(account.Metadata, "meta_release_profile_id"))
	assert.Equal(t, oauthSubjectID, stringConfigValue(account.Metadata, "meta_release_oauth_subject_id"))
	assert.NotEqual(t, account.ExternalAccountID, stringConfigValue(account.Metadata, "meta_authorizing_user_id"))
	assert.Equal(t, models.ChannelAccountStatusPending, account.Status)
	assert.True(t, exactManagedInstagramCallbackBinding(
		&account, metaInstagramTestAppID, oauthSubjectID,
	))
	assert.False(t, exactManagedInstagramCallbackBinding(
		&account, metaInstagramTestAppID, professionalAccountID,
	))

	binding, err := fixture.app.scopedApp(fixture.db, fixture.org.ID).loadMetaRegistryBinding(
		metaregistry.ResolveRequest{
			Channel: models.ChannelInstagram, ExternalAccountID: professionalAccountID,
			Purpose: metaregistry.ResolvePurposeHealth,
		}, time.Now().UTC(),
	)
	require.NoError(t, err)
	assert.Equal(t, professionalAccountID, binding.ExternalAccountID)
}

func TestManagedInstagramDevelopmentOAuthRejectsWrongProfessionalProfileBeforeSubscription(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	const oauthSubjectID = "800000000000272"
	const expectedProfessionalID = "700000000000272"
	const wrongProfessionalID = "700000000000273"
	fixture.app.Config.App.Environment = "development"
	fixture.app.Config.MetaInstagram.AppReviewStatus = "not_submitted"
	fixture.app.Config.MetaInstagram.DevelopmentTestOAuthSubjectID = oauthSubjectID
	fixture.app.Config.MetaInstagram.DevelopmentTestProfileID = expectedProfessionalID
	fixture.app.Config.MetaInstagram.DevelopmentAppRole = "tester"
	fixture.app.Redis = testutil.SetupTestRedis(t)
	require.NotNil(t, fixture.app.Redis)
	nonce, verifier, _, _ := startMetaInstagramOAuthBrowserBinding(t, fixture)
	expiresAt := time.Now().UTC().Add(2 * time.Hour).Unix()
	var providerCalls atomic.Int32
	var subscriptionCalls atomic.Int32
	fixture.app.HTTPClient = &http.Client{Transport: metaInstagramRoundTripFunc(
		func(request *http.Request) (*http.Response, error) {
			providerCalls.Add(1)
			body := ""
			switch request.URL.Path {
			case "/oauth/access_token":
				body = `{"data":[{"access_token":"synthetic-short-token","user_id":"` + oauthSubjectID + `","permissions":"instagram_business_basic,instagram_business_manage_messages"}]}`
			case "/access_token":
				body = `{"access_token":"synthetic-long-token","expires_in":7200}`
			case "/debug_token":
				body = `{"data":{"app_id":"` + metaInstagramTestAppID + `","is_valid":true,"user_id":"` + oauthSubjectID + `","scopes":["instagram_business_basic","instagram_business_manage_messages"],"expires_at":` + strconv.FormatInt(expiresAt, 10) + `}}`
			case "/v25.0/me":
				body = `{"data":[{"id":"` + oauthSubjectID + `","user_id":"` + wrongProfessionalID + `","username":"synthetic_wrong_professional","account_type":"BUSINESS"}]}`
			default:
				subscriptionCalls.Add(1)
				return nil, errors.New("professional mismatch reached subscription")
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)), Request: request,
			}, nil
		},
	)}
	var accountsBefore int64
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).Count(&accountsBefore).Error)
	callback := testutil.NewRequest(t)
	callback.RequestCtx.QueryArgs().Set("state", nonce)
	callback.RequestCtx.QueryArgs().Set("code", "synthetic-authorization-code")
	callback.RequestCtx.Request.Header.SetCookie(
		metaInstagramOAuthBrowserCookieName(nonce), verifier,
	)
	require.NoError(t, fixture.app.CallbackMetaInstagram(callback))
	assert.Equal(t, fasthttp.StatusSeeOther, callback.RequestCtx.Response.StatusCode())
	assert.Contains(t, string(callback.RequestCtx.Response.Header.Peek("Location")), "managed_instagram=error")
	assert.Equal(t, int32(4), providerCalls.Load())
	assert.Zero(t, subscriptionCalls.Load())
	var accountsAfter int64
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).Count(&accountsAfter).Error)
	assert.Equal(t, accountsBefore, accountsAfter)
}

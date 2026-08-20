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
	channelapi "github.com/shridarpatil/whatomate/internal/channel"
	"github.com/shridarpatil/whatomate/internal/metaregistry"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

type metaInstagramOAuthStartResponse struct {
	AuthorizationURL string `json:"authorization_url"`
}

func setMetaInstagramOmnichannelEntitlement(
	t *testing.T,
	fixture metaInstagramLifecycleFixture,
	enabled bool,
) {
	t.Helper()
	var subscription models.Subscription
	require.NoError(t, fixture.db.Where(
		"organization_id = ? AND status = ?",
		fixture.org.ID, models.SubscriptionStatusActive,
	).First(&subscription).Error)
	snapshot := cloneJSONB(subscription.EntitlementsSnapshot)
	snapshot[channelapi.OmnichannelEntitlementKey] = enabled
	require.NoError(t, fixture.db.Model(&models.Subscription{}).Where(
		"id = ? AND organization_id = ?", subscription.ID, fixture.org.ID,
	).Update("entitlements_snapshot", snapshot).Error)
}

func installSuccessfulMetaInstagramUnsubscribe(
	t *testing.T,
	fixture metaInstagramLifecycleFixture,
) *atomic.Int32 {
	t.Helper()
	calls := &atomic.Int32{}
	fixture.app.HTTPClient = &http.Client{Transport: metaInstagramRoundTripFunc(
		func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			body := ""
			switch request.Method {
			case http.MethodDelete:
				body = `{"success":true}`
			case http.MethodGet:
				body = `{"data":[]}`
			default:
				return nil, errors.New("unexpected entitlement cleanup provider method")
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    request,
			}, nil
		},
	)}
	return calls
}

func assertMetaInstagramEntitlementCleanupDisconnected(
	t *testing.T,
	fixture metaInstagramLifecycleFixture,
	accountID uuid.UUID,
) {
	t.Helper()
	var account models.ChannelAccount
	require.NoError(t, fixture.db.Preload("Credentials").First(
		&account, "id = ?", accountID,
	).Error)
	assert.Equal(t, models.ChannelAccountStatusDisconnected, account.Status)
	assert.False(t, boolConfigValue(account.Config, "outbound_enabled"))
	assert.False(t, boolConfigValue(account.Config, "ai_reply_enabled"))
	assert.False(t, account.IsDefaultIncoming)
	assert.False(t, account.IsDefaultOutgoing)
	assert.Nil(t, account.ConnectedAt)
	assert.Equal(
		t, metaMessengerSubscriptionDesiredUnsubscribed,
		stringConfigValue(account.Metadata, metaMessengerSubscriptionDesiredStateKey),
	)
	assert.Equal(
		t, metaMessengerSubscriptionUnsubscribeConfirmed,
		stringConfigValue(account.Metadata, metaMessengerSubscriptionOperationStateKey),
	)
	assert.Equal(
		t, metaMessengerSubscriptionRemoteUnsubscribed,
		stringConfigValue(account.Metadata, metaMessengerSubscriptionRemoteStateKey),
	)
	assert.Empty(t, stringConfigValue(account.Metadata, metaMessengerSubscriptionFencedOperationIDKey))
	for index := range account.Credentials {
		assert.NotContains(
			t,
			[]models.ChannelCredentialStatus{
				models.ChannelCredentialStatusActive,
				models.ChannelCredentialStatusExpiring,
			},
			account.Credentials[index].Status,
		)
	}
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
		t, fixture.app.metaInstagramRuntimeFingerprint(
			fixture.app.Config.MetaInstagram, fixture.org.ID,
		),
		state.ConfigFingerprint,
	)
	assert.Equal(t, metaInstagramOAuthBrowserBindingDigest(state, verifier), state.BrowserBindingDigest)
	assert.NotContains(t, string(stateJSON), verifier, "Redis stores only the bound digest")

	// The only browser-correlating value sent through Meta is the public state
	// nonce. The independent verifier stays in the initiator's HttpOnly cookie.
	assert.NotContains(t, authorizationURL, verifier)
}

func TestManagedInstagramOAuthStartRequiresOmnichannelEntitlement(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Redis = testutil.SetupTestRedis(t)
	require.NotNil(t, fixture.app.Redis)
	setMetaInstagramOmnichannelEntitlement(t, fixture, false)

	request := testutil.NewJSONRequest(t, map[string]any{})
	testutil.SetAuthContext(request, fixture.org.ID, fixture.user.ID)
	testutil.SetHeader(request, "X-Organization-ID", fixture.org.ID.String())
	require.NoError(t, fixture.app.StartMetaInstagramOnboarding(request))
	testutil.AssertErrorResponse(
		t, request, fasthttp.StatusPaymentRequired,
		"Feature is not included",
	)
	direct := testutil.NewJSONRequest(t, map[string]any{})
	require.NoError(t, fixture.app.beginMetaInstagramOnboarding(
		direct, fixture.org.ID, fixture.user.ID, uuid.Nil,
	))
	testutil.AssertErrorResponse(
		t, direct, fasthttp.StatusPaymentRequired,
		"Instagram messaging is not included",
	)
	var oauthCookieCount int
	direct.RequestCtx.Response.Header.VisitAllCookie(func(key, _ []byte) {
		if strings.HasPrefix(string(key), metaInstagramOAuthBrowserCookieBase) {
			oauthCookieCount++
		}
	})
	assert.Zero(t, oauthCookieCount, "denied onboarding must not create a browser-bound OAuth state")
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

func TestManagedInstagramOAuthCallbackEntitlementRevocationStopsBeforeGraph(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Redis = testutil.SetupTestRedis(t)
	require.NotNil(t, fixture.app.Redis)
	nonce, verifier, _, _ := startMetaInstagramOAuthBrowserBinding(t, fixture)
	setMetaInstagramOmnichannelEntitlement(t, fixture, false)

	var providerCalls atomic.Int32
	fixture.app.HTTPClient = &http.Client{Transport: metaInstagramRoundTripFunc(
		func(*http.Request) (*http.Response, error) {
			providerCalls.Add(1)
			return nil, errors.New("revoked entitlement reached Instagram")
		},
	)}
	var accountsBefore, credentialsBefore int64
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).Count(&accountsBefore).Error)
	require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).Count(&credentialsBefore).Error)

	callback := testutil.NewRequest(t)
	callback.RequestCtx.QueryArgs().Set("state", nonce)
	callback.RequestCtx.QueryArgs().Set("code", "synthetic-authorization-code")
	callback.RequestCtx.Request.Header.SetCookie(
		metaInstagramOAuthBrowserCookieName(nonce), verifier,
	)
	require.NoError(t, fixture.app.CallbackMetaInstagram(callback))
	assert.Equal(t, fasthttp.StatusSeeOther, callback.RequestCtx.Response.StatusCode())
	assert.Contains(t, string(callback.RequestCtx.Response.Header.Peek("Location")), "managed_instagram=error")
	assert.Zero(t, providerCalls.Load())

	var accountsAfter, credentialsAfter int64
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).Count(&accountsAfter).Error)
	require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).Count(&credentialsAfter).Error)
	assert.Equal(t, accountsBefore, accountsAfter)
	assert.Equal(t, credentialsBefore, credentialsAfter)
}

func TestManagedInstagramOAuthCallbackCommittedEntitlementRevocationWinsBeforeGraph(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Redis = testutil.SetupTestRedis(t)
	require.NotNil(t, fixture.app.Redis)
	nonce, verifier, _, _ := startMetaInstagramOAuthBrowserBinding(t, fixture)

	var subscription models.Subscription
	require.NoError(t, fixture.db.Where(
		"organization_id = ? AND status = ?",
		fixture.org.ID, models.SubscriptionStatusActive,
	).First(&subscription).Error)
	revokedSnapshot := cloneJSONB(subscription.EntitlementsSnapshot)
	revokedSnapshot[channelapi.OmnichannelEntitlementKey] = false
	downgradeTx := fixture.db.Begin()
	require.NoError(t, downgradeTx.Error)
	committed := false
	t.Cleanup(func() {
		if !committed {
			_ = downgradeTx.Rollback().Error
		}
	})
	require.NoError(t, lockChannelAIOrganizationScopeTx(downgradeTx, fixture.org.ID))
	require.NoError(t, downgradeTx.Model(&models.Subscription{}).Where(
		"id = ? AND organization_id = ?", subscription.ID, fixture.org.ID,
	).Update("entitlements_snapshot", revokedSnapshot).Error)

	var providerCalls atomic.Int32
	fixture.app.HTTPClient = &http.Client{Transport: metaInstagramRoundTripFunc(
		func(*http.Request) (*http.Response, error) {
			providerCalls.Add(1)
			return nil, errors.New("committed entitlement revocation reached Instagram")
		},
	)}
	callback := testutil.NewRequest(t)
	callback.RequestCtx.QueryArgs().Set("state", nonce)
	callback.RequestCtx.QueryArgs().Set("code", "synthetic-authorization-code")
	callback.RequestCtx.Request.Header.SetCookie(
		metaInstagramOAuthBrowserCookieName(nonce), verifier,
	)
	callbackPID := make(chan int, 1)
	callbackDone := make(chan error, 1)
	go func() {
		callbackDone <- fixture.db.Connection(func(connection *gorm.DB) error {
			session := connection.Session(&gorm.Session{NewDB: true})
			var backendPID int
			if err := session.Raw("SELECT pg_backend_pid()").Scan(&backendPID).Error; err != nil {
				callbackPID <- 0
				return err
			}
			callbackPID <- backendPID
			callbackApp := &App{
				Config: fixture.app.Config, DB: session, Redis: fixture.app.Redis,
				Log: fixture.app.Log, HTTPClient: fixture.app.HTTPClient,
			}
			return callbackApp.CallbackMetaInstagram(callback)
		})
	}()
	backendPID := <-callbackPID
	require.Positive(t, backendPID)
	testutil.RequirePostgresBackendWaitingForLock(t, fixture.db, backendPID)
	require.NoError(t, downgradeTx.Commit().Error)
	committed = true
	select {
	case err := <-callbackDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "Instagram callback did not resume after entitlement downgrade")
	}
	assert.Equal(t, fasthttp.StatusSeeOther, callback.RequestCtx.Response.StatusCode())
	assert.Contains(t, string(callback.RequestCtx.Response.Header.Peek("Location")), "managed_instagram=error")
	assert.Zero(t, providerCalls.Load())
}

func TestManagedInstagramOAuthCallbackEntitlementRevocationWinsFinalPersistenceFence(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Redis = testutil.SetupTestRedis(t)
	require.NotNil(t, fixture.app.Redis)
	nonce, verifier, _, _ := startMetaInstagramOAuthBrowserBinding(t, fixture)

	const oauthSubjectID = "800000000000281"
	const professionalAccountID = "700000000000281"
	expiresAt := time.Now().UTC().Add(2 * time.Hour).Unix()
	var providerCalls atomic.Int32
	var subscriptionCalls atomic.Int32
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
				// Revoke after every identity Graph read succeeded but before the
				// callback may claim credentials or mutate the subscription.
				setMetaInstagramOmnichannelEntitlement(t, fixture, false)
				body = `{"data":[{"id":"` + oauthSubjectID + `","user_id":"` + professionalAccountID + `","username":"synthetic_revoked_during_oauth","account_type":"BUSINESS"}]}`
			default:
				subscriptionCalls.Add(1)
				return nil, errors.New("revoked entitlement reached Instagram subscription")
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    request,
			}, nil
		},
	)}
	var accountsBefore, credentialsBefore int64
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).Count(&accountsBefore).Error)
	require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).Count(&credentialsBefore).Error)

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

	var accountsAfter, credentialsAfter int64
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).Count(&accountsAfter).Error)
	require.NoError(t, fixture.db.Model(&models.ChannelCredential{}).Count(&credentialsAfter).Error)
	assert.Equal(t, accountsBefore, accountsAfter)
	assert.Equal(t, credentialsBefore, credentialsAfter)
}

func TestManagedInstagramSubscribeProviderAttemptRechecksEntitlementUnderOrganizationLock(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	const professionalAccountID = "700000000000282"
	var provisioned metaRegistryProvisionResult
	require.NoError(t, fixture.app.WithCommittedTenantApp(
		fixture.org.ID,
		func(scoped *App) error {
			var err error
			provisioned, err = scoped.provisionMetaRegistryBinding(
				syntheticMetaInstagramProvisionInput(
					fixture, professionalAccountID, uuid.New(),
				),
			)
			return err
		},
	))
	setMetaInstagramOmnichannelEntitlement(t, fixture, false)

	var providerCalls atomic.Int32
	err := fixture.app.withLockedMetaInstagramSubscriptionProviderAttempt(
		t.Context(), fixture.org.ID, provisioned.Account.ID,
		provisioned.SubscriptionOperation,
		metaMessengerSubscriptionSubscribePending,
		func(models.ChannelAccount, string) error {
			providerCalls.Add(1)
			return nil
		},
	)
	require.ErrorIs(t, err, errMetaInstagramEntitlementDenied)
	assert.Zero(t, providerCalls.Load())

	cleanupCalls := installSuccessfulMetaInstagramUnsubscribe(t, fixture)
	settings, settingsErr := fixture.app.metaInstagramOnboardingSettings()
	require.NoError(t, settingsErr)
	require.NoError(t, fixture.app.compensateMetaInstagramEntitlementLoss(
		t.Context(), settings, fixture.org.ID, fixture.user.ID,
		provisioned.Account.ID, provisioned.SubscriptionOperation,
	))
	assert.Equal(t, int32(2), cleanupCalls.Load(), "cleanup must DELETE and verify remote absence")
	assertMetaInstagramEntitlementCleanupDisconnected(
		t, fixture, provisioned.Account.ID,
	)
}

func TestManagedInstagramActivationCommittedEntitlementRevocationWinsFinalFence(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	healthCheckedAt := now.Add(-time.Minute)
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
	require.NoError(t, fixture.db.Model(&models.ChannelAccount{}).Where(
		"id = ? AND organization_id = ?", fixture.account.ID, fixture.org.ID,
	).Updates(map[string]any{
		"status": models.ChannelAccountStatusPending, "config": accountConfig,
		"metadata": metadata, "last_health_check_at": healthCheckedAt,
		"last_error": "", "last_error_at": nil, "connected_at": nil,
	}).Error)
	evidence := metaInstagramSubscriptionApproval{
		ProfileID: fixture.profileID, CheckedAt: now,
		OAuthCredentialID: fixture.oauth.ID, OAuthCredentialVersion: fixture.oauth.Version,
		WebhookCredentialID: fixture.webhook.ID, WebhookCredentialVersion: fixture.webhook.Version,
	}

	var subscription models.Subscription
	require.NoError(t, fixture.db.Where(
		"organization_id = ? AND status = ?",
		fixture.org.ID, models.SubscriptionStatusActive,
	).First(&subscription).Error)
	revokedSnapshot := cloneJSONB(subscription.EntitlementsSnapshot)
	revokedSnapshot[channelapi.OmnichannelEntitlementKey] = false
	downgradeTx := fixture.db.Begin()
	require.NoError(t, downgradeTx.Error)
	committed := false
	t.Cleanup(func() {
		if !committed {
			_ = downgradeTx.Rollback().Error
		}
	})
	require.NoError(t, lockChannelAIOrganizationScopeTx(downgradeTx, fixture.org.ID))
	require.NoError(t, downgradeTx.Model(&models.Subscription{}).Where(
		"id = ? AND organization_id = ?", subscription.ID, fixture.org.ID,
	).Update("entitlements_snapshot", revokedSnapshot).Error)

	activationPID := make(chan int, 1)
	activationDone := make(chan error, 1)
	go func() {
		activationDone <- fixture.db.Connection(func(connection *gorm.DB) error {
			session := connection.Session(&gorm.Session{NewDB: true})
			var backendPID int
			if err := session.Raw("SELECT pg_backend_pid()").Scan(&backendPID).Error; err != nil {
				activationPID <- 0
				return err
			}
			activationPID <- backendPID
			requestApp := &App{
				Config: fixture.app.Config, DB: session, Redis: fixture.app.Redis,
				Log: fixture.app.Log,
			}
			_, err := requestApp.activateMetaInstagramAccount(
				fixture.org.ID, fixture.user.ID, fixture.account.ID, now, evidence,
			)
			return err
		})
	}()
	backendPID := <-activationPID
	require.Positive(t, backendPID)
	testutil.RequirePostgresBackendWaitingForLock(t, fixture.db, backendPID)
	require.NoError(t, downgradeTx.Commit().Error)
	committed = true
	select {
	case err := <-activationDone:
		require.ErrorIs(t, err, errMetaInstagramEntitlementDenied)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "Instagram activation did not resume after entitlement downgrade")
	}

	var account models.ChannelAccount
	require.NoError(t, fixture.db.First(&account, "id = ?", fixture.account.ID).Error)
	assert.Equal(t, models.ChannelAccountStatusPending, account.Status)
	assert.False(t, boolConfigValue(account.Config, "outbound_enabled"))
	assert.False(t, boolConfigValue(account.Config, "ai_reply_enabled"))
	assert.Nil(t, account.ConnectedAt)
}

func TestManagedInstagramSubscribeFinalizationRechecksEntitlementAfterProviderAttempt(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	const professionalAccountID = "700000000000283"
	var provisioned metaRegistryProvisionResult
	require.NoError(t, fixture.app.WithCommittedTenantApp(
		fixture.org.ID,
		func(scoped *App) error {
			var err error
			provisioned, err = scoped.provisionMetaRegistryBinding(
				syntheticMetaInstagramProvisionInput(
					fixture, professionalAccountID, uuid.New(),
				),
			)
			return err
		},
	))

	var subscription models.Subscription
	require.NoError(t, fixture.db.Where(
		"organization_id = ? AND status = ?",
		fixture.org.ID, models.SubscriptionStatusActive,
	).First(&subscription).Error)
	revokedSnapshot := cloneJSONB(subscription.EntitlementsSnapshot)
	revokedSnapshot[channelapi.OmnichannelEntitlementKey] = false

	var providerCalls atomic.Int32
	providerStarted := make(chan struct{})
	releaseProvider := make(chan struct{})
	providerDone := make(chan error, 1)
	go func() {
		providerDone <- fixture.app.withLockedMetaInstagramSubscriptionProviderAttempt(
			t.Context(), fixture.org.ID, provisioned.Account.ID,
			provisioned.SubscriptionOperation,
			metaMessengerSubscriptionSubscribePending,
			func(models.ChannelAccount, string) error {
				providerCalls.Add(1)
				close(providerStarted)
				<-releaseProvider
				return nil
			},
		)
	}()
	select {
	case <-providerStarted:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "Instagram subscribe provider attempt did not start")
	}

	downgradePID := make(chan int, 1)
	downgradeDone := make(chan error, 1)
	go func() {
		downgradeDone <- fixture.db.Connection(func(connection *gorm.DB) error {
			return connection.Transaction(func(tx *gorm.DB) error {
				var backendPID int
				if err := tx.Raw("SELECT pg_backend_pid()").Scan(&backendPID).Error; err != nil {
					downgradePID <- 0
					return err
				}
				downgradePID <- backendPID
				if err := lockChannelAIOrganizationScopeTx(tx, fixture.org.ID); err != nil {
					return err
				}
				return tx.Model(&models.Subscription{}).Where(
					"id = ? AND organization_id = ?", subscription.ID, fixture.org.ID,
				).Update("entitlements_snapshot", revokedSnapshot).Error
			})
		})
	}()
	backendPID := <-downgradePID
	require.Positive(t, backendPID)
	testutil.RequirePostgresBackendWaitingForLock(t, fixture.db, backendPID)
	close(releaseProvider)
	select {
	case err := <-providerDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "Instagram subscribe provider attempt did not finish")
	}
	select {
	case err := <-downgradeDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "entitlement downgrade did not resume")
	}
	require.Equal(t, int32(1), providerCalls.Load())

	// The downgrade won after the allowed provider attempt but before the
	// callback's separate subscribe_complete persistence transaction.
	err := fixture.app.WithCommittedTenantApp(fixture.org.ID, func(scoped *App) error {
		_, finalizeErr := scoped.finalizeMetaInstagramSubscribeOperation(
			fixture.org.ID, fixture.user.ID, provisioned.Account.ID,
			provisioned.SubscriptionOperation, time.Now().UTC(),
		)
		return finalizeErr
	})
	require.ErrorIs(t, err, errMetaInstagramEntitlementDenied)

	var account models.ChannelAccount
	require.NoError(t, fixture.db.First(&account, "id = ?", provisioned.Account.ID).Error)
	assert.Equal(
		t,
		metaMessengerSubscriptionSubscribePending,
		stringConfigValue(account.Metadata, metaMessengerSubscriptionOperationStateKey),
	)
	assert.NotEqual(
		t,
		metaMessengerSubscriptionRemoteSubscribed,
		stringConfigValue(account.Metadata, metaMessengerSubscriptionRemoteStateKey),
	)

	cleanupCalls := installSuccessfulMetaInstagramUnsubscribe(t, fixture)
	settings, settingsErr := fixture.app.metaInstagramOnboardingSettings()
	require.NoError(t, settingsErr)
	require.NoError(t, fixture.app.compensateMetaInstagramEntitlementLoss(
		t.Context(), settings, fixture.org.ID, fixture.user.ID,
		provisioned.Account.ID, provisioned.SubscriptionOperation,
	))
	assert.Equal(t, int32(2), cleanupCalls.Load(), "cleanup must compensate the successful subscribe")
	assertMetaInstagramEntitlementCleanupDisconnected(
		t, fixture, provisioned.Account.ID,
	)
}

func TestManagedInstagramEntitlementCleanupFailureRemainsReconcilableWithoutEntitlement(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Redis = testutil.SetupTestRedis(t)
	require.NotNil(t, fixture.app.Redis)
	const professionalAccountID = "700000000000284"
	var provisioned metaRegistryProvisionResult
	require.NoError(t, fixture.app.WithCommittedTenantApp(
		fixture.org.ID,
		func(scoped *App) error {
			var err error
			provisioned, err = scoped.provisionMetaRegistryBinding(
				syntheticMetaInstagramProvisionInput(
					fixture, professionalAccountID, uuid.New(),
				),
			)
			return err
		},
	))
	setMetaInstagramOmnichannelEntitlement(t, fixture, false)
	var failedCleanupCalls atomic.Int32
	fixture.app.HTTPClient = &http.Client{Transport: metaInstagramRoundTripFunc(
		func(*http.Request) (*http.Response, error) {
			failedCleanupCalls.Add(1)
			return nil, errors.New("synthetic ambiguous unsubscription")
		},
	)}
	settings, settingsErr := fixture.app.metaInstagramOnboardingSettings()
	require.NoError(t, settingsErr)
	require.Error(t, fixture.app.compensateMetaInstagramEntitlementLoss(
		t.Context(), settings, fixture.org.ID, fixture.user.ID,
		provisioned.Account.ID, provisioned.SubscriptionOperation,
	))
	assert.Equal(t, int32(1), failedCleanupCalls.Load())

	var quarantined models.ChannelAccount
	require.NoError(t, fixture.db.Preload("Credentials").First(
		&quarantined, "id = ?", provisioned.Account.ID,
	).Error)
	assert.Equal(t, models.ChannelAccountStatusDegraded, quarantined.Status)
	assert.False(t, boolConfigValue(quarantined.Config, "outbound_enabled"))
	assert.False(t, boolConfigValue(quarantined.Config, "ai_reply_enabled"))
	assert.Equal(
		t, metaMessengerSubscriptionDesiredUnsubscribed,
		stringConfigValue(quarantined.Metadata, metaMessengerSubscriptionDesiredStateKey),
	)
	assert.Equal(
		t, metaMessengerSubscriptionUnsubscribeFailed,
		stringConfigValue(quarantined.Metadata, metaMessengerSubscriptionOperationStateKey),
	)

	status := testutil.NewRequest(t)
	testutil.SetAuthContext(status, fixture.org.ID, fixture.user.ID)
	testutil.SetHeader(status, "X-Organization-ID", fixture.org.ID.String())
	require.NoError(t, fixture.app.GetMetaInstagramOnboardingStatus(status))
	require.Equal(t, fasthttp.StatusOK, status.RequestCtx.Response.StatusCode())
	var onboardingStatus metaInstagramOnboardingStatus
	testutil.ParseEnvelopeResponse(t, status, &onboardingStatus)
	assert.True(t, onboardingStatus.Configured)
	assert.False(t, onboardingStatus.Enabled, "lost entitlement must not advertise OAuth intake")

	cleanupCalls := installSuccessfulMetaInstagramUnsubscribe(t, fixture)
	reconcile := testutil.NewRequest(t)
	testutil.SetAuthContext(reconcile, fixture.org.ID, fixture.user.ID)
	testutil.SetHeader(reconcile, "X-Organization-ID", fixture.org.ID.String())
	testutil.SetPathParam(reconcile, "id", provisioned.Account.ID.String())
	require.NoError(t, fixture.app.ReconcileMetaInstagramSubscription(reconcile))
	require.Equal(t, fasthttp.StatusOK, reconcile.RequestCtx.Response.StatusCode())
	assert.Equal(t, int32(2), cleanupCalls.Load())
	assertMetaInstagramEntitlementCleanupDisconnected(
		t, fixture, provisioned.Account.ID,
	)
}

func TestManagedInstagramDisconnectRemainsAvailableWithoutEntitlement(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Redis = testutil.SetupTestRedis(t)
	require.NotNil(t, fixture.app.Redis)
	setMetaInstagramOmnichannelEntitlement(t, fixture, false)
	cleanupCalls := installSuccessfulMetaInstagramUnsubscribe(t, fixture)

	request := testutil.NewJSONRequest(t, map[string]any{
		"confirm_account_id": fixture.profileID,
	})
	testutil.SetAuthContext(request, fixture.org.ID, fixture.user.ID)
	testutil.SetHeader(request, "X-Organization-ID", fixture.org.ID.String())
	testutil.SetPathParam(request, "id", fixture.account.ID.String())
	require.NoError(t, fixture.app.DisconnectMetaInstagram(request))
	require.Equal(t, fasthttp.StatusOK, request.RequestCtx.Response.StatusCode())
	assert.Equal(t, int32(2), cleanupCalls.Load())
	assertMetaInstagramEntitlementCleanupDisconnected(t, fixture, fixture.account.ID)
}

func TestManagedInstagramDisconnectConvertsFailedSubscribeToTeardownWithoutEntitlement(t *testing.T) {
	fixture := newMetaInstagramLifecycleFixture(t)
	fixture.app.Redis = testutil.SetupTestRedis(t)
	require.NotNil(t, fixture.app.Redis)
	const professionalAccountID = "700000000000285"
	var provisioned metaRegistryProvisionResult
	require.NoError(t, fixture.app.WithCommittedTenantApp(
		fixture.org.ID,
		func(scoped *App) error {
			var err error
			provisioned, err = scoped.provisionMetaRegistryBinding(
				syntheticMetaInstagramProvisionInput(
					fixture, professionalAccountID, uuid.New(),
				),
			)
			return err
		},
	))
	require.NoError(t, fixture.app.WithCommittedTenantApp(
		fixture.org.ID,
		func(scoped *App) error {
			return scoped.recordMetaInstagramSubscriptionOperationFailure(
				fixture.org.ID, provisioned.Account.ID, provisioned.SubscriptionOperation,
				metaMessengerSubscriptionSubscribePending,
				metaMessengerSubscriptionSubscribeFailed,
				time.Now().UTC(),
			)
		},
	))
	setMetaInstagramOmnichannelEntitlement(t, fixture, false)

	providerMethods := make([]string, 0, 2)
	fixture.app.HTTPClient = &http.Client{Transport: metaInstagramRoundTripFunc(
		func(request *http.Request) (*http.Response, error) {
			providerMethods = append(providerMethods, request.Method)
			require.Equal(t, "/v25.0/"+professionalAccountID+"/subscribed_apps", request.URL.Path)
			body := ""
			switch request.Method {
			case http.MethodDelete:
				body = `{"success":true}`
			case http.MethodGet:
				body = `{"data":[]}`
			default:
				return nil, errors.New("failed-subscribe teardown must never call subscribe")
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    request,
			}, nil
		},
	)}
	request := testutil.NewJSONRequest(t, map[string]any{
		"confirm_account_id": professionalAccountID,
	})
	testutil.SetAuthContext(request, fixture.org.ID, fixture.user.ID)
	testutil.SetHeader(request, "X-Organization-ID", fixture.org.ID.String())
	testutil.SetPathParam(request, "id", provisioned.Account.ID.String())
	require.NoError(t, fixture.app.DisconnectMetaInstagram(request))
	require.Equal(t, fasthttp.StatusOK, request.RequestCtx.Response.StatusCode())
	assert.Equal(t, []string{http.MethodDelete, http.MethodGet}, providerMethods)
	assertMetaInstagramEntitlementCleanupDisconnected(
		t, fixture, provisioned.Account.ID,
	)
	allowed, err := fixture.app.HasProductEntitlement(
		uuid.Nil, fixture.org.ID, channelapi.OmnichannelEntitlementKey,
	)
	require.NoError(t, err)
	assert.False(t, allowed, "teardown must not recreate the commercial entitlement")
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

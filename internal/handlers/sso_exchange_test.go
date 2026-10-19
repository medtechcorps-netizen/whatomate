package handlers_test

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/middleware"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

const laneABindPrefix = "whm_sso_bind_"

// laneASSOAttempt is what one SSO callback handed the browser.
type laneASSOAttempt struct {
	code     string
	bindName string
	binding  string
	user     *models.User
	orgID    uuid.UUID
	callback *fastglue.Request
}

// laneABindingCookie returns the SSO binding cookie a response sets (or
// clears): its name and value.
func laneABindingCookie(req *fastglue.Request) (string, string) {
	var name, value string
	req.RequestCtx.Response.Header.VisitAllCookie(func(key, raw []byte) {
		if strings.HasPrefix(string(key), laneABindPrefix) {
			c := fasthttp.AcquireCookie()
			defer fasthttp.ReleaseCookie(c)
			_ = c.ParseBytes(raw)
			name, value = string(key), string(c.Value())
		}
	})
	return name, value
}

// laneASSOCallbackFor runs a successful SSO callback through the SSO of
// organization orgID for an existing user.
func laneASSOCallbackFor(t *testing.T, app *handlers.App, fake *fakeOAuthProvider, user *models.User, orgID uuid.UUID) laneASSOAttempt {
	t.Helper()
	fake.UserEmail = user.Email
	nonce := "exchange-" + uuid.NewString()
	state := handlers.SSOState{
		OrgID: orgID.String(), Provider: "custom", Nonce: nonce, ExpiresAt: time.Now().Add(5 * time.Minute),
	}
	stored, _ := json.Marshal(state)
	require.NoError(t, app.Redis.Set(context.Background(), "sso:state:"+nonce, stored, 5*time.Minute).Err())

	req := testutil.NewGETRequest(t)
	testutil.SetPathParam(req, "provider", "custom")
	testutil.SetQueryParam(req, "code", "auth-code-xyz")
	testutil.SetQueryParam(req, "state", nonce)
	require.NoError(t, app.CallbackSSO(req))
	require.Equal(t, fasthttp.StatusTemporaryRedirect, testutil.GetResponseStatusCode(req))

	location, err := url.Parse(string(req.RequestCtx.Response.Header.Peek("Location")))
	require.NoError(t, err)
	code := location.Query().Get("code")
	require.NotEmpty(t, code)
	name, value := laneABindingCookie(req)
	require.NotEmpty(t, name)
	require.NotEmpty(t, value)
	return laneASSOAttempt{code: code, bindName: name, binding: value, user: user, orgID: orgID, callback: req}
}

// laneASSOCallback runs a successful SSO callback for a new user of a new
// organization (signing in through that organization's SSO).
func laneASSOCallback(t *testing.T, app *handlers.App) laneASSOAttempt {
	t.Helper()
	fake := newFakeOAuth(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	createCustomSSOProvider(t, app, org.ID, fake, func(p *models.SSOProvider) {
		p.AllowAutoCreate = false
	})
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(testutil.UniqueEmail("sso-exchange")))
	return laneASSOCallbackFor(t, app, fake, user, org.ID)
}

// laneASSOExchange posts {"sso_exchange": code} to the refresh route with the
// given cookies (the browser's jar for that route).
func laneASSOExchange(t *testing.T, app *handlers.App, code string, cookies map[string]string) *fastglue.Request {
	t.Helper()
	req := testutil.NewJSONRequest(t, map[string]any{"sso_exchange": code})
	for name, value := range cookies {
		req.RequestCtx.Request.Header.SetCookie(name, value)
	}
	require.NoError(t, app.RefreshToken(req))
	return req
}

// laneASessionCookies lists the session cookies a response sets or clears.
func laneASessionCookies(req *fastglue.Request) []string {
	var names []string
	req.RequestCtx.Response.Header.VisitAllCookie(func(key, _ []byte) {
		switch string(key) {
		case "whm_access", "whm_refresh", "whm_csrf":
			names = append(names, string(key))
		}
	})
	return names
}

// laneAApplyCookies applies a response's Set-Cookie headers to a jar, as the
// browser does (an expired or empty cookie is removed).
func laneAApplyCookies(jar map[string]string, req *fastglue.Request) {
	req.RequestCtx.Response.Header.VisitAllCookie(func(key, raw []byte) {
		c := fasthttp.AcquireCookie()
		defer fasthttp.ReleaseCookie(c)
		_ = c.ParseBytes(raw)
		expired := c.MaxAge() < 0 || (!c.Expire().IsZero() && c.Expire().Before(time.Now()))
		if len(c.Value()) == 0 || expired {
			delete(jar, string(key))
			return
		}
		jar[string(key)] = string(c.Value())
	})
}

func laneAAccessClaims(t *testing.T, token string) *middleware.JWTClaims {
	t.Helper()
	parsed, err := jwt.ParseWithClaims(token, &middleware.JWTClaims{}, func(*jwt.Token) (any, error) {
		return []byte(testutil.TestJWTSecret), nil
	})
	require.NoError(t, err)
	return parsed.Claims.(*middleware.JWTClaims)
}

func TestLaneA_SSOCallback_SetsNoSessionCookie(t *testing.T) {
	app := newSSOApp(t)
	attempt := laneASSOCallback(t, app)
	req := attempt.callback

	// The navigation carries no session: nothing it could race other tabs with.
	assert.Empty(t, laneASessionCookies(req))
	assert.NotContains(t, string(req.RequestCtx.Response.Header.Peek("Location")), "access_token")

	// The attempt's own binding cookie: httpOnly, sent only to the exchange,
	// for the code's life.
	bind := fasthttp.AcquireCookie()
	defer fasthttp.ReleaseCookie(bind)
	bind.SetKey(attempt.bindName)
	require.True(t, req.RequestCtx.Response.Header.Cookie(bind))
	assert.True(t, bind.HTTPOnly())
	assert.Equal(t, "/api/auth/refresh", string(bind.Path()))
	assert.Equal(t, 60, bind.MaxAge())

	// The code is stored for at most a minute.
	ttl, err := app.Redis.TTL(context.Background(), "sso:exchange:"+attempt.code).Result()
	require.NoError(t, err)
	assert.Greater(t, ttl, time.Duration(0))
	assert.LessOrEqual(t, ttl, 60*time.Second)
}

func TestLaneA_SSOExchange_AnswersLikeLogin(t *testing.T) {
	app := newSSOApp(t)
	attempt := laneASSOCallback(t, app)

	req := laneASSOExchange(t, app, attempt.code, map[string]string{attempt.bindName: attempt.binding})
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	// The session cookies, as a login sets them...
	assert.ElementsMatch(t, []string{"whm_access", "whm_refresh", "whm_csrf"}, laneASessionCookies(req))
	// ...and the login answer.
	var envelope struct {
		Data struct {
			ExpiresIn int `json:"expires_in"`
			User      struct {
				ID             string `json:"id"`
				Email          string `json:"email"`
				OrganizationID string `json:"organization_id"`
			} `json:"user"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &envelope))
	assert.Equal(t, 15*60, envelope.Data.ExpiresIn)
	assert.Equal(t, attempt.user.ID.String(), envelope.Data.User.ID)
	assert.Equal(t, attempt.user.Email, envelope.Data.User.Email)
	assert.Equal(t, attempt.orgID.String(), envelope.Data.User.OrganizationID)
	// This attempt's binding is spent.
	name, value := laneABindingCookie(req)
	assert.Equal(t, attempt.bindName, name)
	assert.Empty(t, value)

	// The issued refresh token is a live session.
	refreshed := laneARefresh(t, app, testutil.GetResponseCookie(req, "whm_refresh"))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(refreshed))
}

func TestLaneA_SSOExchange_CodeIsSingleUse(t *testing.T) {
	app := newSSOApp(t)
	attempt := laneASSOCallback(t, app)
	jar := map[string]string{attempt.bindName: attempt.binding}
	first := laneASSOExchange(t, app, attempt.code, jar)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(first))

	again := laneASSOExchange(t, app, attempt.code, jar)
	testutil.AssertErrorResponse(t, again, fasthttp.StatusUnauthorized, "Invalid or expired sign-in")
	assert.Empty(t, laneASessionCookies(again))
}

func TestLaneA_SSOExchange_RequiresTheBindingCookie(t *testing.T) {
	app := newSSOApp(t)

	// Without the binding cookie (another browser holding the URL).
	attempt := laneASSOCallback(t, app)
	missing := laneASSOExchange(t, app, attempt.code, nil)
	testutil.AssertErrorResponse(t, missing, fasthttp.StatusUnauthorized, "Invalid or expired sign-in")
	assert.Empty(t, laneASessionCookies(missing))
	// The attempt spent the code: not even the right browser can use it now.
	late := laneASSOExchange(t, app, attempt.code, map[string]string{attempt.bindName: attempt.binding})
	testutil.AssertErrorResponse(t, late, fasthttp.StatusUnauthorized, "Invalid or expired sign-in")
	assert.Empty(t, laneASessionCookies(late))

	// With a wrong value for that attempt's binding cookie.
	attempt2 := laneASSOCallback(t, app)
	wrong := laneASSOExchange(t, app, attempt2.code, map[string]string{attempt2.bindName: "binding-of-another-browser"})
	testutil.AssertErrorResponse(t, wrong, fasthttp.StatusUnauthorized, "Invalid or expired sign-in")
	assert.Empty(t, laneASessionCookies(wrong))
}

func TestLaneA_SSOExchange_ExpiredCodeRejected(t *testing.T) {
	app := newSSOApp(t)
	attempt := laneASSOCallback(t, app)
	require.NoError(t, app.Redis.PExpire(context.Background(), "sso:exchange:"+attempt.code, 50*time.Millisecond).Err())
	time.Sleep(200 * time.Millisecond)

	req := laneASSOExchange(t, app, attempt.code, map[string]string{attempt.bindName: attempt.binding})
	testutil.AssertErrorResponse(t, req, fasthttp.StatusUnauthorized, "Invalid or expired sign-in")
	assert.Empty(t, laneASessionCookies(req))
}

func TestLaneA_SSOExchange_DisabledUserRejected(t *testing.T) {
	app := newSSOApp(t)
	attempt := laneASSOCallback(t, app)
	require.NoError(t, app.DB.Model(&models.User{}).Where("id = ?", attempt.user.ID).Update("is_active", false).Error)

	req := laneASSOExchange(t, app, attempt.code, map[string]string{attempt.bindName: attempt.binding})
	testutil.AssertErrorResponse(t, req, fasthttp.StatusUnauthorized, "Account is disabled")
	assert.Empty(t, laneASessionCookies(req))
}

func TestLaneA_SSOExchange_RefusedOnceRemovedFromTheOrganization(t *testing.T) {
	app := newSSOApp(t)
	attempt := laneASSOCallback(t, app)
	// Removed from the organization between the callback and the exchange.
	require.NoError(t, app.DB.Where("user_id = ?", attempt.user.ID).Delete(&models.UserOrganization{}).Error)

	req := laneASSOExchange(t, app, attempt.code, map[string]string{attempt.bindName: attempt.binding})
	testutil.AssertErrorResponse(t, req, fasthttp.StatusUnauthorized, "No active organization membership")
	assert.Empty(t, laneASessionCookies(req))
}

func TestLaneA_SSOExchange_RefusedOnceRemovedFromTheSSOOrganization(t *testing.T) {
	app := newSSOApp(t)
	// The user's own organization is another one; they sign in through the
	// SSO of an organization they are a member of...
	fake := newFakeOAuth(t)
	home := testutil.CreateTestOrganization(t, app.DB)
	ssoOrg := testutil.CreateTestOrganization(t, app.DB)
	createCustomSSOProvider(t, app, ssoOrg.ID, fake, func(p *models.SSOProvider) {
		p.AllowAutoCreate = false
	})
	user := testutil.CreateTestUser(t, app.DB, home.ID, testutil.WithEmail(testutil.UniqueEmail("sso-member")))
	require.NoError(t, app.DB.Create(&models.UserOrganization{UserID: user.ID, OrganizationID: ssoOrg.ID}).Error)
	attempt := laneASSOCallbackFor(t, app, fake, user, ssoOrg.ID)
	// ...and lose that membership before the exchange (a password login
	// would still work through their own organization; this SSO no longer).
	require.NoError(t, app.DB.Where("user_id = ? AND organization_id = ?", user.ID, ssoOrg.ID).
		Delete(&models.UserOrganization{}).Error)

	req := laneASSOExchange(t, app, attempt.code, map[string]string{attempt.bindName: attempt.binding})
	testutil.AssertErrorResponse(t, req, fasthttp.StatusUnauthorized, "not a member of this organization")
	assert.Empty(t, laneASessionCookies(req))
}

func TestLaneA_SSOExchange_IssuesTheRoleCurrentAtExchange(t *testing.T) {
	app := newSSOApp(t)
	attempt := laneASSOCallback(t, app)
	// The user's role changes between the callback and the exchange.
	role := testutil.CreateTestRole(t, app.DB, attempt.orgID, "sso-role-"+uuid.NewString()[:8], nil)
	require.NoError(t, app.DB.Model(&models.UserOrganization{}).
		Where("user_id = ? AND organization_id = ?", attempt.user.ID, attempt.orgID).
		Update("role_id", role.ID).Error)

	req := laneASSOExchange(t, app, attempt.code, map[string]string{attempt.bindName: attempt.binding})
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	claims := laneAAccessClaims(t, testutil.GetResponseCookie(req, "whm_access"))
	require.NotNil(t, claims.RoleID)
	assert.Equal(t, role.ID, *claims.RoleID)
	assert.Equal(t, attempt.orgID, claims.OrganizationID)
}

func TestLaneA_SSOExchange_TwoAttemptsKeepTheirOwnBinding(t *testing.T) {
	app := newSSOApp(t)
	jar := map[string]string{}

	// Callback A; its exchange is sent (with the jar as it is then)...
	a := laneASSOCallback(t, app)
	laneAApplyCookies(jar, a.callback)
	sentA := map[string]string{}
	for name, value := range jar {
		sentA[name] = value
	}
	// ...and while it is in flight, callback B lands in the same browser.
	b := laneASSOCallback(t, app)
	laneAApplyCookies(jar, b.callback)
	require.NotEqual(t, a.bindName, b.bindName)

	// A's answer arrives: it spends only A's binding.
	exchangeA := laneASSOExchange(t, app, a.code, sentA)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(exchangeA))
	cleared, _ := laneABindingCookie(exchangeA)
	assert.Equal(t, a.bindName, cleared)
	laneAApplyCookies(jar, exchangeA)
	assert.Equal(t, b.binding, jar[b.bindName])

	// B's exchange (queued behind A's) still succeeds...
	exchangeB := laneASSOExchange(t, app, b.code, jar)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(exchangeB))
	laneAApplyCookies(jar, exchangeB)

	// ...and the jar ends with exactly B's session.
	for _, name := range []string{"whm_access", "whm_refresh", "whm_csrf"} {
		assert.Equal(t, testutil.GetResponseCookie(exchangeB, name), jar[name], name)
	}
	_, leftover := jar[b.bindName]
	assert.False(t, leftover)

	// /me, for the jar's access token, is B.
	claims := laneAAccessClaims(t, jar["whm_access"])
	assert.Equal(t, b.user.ID, claims.UserID)
	me := testutil.NewGETRequest(t)
	testutil.SetAuthContext(me, claims.OrganizationID, claims.UserID)
	require.NoError(t, app.GetCurrentUser(me))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(me))
	var profile struct {
		Data struct {
			Email string `json:"email"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(me), &profile))
	assert.Equal(t, b.user.Email, profile.Data.Email)
	// B's refresh token is live.
	refreshed := laneARefresh(t, app, jar["whm_refresh"])
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(refreshed))
}

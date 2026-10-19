package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/middleware"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const laneABindPrefix = "whm_sso_bind_"

// laneASSOAttempt is what one SSO callback handed the browser.
type laneASSOAttempt struct {
	code     string
	bindName string
	binding  string
	user     *models.User
	orgID    uuid.UUID
	fake     *fakeOAuthProvider
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

// laneABindingSetCookies lists the raw Set-Cookie values of every SSO binding
// cookie a response sets or clears, as they go on the wire.
func laneABindingSetCookies(req *fastglue.Request) []string {
	var raws []string
	req.RequestCtx.Response.Header.VisitAllCookie(func(key, raw []byte) {
		if strings.HasPrefix(string(key), laneABindPrefix) {
			raws = append(raws, string(raw))
		}
	})
	return raws
}

// laneAAssertBindingSpent asserts the response expires exactly this attempt's
// binding cookie, on the wire: an empty value, max-age=0, the refresh path.
func laneAAssertBindingSpent(t *testing.T, req *fastglue.Request, attempt laneASSOAttempt, path string) {
	t.Helper()
	raws := laneABindingSetCookies(req)
	require.Len(t, raws, 1, "exactly one binding cookie is touched")
	raw := strings.ToLower(raws[0])
	assert.True(t, strings.HasPrefix(raw, strings.ToLower(attempt.bindName)+"=;"), raws[0])
	assert.Contains(t, raw, "max-age=0", raws[0])
	assert.Contains(t, raw, "path="+path, raws[0])
}

// laneAAssertNoBindingTouched asserts the response sets or clears no binding
// cookie: an exchange that cannot name its attempt leaves every binding alone.
func laneAAssertNoBindingTouched(t *testing.T, req *fastglue.Request) {
	t.Helper()
	assert.Empty(t, laneABindingSetCookies(req))
}

// laneAAssertNoGrantFor asserts no SSO exchange grant is stored for userID.
func laneAAssertNoGrantFor(t *testing.T, app *handlers.App, userID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	iter := app.Redis.Scan(ctx, 0, "sso:exchange:*", 100).Iterator()
	for iter.Next(ctx) {
		stored, err := app.Redis.Get(ctx, iter.Val()).Bytes()
		if err != nil {
			continue
		}
		var grant struct {
			UserID string `json:"user_id"`
		}
		_ = json.Unmarshal(stored, &grant)
		assert.NotEqual(t, userID.String(), grant.UserID, "grant %s", iter.Val())
	}
	require.NoError(t, iter.Err())
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
	return laneASSOAttempt{code: code, bindName: name, binding: value, user: user, orgID: orgID, fake: fake, callback: req}
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

// laneAExchangeRequest builds a {"sso_exchange": code} request to the refresh
// route with the given cookies (the browser's jar for that route).
func laneAExchangeRequest(t *testing.T, code string, cookies map[string]string) *fastglue.Request {
	t.Helper()
	req := testutil.NewJSONRequest(t, map[string]any{"sso_exchange": code})
	for name, value := range cookies {
		req.RequestCtx.Request.Header.SetCookie(name, value)
	}
	return req
}

// laneASSOExchange posts {"sso_exchange": code} to the refresh route with the
// given cookies.
func laneASSOExchange(t *testing.T, app *handlers.App, code string, cookies map[string]string) *fastglue.Request {
	t.Helper()
	req := laneAExchangeRequest(t, code, cookies)
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

func TestLaneA_SSOBindingCookie_UnderABasePath(t *testing.T) {
	app := newSSOApp(t)
	app.Config.Server.BasePath = "/crm"
	app.Config.Cookie.Secure = true
	attempt := laneASSOCallback(t, app)

	// As fasthttp writes it: max-age, path, HttpOnly, secure, SameSite.
	set := laneABindingSetCookies(attempt.callback)
	require.Len(t, set, 1)
	assert.Equal(t,
		attempt.bindName+"="+attempt.binding+"; max-age=60; path=/crm/api/auth/refresh; HttpOnly; secure; SameSite=Lax",
		set[0])

	req := laneASSOExchange(t, app, attempt.code, map[string]string{attempt.bindName: attempt.binding})
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
	spent := laneABindingSetCookies(req)
	require.Len(t, spent, 1)
	assert.Equal(t,
		attempt.bindName+"=; max-age=0; path=/crm/api/auth/refresh; HttpOnly; secure; SameSite=Lax",
		spent[0])
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
	laneAAssertBindingSpent(t, req, attempt, "/api/auth/refresh")

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
	// A spent code names no attempt: no binding is touched.
	laneAAssertNoBindingTouched(t, again)
}

func TestLaneA_SSOExchange_ConcurrentExchangesOfOneCode(t *testing.T) {
	app := newSSOApp(t)
	attempt := laneASSOCallback(t, app)
	const n = 8
	reqs := make([]*fastglue.Request, n)
	for i := range reqs {
		reqs[i] = laneAExchangeRequest(t, attempt.code, map[string]string{attempt.bindName: attempt.binding})
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range reqs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = app.RefreshToken(reqs[i])
		}(i)
	}
	wg.Wait()

	ok := 0
	for i, req := range reqs {
		require.NoError(t, errs[i])
		switch testutil.GetResponseStatusCode(req) {
		case fasthttp.StatusOK:
			ok++
		default:
			testutil.AssertErrorResponse(t, req, fasthttp.StatusUnauthorized, "Invalid or expired sign-in")
			assert.Empty(t, laneASessionCookies(req))
		}
	}
	assert.Equal(t, 1, ok, "exactly one exchange of a code succeeds")
}

func TestLaneA_SSOExchange_RequiresTheBindingCookie(t *testing.T) {
	app := newSSOApp(t)

	// Without the binding cookie (another browser holding the URL).
	attempt := laneASSOCallback(t, app)
	missing := laneASSOExchange(t, app, attempt.code, nil)
	testutil.AssertErrorResponse(t, missing, fasthttp.StatusUnauthorized, "Invalid or expired sign-in")
	assert.Empty(t, laneASessionCookies(missing))
	laneAAssertBindingSpent(t, missing, attempt, "/api/auth/refresh")
	// The attempt spent the code: not even the right browser can use it now.
	late := laneASSOExchange(t, app, attempt.code, map[string]string{attempt.bindName: attempt.binding})
	testutil.AssertErrorResponse(t, late, fasthttp.StatusUnauthorized, "Invalid or expired sign-in")
	assert.Empty(t, laneASessionCookies(late))
	laneAAssertNoBindingTouched(t, late)

	// With a wrong value for that attempt's binding cookie.
	attempt2 := laneASSOCallback(t, app)
	wrong := laneASSOExchange(t, app, attempt2.code, map[string]string{attempt2.bindName: "binding-of-another-browser"})
	testutil.AssertErrorResponse(t, wrong, fasthttp.StatusUnauthorized, "Invalid or expired sign-in")
	assert.Empty(t, laneASessionCookies(wrong))
	laneAAssertBindingSpent(t, wrong, attempt2, "/api/auth/refresh")
}

func TestLaneA_SSOExchange_RequiresAJSONBody(t *testing.T) {
	app := newSSOApp(t)
	attempt := laneASSOCallback(t, app)

	// What a cross-site page can send (a form body), or a type that only
	// mentions JSON in a parameter, is refused before the code is touched.
	for _, body := range []struct{ contentType, body string }{
		{"application/x-www-form-urlencoded", "sso_exchange=" + url.QueryEscape(attempt.code)},
		{"text/plain;x=application/json", `{"sso_exchange":"` + attempt.code + `"}`},
	} {
		req := testutil.NewRequest(t)
		req.RequestCtx.Request.Header.SetMethod("POST")
		req.RequestCtx.Request.Header.SetContentType(body.contentType)
		req.RequestCtx.Request.SetBodyString(body.body)
		req.RequestCtx.Request.Header.SetCookie(attempt.bindName, attempt.binding)
		require.NoError(t, app.RefreshToken(req))
		assert.Equal(t, fasthttp.StatusUnsupportedMediaType, testutil.GetResponseStatusCode(req), body.contentType)
		assert.Empty(t, laneASessionCookies(req))
		laneAAssertNoBindingTouched(t, req)
	}

	// The code is still good for its own browser's JSON exchange (a charset
	// parameter is fine).
	req := laneAExchangeRequest(t, attempt.code, map[string]string{attempt.bindName: attempt.binding})
	req.RequestCtx.Request.Header.SetContentType("application/json; charset=utf-8")
	require.NoError(t, app.RefreshToken(req))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
}

func TestLaneA_SSOExchange_TakesPrecedenceOverRevoke(t *testing.T) {
	app := newSSOApp(t)
	// The user already has a session in this browser...
	first := laneASSOCallback(t, app)
	signedIn := laneASSOExchange(t, app, first.code, map[string]string{first.bindName: first.binding})
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(signedIn))
	oldRefresh := testutil.GetResponseCookie(signedIn, "whm_refresh")
	oldCSRF := testutil.GetResponseCookie(signedIn, "whm_csrf")

	// ...and a body that asks for both a revoke and an exchange does the
	// exchange only.
	second := laneASSOCallbackFor(t, app, first.fake, first.user, first.orgID)
	req := testutil.NewJSONRequest(t, map[string]any{"revoke": true, "sso_exchange": second.code})
	req.RequestCtx.Request.Header.SetCookie(second.bindName, second.binding)
	req.RequestCtx.Request.Header.SetCookie("whm_refresh", oldRefresh)
	req.RequestCtx.Request.Header.SetCookie("whm_csrf", oldCSRF)
	req.RequestCtx.Request.Header.Set("X-CSRF-Token", oldCSRF)
	require.NoError(t, app.RefreshToken(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
	assert.ElementsMatch(t, []string{"whm_access", "whm_refresh", "whm_csrf"}, laneASessionCookies(req))
	assert.NotEqual(t, oldRefresh, testutil.GetResponseCookie(req, "whm_refresh"))

	// Nothing was revoked: the earlier session still refreshes.
	refreshed := laneARefresh(t, app, oldRefresh)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(refreshed))
}

func TestLaneA_SSOExchange_ExpiredCodeRejected(t *testing.T) {
	app := newSSOApp(t)
	attempt := laneASSOCallback(t, app)
	require.NoError(t, app.Redis.PExpire(context.Background(), "sso:exchange:"+attempt.code, 50*time.Millisecond).Err())
	time.Sleep(200 * time.Millisecond)

	req := laneASSOExchange(t, app, attempt.code, map[string]string{attempt.bindName: attempt.binding})
	testutil.AssertErrorResponse(t, req, fasthttp.StatusUnauthorized, "Invalid or expired sign-in")
	assert.Empty(t, laneASessionCookies(req))
	laneAAssertNoBindingTouched(t, req)
}

func TestLaneA_SSOExchange_StoreFaultAnswers503(t *testing.T) {
	app := newSSOApp(t)
	attempt := laneASSOCallback(t, app)
	// Redis fails (not a missing key) during the exchange.
	live := app.Redis
	app.Redis = redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 200 * time.Millisecond})
	defer func() {
		_ = app.Redis.Close()
		app.Redis = live
	}()

	req := laneASSOExchange(t, app, attempt.code, map[string]string{attempt.bindName: attempt.binding})
	testutil.AssertErrorResponse(t, req, fasthttp.StatusServiceUnavailable, "temporarily unavailable")
	assert.Empty(t, laneASessionCookies(req))
	laneAAssertNoBindingTouched(t, req)
}

func TestLaneA_SSOExchange_UserLookupFaultAnswers503(t *testing.T) {
	app := newSSOApp(t)
	attempt := laneASSOCallback(t, app)
	// The database fails (not a missing row) while the user is loaded.
	live := app.DB
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	app.DB = live.WithContext(cancelled)
	defer func() { app.DB = live }()

	req := laneASSOExchange(t, app, attempt.code, map[string]string{attempt.bindName: attempt.binding})
	testutil.AssertErrorResponse(t, req, fasthttp.StatusServiceUnavailable, "temporarily unavailable")
	assert.Empty(t, laneASessionCookies(req))
	laneAAssertBindingSpent(t, req, attempt, "/api/auth/refresh")
}

func TestLaneA_SSOExchange_DisabledUserRejected(t *testing.T) {
	app := newSSOApp(t)
	attempt := laneASSOCallback(t, app)
	require.NoError(t, app.DB.Model(&models.User{}).Where("id = ?", attempt.user.ID).Update("is_active", false).Error)

	req := laneASSOExchange(t, app, attempt.code, map[string]string{attempt.bindName: attempt.binding})
	testutil.AssertErrorResponse(t, req, fasthttp.StatusUnauthorized, "Account is disabled")
	assert.Empty(t, laneASessionCookies(req))
	laneAAssertBindingSpent(t, req, attempt, "/api/auth/refresh")
}

func TestLaneA_SSOExchange_SoftDeletedUserRejected(t *testing.T) {
	app := newSSOApp(t)
	attempt := laneASSOCallback(t, app)
	require.NoError(t, app.DB.Delete(&models.User{}, "id = ?", attempt.user.ID).Error)

	req := laneASSOExchange(t, app, attempt.code, map[string]string{attempt.bindName: attempt.binding})
	testutil.AssertErrorResponse(t, req, fasthttp.StatusUnauthorized, "Invalid or expired sign-in")
	assert.Empty(t, laneASessionCookies(req))
	laneAAssertBindingSpent(t, req, attempt, "/api/auth/refresh")
}

func TestLaneA_SSOExchange_RefusedOnceRemovedFromTheOrganization(t *testing.T) {
	app := newSSOApp(t)
	attempt := laneASSOCallback(t, app)
	// Removed from the organization between the callback and the exchange
	// (it was their own: users.organization_id still names it).
	require.NoError(t, app.DB.Where("user_id = ?", attempt.user.ID).Delete(&models.UserOrganization{}).Error)

	req := laneASSOExchange(t, app, attempt.code, map[string]string{attempt.bindName: attempt.binding})
	testutil.AssertErrorResponse(t, req, fasthttp.StatusUnauthorized, "not a member of this organization")
	assert.Empty(t, laneASessionCookies(req))
	laneAAssertBindingSpent(t, req, attempt, "/api/auth/refresh")
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
	laneAAssertBindingSpent(t, req, attempt, "/api/auth/refresh")
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
	laneAAssertBindingSpent(t, req, attempt, "/api/auth/refresh")
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
	laneAAssertBindingSpent(t, exchangeA, a, "/api/auth/refresh")
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

// laneASessionClaims returns the access-token claims and the answered
// organization of a session-issuing response.
func laneASessionClaims(t *testing.T, req *fastglue.Request) (*middleware.JWTClaims, string) {
	t.Helper()
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
	var envelope struct {
		Data struct {
			User struct {
				OrganizationID string `json:"organization_id"`
			} `json:"user"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &envelope))
	return laneAAccessClaims(t, testutil.GetResponseCookie(req, "whm_access")), envelope.Data.User.OrganizationID
}

// laneALogin signs a testutil user in with its password ("password123").
func laneALogin(t *testing.T, app *handlers.App, email string) *fastglue.Request {
	t.Helper()
	req := testutil.NewJSONRequest(t, map[string]string{"email": email, "password": "password123"})
	require.NoError(t, app.Login(req))
	return req
}

// laneAMembership adds user to orgID with a role of its own there.
func laneAMembership(t *testing.T, app *handlers.App, userID, orgID uuid.UUID) *models.CustomRole {
	t.Helper()
	role := testutil.CreateTestRole(t, app.DB, orgID, "member-"+uuid.NewString()[:8], nil)
	require.NoError(t, app.DB.Create(&models.UserOrganization{UserID: userID, OrganizationID: orgID, RoleID: &role.ID}).Error)
	return role
}

func TestLaneA_SSOCallback_TheLegacyHomeOrganizationGrantsNothing(t *testing.T) {
	app := newSSOApp(t)
	// Alice's home organization is A (users.organization_id), and she is also
	// a member of C. She is removed from A: only her membership row goes.
	fake := newFakeOAuth(t)
	orgA := testutil.CreateTestOrganization(t, app.DB)
	orgC := testutil.CreateTestOrganization(t, app.DB)
	createCustomSSOProvider(t, app, orgA.ID, fake, func(p *models.SSOProvider) {
		p.AllowAutoCreate = false
	})
	alice := testutil.CreateTestUser(t, app.DB, orgA.ID, testutil.WithEmail(testutil.UniqueEmail("alice")))
	laneAMembership(t, app, alice.ID, orgC.ID)
	require.NoError(t, app.DB.Where("user_id = ? AND organization_id = ?", alice.ID, orgA.ID).
		Delete(&models.UserOrganization{}).Error)

	// A's identity provider vouches for "Alice": A's SSO still refuses her.
	fake.UserEmail = alice.Email
	nonce := "legacy-home-" + uuid.NewString()
	state, _ := json.Marshal(handlers.SSOState{
		OrgID: orgA.ID.String(), Provider: "custom", Nonce: nonce, ExpiresAt: time.Now().Add(5 * time.Minute),
	})
	require.NoError(t, app.Redis.Set(context.Background(), "sso:state:"+nonce, state, 5*time.Minute).Err())
	req := testutil.NewGETRequest(t)
	testutil.SetPathParam(req, "provider", "custom")
	testutil.SetQueryParam(req, "code", "auth-code-xyz")
	testutil.SetQueryParam(req, "state", nonce)
	require.NoError(t, app.CallbackSSO(req))

	assert.Contains(t, string(req.RequestCtx.Response.Header.Peek("Location")), "not+a+member")
	laneAAssertNoBindingTouched(t, req)
	laneAAssertNoGrantFor(t, app, alice.ID)
}

func TestLaneA_SSOExchange_RefusedOnceRemovedFromTheHomeOrganizationThoughAMemberElsewhere(t *testing.T) {
	app := newSSOApp(t)
	// Alice signs in through the SSO of her home organization A; she is also
	// a member of C...
	attempt := laneASSOCallback(t, app)
	orgC := testutil.CreateTestOrganization(t, app.DB)
	laneAMembership(t, app, attempt.user.ID, orgC.ID)
	// ...and is removed from A before the exchange: only the membership row
	// goes, users.organization_id still says A.
	require.NoError(t, app.DB.Where("user_id = ? AND organization_id = ?", attempt.user.ID, attempt.orgID).
		Delete(&models.UserOrganization{}).Error)

	// No session in C (or anywhere) through A's SSO.
	req := laneASSOExchange(t, app, attempt.code, map[string]string{attempt.bindName: attempt.binding})
	testutil.AssertErrorResponse(t, req, fasthttp.StatusUnauthorized, "not a member of this organization")
	assert.Empty(t, laneASessionCookies(req))
	laneAAssertBindingSpent(t, req, attempt, "/api/auth/refresh")
}

func TestLaneA_SSOExchange_SuperAdminOfTheOrganization(t *testing.T) {
	app := newSSOApp(t)
	// A super admin whose own organization is S, with no membership row there.
	fake := newFakeOAuth(t)
	orgS := testutil.CreateTestOrganization(t, app.DB)
	createCustomSSOProvider(t, app, orgS.ID, fake, func(p *models.SSOProvider) {
		p.AllowAutoCreate = false
	})
	admin := testutil.CreateTestUser(t, app.DB, orgS.ID, testutil.WithEmail(testutil.UniqueEmail("super")))
	require.NoError(t, app.DB.Model(&models.User{}).Where("id = ?", admin.ID).Update("is_super_admin", true).Error)
	require.NoError(t, app.DB.Where("user_id = ?", admin.ID).Delete(&models.UserOrganization{}).Error)
	admin.IsSuperAdmin = true

	attempt := laneASSOCallbackFor(t, app, fake, admin, orgS.ID)
	req := laneASSOExchange(t, app, attempt.code, map[string]string{attempt.bindName: attempt.binding})

	claims, org := laneASessionClaims(t, req)
	assert.Equal(t, orgS.ID.String(), org)
	assert.Equal(t, orgS.ID, claims.OrganizationID)
	assert.True(t, claims.IsSuperAdmin)
	laneAAssertBindingSpent(t, req, attempt, "/api/auth/refresh")
}

func TestLaneA_SSOExchange_FallsBackLikeLogin(t *testing.T) {
	app := newSSOApp(t)
	// Home organization H, a default membership in C and a membership in B;
	// the membership in H is gone. The sign-in goes through B's SSO.
	fake := newFakeOAuth(t)
	home := testutil.CreateTestOrganization(t, app.DB)
	orgB := testutil.CreateTestOrganization(t, app.DB)
	orgC := testutil.CreateTestOrganization(t, app.DB)
	createCustomSSOProvider(t, app, orgB.ID, fake, func(p *models.SSOProvider) {
		p.AllowAutoCreate = false
	})
	user := testutil.CreateTestUser(t, app.DB, home.ID, testutil.WithEmail(testutil.UniqueEmail("fallback")))
	// B's membership first: only the default flag, not the creation order,
	// makes C the one Login falls back to.
	laneAMembership(t, app, user.ID, orgB.ID)
	roleC := laneAMembership(t, app, user.ID, orgC.ID)
	madeDefault := app.DB.Model(&models.UserOrganization{}).
		Where("user_id = ? AND organization_id = ?", user.ID, orgC.ID).Update("is_default", true)
	require.NoError(t, madeDefault.Error)
	require.EqualValues(t, 1, madeDefault.RowsAffected)
	require.NoError(t, app.DB.Where("user_id = ? AND organization_id = ?", user.ID, home.ID).
		Delete(&models.UserOrganization{}).Error)

	attempt := laneASSOCallbackFor(t, app, fake, user, orgB.ID)
	req := laneASSOExchange(t, app, attempt.code, map[string]string{attempt.bindName: attempt.binding})

	// Login's fallback (the default active membership, C, with its role), not
	// the organization whose SSO it was...
	claims, org := laneASessionClaims(t, req)
	assert.Equal(t, orgC.ID.String(), org)
	assert.Equal(t, orgC.ID, claims.OrganizationID)
	require.NotNil(t, claims.RoleID)
	assert.Equal(t, roleC.ID, *claims.RoleID)
	laneAAssertBindingSpent(t, req, attempt, "/api/auth/refresh")
	// ...exactly what a password login gives the same user.
	loggedIn, loginOrg := laneASessionClaims(t, laneALogin(t, app, user.Email))
	assert.Equal(t, loginOrg, org)
	assert.Equal(t, loggedIn.OrganizationID, claims.OrganizationID)
	assert.Equal(t, loggedIn.RoleID, claims.RoleID)
}

func TestApp_Login_FallsBackToAnotherMembership(t *testing.T) {
	app := newTestApp(t)
	home := testutil.CreateTestOrganization(t, app.DB)
	orgB := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, home.ID, testutil.WithEmail(testutil.UniqueEmail("login-fallback")))
	roleB := laneAMembership(t, app, user.ID, orgB.ID)
	require.NoError(t, app.DB.Where("user_id = ? AND organization_id = ?", user.ID, home.ID).
		Delete(&models.UserOrganization{}).Error)

	claims, org := laneASessionClaims(t, laneALogin(t, app, user.Email))

	assert.Equal(t, orgB.ID.String(), org)
	assert.Equal(t, orgB.ID, claims.OrganizationID)
	require.NotNil(t, claims.RoleID)
	assert.Equal(t, roleB.ID, *claims.RoleID)
}

func TestLaneA_SSOExchange_IssuesWhatLoginIssues(t *testing.T) {
	app := newSSOApp(t)
	// Home organization A (active), also a member of B with another role; the
	// sign-in goes through B's SSO.
	fake := newFakeOAuth(t)
	orgA := testutil.CreateTestOrganization(t, app.DB)
	orgB := testutil.CreateTestOrganization(t, app.DB)
	createCustomSSOProvider(t, app, orgB.ID, fake, func(p *models.SSOProvider) {
		p.AllowAutoCreate = false
	})
	roleA := testutil.CreateTestRole(t, app.DB, orgA.ID, "home-"+uuid.NewString()[:8], nil)
	user := testutil.CreateTestUser(t, app.DB, orgA.ID,
		testutil.WithEmail(testutil.UniqueEmail("same-as-login")), testutil.WithRoleID(&roleA.ID))
	laneAMembership(t, app, user.ID, orgB.ID)

	attempt := laneASSOCallbackFor(t, app, fake, user, orgB.ID)
	exchanged, exchangedOrg := laneASessionClaims(t,
		laneASSOExchange(t, app, attempt.code, map[string]string{attempt.bindName: attempt.binding}))
	loggedIn, loginOrg := laneASessionClaims(t, laneALogin(t, app, user.Email))

	assert.Equal(t, loginOrg, exchangedOrg)
	assert.Equal(t, loggedIn.OrganizationID, exchanged.OrganizationID)
	assert.Equal(t, loggedIn.RoleID, exchanged.RoleID)
	assert.Equal(t, loggedIn.IsSuperAdmin, exchanged.IsSuperAdmin)
	assert.Equal(t, orgA.ID, exchanged.OrganizationID)
	require.NotNil(t, exchanged.RoleID)
	assert.Equal(t, roleA.ID, *exchanged.RoleID)
}

// laneAExchangeOrdered posts the exchange with the cookies in this order in
// the Cookie header, as a browser sends them.
func laneAExchangeOrdered(t *testing.T, app *handlers.App, code string, cookies [][2]string) *fastglue.Request {
	t.Helper()
	req := testutil.NewJSONRequest(t, map[string]any{"sso_exchange": code})
	for _, cookie := range cookies {
		req.RequestCtx.Request.Header.SetCookie(cookie[0], cookie[1])
	}
	require.NoError(t, app.RefreshToken(req))
	return req
}

// laneAJarCookies lists a jar's cookies with the given ones first, in order.
func laneAJarCookies(jar map[string]string, first ...string) [][2]string {
	cookies := make([][2]string, 0, len(jar))
	for _, name := range first {
		if value, ok := jar[name]; ok {
			cookies = append(cookies, [2]string{name, value})
		}
	}
	for name, value := range jar {
		listed := false
		for _, f := range first {
			listed = listed || f == name
		}
		if !listed {
			cookies = append(cookies, [2]string{name, value})
		}
	}
	return cookies
}

func TestLaneA_SSOExchange_TwoBindingsInOneRequest(t *testing.T) {
	// Callbacks A then B in one browser. The first exchange carries both
	// bindings, A's first in the Cookie header; it reads and spends only its
	// own, and the other attempt's exchange (carrying its own binding) then
	// succeeds.
	for _, bFirst := range []bool{true, false} {
		name := "A first"
		if bFirst {
			name = "B first"
		}
		t.Run(name, func(t *testing.T) {
			app := newSSOApp(t)
			jar := map[string]string{}
			a := laneASSOCallback(t, app)
			laneAApplyCookies(jar, a.callback)
			b := laneASSOCallback(t, app)
			laneAApplyCookies(jar, b.callback)
			require.Len(t, jar, 2)

			first, second := b, a
			if !bFirst {
				first, second = a, b
			}
			exchanged := laneAExchangeOrdered(t, app, first.code, laneAJarCookies(jar, a.bindName, b.bindName))
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(exchanged))
			laneAAssertBindingSpent(t, exchanged, first, "/api/auth/refresh")
			laneAApplyCookies(jar, exchanged)
			assert.Equal(t, second.binding, jar[second.bindName])

			// The other then succeeds, and the jar ends with its session.
			exchanged = laneAExchangeOrdered(t, app, second.code, laneAJarCookies(jar, a.bindName, b.bindName))
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(exchanged))
			laneAAssertBindingSpent(t, exchanged, second, "/api/auth/refresh")
			laneAApplyCookies(jar, exchanged)
			for _, cookie := range []string{"whm_access", "whm_refresh", "whm_csrf"} {
				assert.Equal(t, testutil.GetResponseCookie(exchanged, cookie), jar[cookie], cookie)
			}
			assert.Equal(t, second.user.ID, laneAAccessClaims(t, jar["whm_access"]).UserID)
		})
	}
}

func TestLaneA_SSOExchange_AnotherAttemptsBindingDoesNotCount(t *testing.T) {
	app := newSSOApp(t)
	a := laneASSOCallback(t, app)
	b := laneASSOCallback(t, app)

	// A's code with only B's binding: refused, and A's attempt is spent.
	req := laneASSOExchange(t, app, a.code, map[string]string{b.bindName: b.binding})
	testutil.AssertErrorResponse(t, req, fasthttp.StatusUnauthorized, "Invalid or expired sign-in")
	assert.Empty(t, laneASessionCookies(req))
	laneAAssertBindingSpent(t, req, a, "/api/auth/refresh")
}

type laneAFaultKey struct{}

func TestLaneA_SSOExchange_MembershipLookupFaultAnswers503(t *testing.T) {
	app := newSSOApp(t)
	// Signing in through the SSO of an organization that is not the user's
	// own...
	fake := newFakeOAuth(t)
	home := testutil.CreateTestOrganization(t, app.DB)
	ssoOrg := testutil.CreateTestOrganization(t, app.DB)
	createCustomSSOProvider(t, app, ssoOrg.ID, fake, func(p *models.SSOProvider) {
		p.AllowAutoCreate = false
	})
	user := testutil.CreateTestUser(t, app.DB, home.ID, testutil.WithEmail(testutil.UniqueEmail("membership-fault")))
	laneAMembership(t, app, user.ID, ssoOrg.ID)
	attempt := laneASSOCallbackFor(t, app, fake, user, ssoOrg.ID)

	// ...the user loads, then the membership lookup fails. The fault is
	// injected on a gorm instance of this test's own (the same pool), so no
	// other test sees it.
	live := app.DB
	pool, err := live.DB()
	require.NoError(t, err)
	isolated, err := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	require.NoError(t, isolated.Callback().Query().Before("gorm:query").Register("lane_a:membership_fault", func(tx *gorm.DB) {
		if tx.Statement.Table == "user_organizations" && tx.Statement.Context.Value(laneAFaultKey{}) != nil {
			_ = tx.AddError(errors.New("injected membership lookup fault"))
		}
	}))
	app.DB = isolated.WithContext(context.WithValue(context.Background(), laneAFaultKey{}, true))
	defer func() { app.DB = live }()

	req := laneASSOExchange(t, app, attempt.code, map[string]string{attempt.bindName: attempt.binding})
	testutil.AssertErrorResponse(t, req, fasthttp.StatusServiceUnavailable, "temporarily unavailable")
	assert.Empty(t, laneASessionCookies(req))
	laneAAssertBindingSpent(t, req, attempt, "/api/auth/refresh")
}

func TestLaneA_SSOExchange_RefusedForAnInactiveResellerMembership(t *testing.T) {
	app := newSSOApp(t)
	attempt := laneASSOCallback(t, app)
	// The membership becomes one derived from a reseller that no longer
	// grants it (as Login sees it: not an active membership).
	require.NoError(t, app.DB.Model(&models.UserOrganization{}).
		Where("user_id = ? AND organization_id = ?", attempt.user.ID, attempt.orgID).
		Updates(map[string]any{"source": models.MembershipSourceReseller, "reseller_member_id": nil}).Error)

	req := laneASSOExchange(t, app, attempt.code, map[string]string{attempt.bindName: attempt.binding})
	testutil.AssertErrorResponse(t, req, fasthttp.StatusUnauthorized, "not a member of this organization")
	assert.Empty(t, laneASessionCookies(req))
	laneAAssertBindingSpent(t, req, attempt, "/api/auth/refresh")
}

func TestLaneA_SSOExchange_SuperAdminWithAResellerMembership(t *testing.T) {
	app := newSSOApp(t)
	// A super admin (own organization S) with a reseller-derived membership of
	// T, signing in through T's SSO: as for a login, a super admin's
	// membership counts whatever its source.
	fake := newFakeOAuth(t)
	orgS := testutil.CreateTestOrganization(t, app.DB)
	orgT := testutil.CreateTestOrganization(t, app.DB)
	createCustomSSOProvider(t, app, orgT.ID, fake, func(p *models.SSOProvider) {
		p.AllowAutoCreate = false
	})
	admin := testutil.CreateTestUser(t, app.DB, orgS.ID, testutil.WithEmail(testutil.UniqueEmail("super-reseller")))
	require.NoError(t, app.DB.Model(&models.User{}).Where("id = ?", admin.ID).Update("is_super_admin", true).Error)
	admin.IsSuperAdmin = true
	require.NoError(t, app.DB.Create(&models.UserOrganization{
		UserID: admin.ID, OrganizationID: orgT.ID, Source: models.MembershipSourceReseller,
	}).Error)

	attempt := laneASSOCallbackFor(t, app, fake, admin, orgT.ID)
	req := laneASSOExchange(t, app, attempt.code, map[string]string{attempt.bindName: attempt.binding})

	claims, _ := laneASessionClaims(t, req)
	assert.True(t, claims.IsSuperAdmin)
	laneAAssertBindingSpent(t, req, attempt, "/api/auth/refresh")
}

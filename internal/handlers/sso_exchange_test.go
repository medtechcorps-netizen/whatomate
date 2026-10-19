package handlers_test

import (
	"context"
	"encoding/json"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

// laneASSOCallback runs a successful SSO callback for an existing user and
// returns the one-time code (from the redirect), the binding cookie and the
// callback request.
func laneASSOCallback(t *testing.T, app *handlers.App) (string, string, *models.User, *fastglue.Request) {
	t.Helper()
	fake := newFakeOAuth(t)
	email := testutil.UniqueEmail("sso-exchange")
	fake.UserEmail = email
	org := testutil.CreateTestOrganization(t, app.DB)
	createCustomSSOProvider(t, app, org.ID, fake, func(p *models.SSOProvider) {
		p.AllowAutoCreate = false
	})
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithEmail(email))

	nonce := "exchange-" + uuid.NewString()
	state := handlers.SSOState{
		OrgID: org.ID.String(), Provider: "custom", Nonce: nonce, ExpiresAt: time.Now().Add(5 * time.Minute),
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
	binding := testutil.GetResponseCookie(req, "whm_sso_bind")
	require.NotEmpty(t, binding)
	return code, binding, user, req
}

// laneASSOExchange posts {"sso_exchange": code} to the refresh route, with
// the binding cookie when one is given.
func laneASSOExchange(t *testing.T, app *handlers.App, code, binding string) *fastglue.Request {
	t.Helper()
	req := testutil.NewJSONRequest(t, map[string]any{"sso_exchange": code})
	if binding != "" {
		req.RequestCtx.Request.Header.SetCookie("whm_sso_bind", binding)
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

func TestLaneA_SSOCallback_SetsNoSessionCookie(t *testing.T) {
	app := newSSOApp(t)
	code, _, _, req := laneASSOCallback(t, app)

	// The navigation carries no session: nothing it could race other tabs with.
	assert.Empty(t, laneASessionCookies(req))
	assert.NotContains(t, string(req.RequestCtx.Response.Header.Peek("Location")), "access_token")

	// The binding cookie: httpOnly, sent only to the exchange, for the code's life.
	bind := fasthttp.AcquireCookie()
	defer fasthttp.ReleaseCookie(bind)
	bind.SetKey("whm_sso_bind")
	require.True(t, req.RequestCtx.Response.Header.Cookie(bind))
	assert.True(t, bind.HTTPOnly())
	assert.Equal(t, "/api/auth/refresh", string(bind.Path()))
	assert.Equal(t, 60, bind.MaxAge())

	// The code is stored for at most a minute.
	ttl, err := app.Redis.TTL(context.Background(), "sso:exchange:"+code).Result()
	require.NoError(t, err)
	assert.Greater(t, ttl, time.Duration(0))
	assert.LessOrEqual(t, ttl, 60*time.Second)
}

func TestLaneA_SSOExchange_AnswersLikeLogin(t *testing.T) {
	app := newSSOApp(t)
	code, binding, user, _ := laneASSOCallback(t, app)

	req := laneASSOExchange(t, app, code, binding)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	// The session cookies, as a login sets them...
	assert.ElementsMatch(t, []string{"whm_access", "whm_refresh", "whm_csrf"}, laneASessionCookies(req))
	// ...and the login answer.
	var envelope struct {
		Data struct {
			ExpiresIn int `json:"expires_in"`
			User      struct {
				ID    string `json:"id"`
				Email string `json:"email"`
			} `json:"user"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &envelope))
	assert.Equal(t, 15*60, envelope.Data.ExpiresIn)
	assert.Equal(t, user.ID.String(), envelope.Data.User.ID)
	assert.Equal(t, user.Email, envelope.Data.User.Email)
	// The binding is spent.
	bind := fasthttp.AcquireCookie()
	defer fasthttp.ReleaseCookie(bind)
	bind.SetKey("whm_sso_bind")
	require.True(t, req.RequestCtx.Response.Header.Cookie(bind))
	assert.Empty(t, string(bind.Value()))
	assert.LessOrEqual(t, bind.MaxAge(), 0)

	// The issued refresh token is a live session.
	refreshed := laneARefresh(t, app, testutil.GetResponseCookie(req, "whm_refresh"))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(refreshed))
}

func TestLaneA_SSOExchange_CodeIsSingleUse(t *testing.T) {
	app := newSSOApp(t)
	code, binding, _, _ := laneASSOCallback(t, app)
	first := laneASSOExchange(t, app, code, binding)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(first))

	again := laneASSOExchange(t, app, code, binding)
	testutil.AssertErrorResponse(t, again, fasthttp.StatusUnauthorized, "Invalid or expired sign-in")
	assert.Empty(t, laneASessionCookies(again))
}

func TestLaneA_SSOExchange_RequiresTheBindingCookie(t *testing.T) {
	app := newSSOApp(t)

	// Without the binding cookie (another browser holding the URL).
	code, binding, _, _ := laneASSOCallback(t, app)
	missing := laneASSOExchange(t, app, code, "")
	testutil.AssertErrorResponse(t, missing, fasthttp.StatusUnauthorized, "Invalid or expired sign-in")
	assert.Empty(t, laneASessionCookies(missing))
	// The attempt spent the code: not even the right browser can use it now.
	late := laneASSOExchange(t, app, code, binding)
	testutil.AssertErrorResponse(t, late, fasthttp.StatusUnauthorized, "Invalid or expired sign-in")
	assert.Empty(t, laneASessionCookies(late))

	// With another browser's binding cookie.
	code2, _, _, _ := laneASSOCallback(t, app)
	wrong := laneASSOExchange(t, app, code2, "binding-of-another-browser")
	testutil.AssertErrorResponse(t, wrong, fasthttp.StatusUnauthorized, "Invalid or expired sign-in")
	assert.Empty(t, laneASessionCookies(wrong))
}

func TestLaneA_SSOExchange_ExpiredCodeRejected(t *testing.T) {
	app := newSSOApp(t)
	code, binding, _, _ := laneASSOCallback(t, app)
	require.NoError(t, app.Redis.PExpire(context.Background(), "sso:exchange:"+code, 50*time.Millisecond).Err())
	time.Sleep(200 * time.Millisecond)

	req := laneASSOExchange(t, app, code, binding)
	testutil.AssertErrorResponse(t, req, fasthttp.StatusUnauthorized, "Invalid or expired sign-in")
	assert.Empty(t, laneASessionCookies(req))
}

func TestLaneA_SSOExchange_DisabledUserRejected(t *testing.T) {
	app := newSSOApp(t)
	code, binding, user, _ := laneASSOCallback(t, app)
	require.NoError(t, app.DB.Model(&models.User{}).Where("id = ?", user.ID).Update("is_active", false).Error)

	req := laneASSOExchange(t, app, code, binding)
	testutil.AssertErrorResponse(t, req, fasthttp.StatusUnauthorized, "Account is disabled")
	assert.Empty(t, laneASessionCookies(req))
}

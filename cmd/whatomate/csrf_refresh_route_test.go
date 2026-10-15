package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/middleware"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

// laneACookieJar keeps the latest value of each auth cookie a response set.
type laneACookieJar map[string]string

func (jar laneACookieJar) absorb(t *testing.T, ctx *fasthttp.RequestCtx) {
	t.Helper()
	ctx.Response.Header.VisitAllCookie(func(key, value []byte) {
		cookie := fasthttp.AcquireCookie()
		defer fasthttp.ReleaseCookie(cookie)
		require.NoError(t, cookie.ParseBytes(value))
		jar[string(key)] = string(cookie.Value())
	})
}

func laneAInvoke(
	g *fastglue.Fastglue,
	method, path string,
	cookies map[string]string,
	headers map[string]string,
	body any,
) *fasthttp.RequestCtx {
	ctx := &fasthttp.RequestCtx{}
	ctx.Request.Header.SetMethod(method)
	ctx.Request.SetRequestURI(path)
	for name, value := range cookies {
		ctx.Request.Header.SetCookie(name, value)
	}
	for name, value := range headers {
		ctx.Request.Header.Set(name, value)
	}
	if body != nil {
		encoded, _ := json.Marshal(body)
		ctx.Request.Header.SetContentType("application/json")
		ctx.Request.SetBody(encoded)
	}
	g.Handler()(ctx)
	return ctx
}

// TestCSRFExemptsTokenRefreshThroughMiddlewareChain is the regression test
// for the silent-refresh logout: a refresh that carries a live whm_access
// cookie (path /api covers /api/auth/refresh) but no X-CSRF-Token header must
// not be rejected by the CSRF middleware, while other mutating cookie-auth
// requests still are.
func TestCSRFExemptsTokenRefreshThroughMiddlewareChain(t *testing.T) {
	db := testutil.SetupTestDB(t)
	rdb := testutil.SetupTestRedis(t)
	if rdb == nil {
		t.Skip("TEST_REDIS_URL not set, skipping test")
	}
	org := testutil.CreateTestOrganization(t, db)
	const password = "lane-a-refresh-password"
	user := testutil.CreateTestUser(
		t,
		db,
		org.ID,
		testutil.WithEmail(testutil.UniqueEmail("lane-a-csrf-refresh")),
		testutil.WithPassword(password),
	)

	cfg := &config.Config{
		JWT: config.JWTConfig{
			Secret:            testutil.TestJWTSecret,
			AccessExpiryMins:  15,
			RefreshExpiryDays: 7,
		},
	}
	app := &handlers.App{
		Config:     cfg,
		DB:         db,
		Redis:      rdb,
		Log:        testutil.NopLogger(),
		HTTPClient: http.DefaultClient,
	}
	g := fastglue.NewGlue()
	g.Before(middleware.SecurityHeaders())
	g.Before(middleware.Recovery(app.Log))
	g.Before(middleware.CSRFProtection())
	setupRoutes(g, app, app.Log, "", rdb, cfg)

	jar := laneACookieJar{}
	login := laneAInvoke(g, fasthttp.MethodPost, "/api/auth/login", nil, nil, map[string]string{
		"email":    user.Email,
		"password": password,
	})
	require.Equal(t, fasthttp.StatusOK, login.Response.StatusCode(), string(login.Response.Body()))
	jar.absorb(t, login)
	require.NotEmpty(t, jar["whm_access"])
	require.NotEmpty(t, jar["whm_refresh"])
	require.NotEmpty(t, jar["whm_csrf"])

	// Control: another mutating cookie-auth request without the header is
	// still rejected, so the middleware is active in this chain.
	control := laneAInvoke(g, fasthttp.MethodPost, "/api/auth/switch-org",
		map[string]string{"whm_access": jar["whm_access"], "whm_csrf": jar["whm_csrf"]}, nil,
		map[string]string{"organization_id": org.ID.String()})
	require.Equal(t, fasthttp.StatusForbidden, control.Response.StatusCode())
	assert.Contains(t, string(control.Response.Body()), "CSRF token mismatch")

	// The refresh itself, with the fresh access cookie attached and no header.
	refresh := laneAInvoke(g, fasthttp.MethodPost, "/api/auth/refresh", map[string]string{
		"whm_access":  jar["whm_access"],
		"whm_refresh": jar["whm_refresh"],
		"whm_csrf":    jar["whm_csrf"],
	}, nil, map[string]any{})
	require.Equal(t, fasthttp.StatusOK, refresh.Response.StatusCode(), string(refresh.Response.Body()))
	previousRefresh := jar["whm_refresh"]
	jar.absorb(t, refresh)
	assert.NotEqual(t, previousRefresh, jar["whm_refresh"], "refresh token must rotate")

	var envelope struct {
		Data struct {
			ExpiresIn int `json:"expires_in"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(refresh.Response.Body(), &envelope))
	assert.Equal(t, 15*60, envelope.Data.ExpiresIn)
}

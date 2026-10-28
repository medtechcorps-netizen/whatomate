package main

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/middleware"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

// The standard cookie jar applies actual path/Secure matching. The in-process
// route invocation uses the path after the server's configured base-path strip.
// This does not substitute for browser delayed-Set-Cookie qualification.
func TestSwitchRefreshChildRouteCookiePathAndMiddleware(t *testing.T) {
	for _, basePath := range []string{"", "/rereply"} {
		t.Run("base"+basePath, func(t *testing.T) {
			db := testutil.SetupTestDB(t)
			rdb := testutil.SetupTestRedis(t)
			if rdb == nil {
				t.Skip("TEST_REDIS_URL not set")
			}
			home := testutil.CreateTestOrganization(t, db)
			target := testutil.CreateTestOrganization(t, db)
			user := testutil.CreateTestUser(t, db, home.ID, testutil.WithPassword("switch-route-password"))
			require.NoError(t, db.Create(&models.UserOrganization{BaseModel: models.BaseModel{ID: uuid.New()},
				UserID: user.ID, OrganizationID: target.ID}).Error)
			cfg := &config.Config{JWT: config.JWTConfig{Secret: testutil.TestJWTSecret, AccessExpiryMins: 15, RefreshExpiryDays: 1}}
			cfg.Server.BasePath = basePath
			cfg.Cookie.Secure = true
			app := &handlers.App{DB: db, Redis: rdb, Config: cfg, Log: testutil.NopLogger(), HTTPClient: http.DefaultClient}
			g := fastglue.NewGlue()
			g.Before(middleware.CSRFProtection())
			setupRoutes(g, app, app.Log, basePath, rdb, cfg)
			login := laneAInvoke(g, fasthttp.MethodPost, "/api/auth/login", nil, nil,
				map[string]string{"email": user.Email, "password": "switch-route-password"})
			require.Equal(t, fasthttp.StatusOK, login.Response.StatusCode(), string(login.Response.Body()))
			response := &http.Response{Header: make(http.Header)}
			login.Response.Header.VisitAllCookie(func(_, raw []byte) { response.Header.Add("Set-Cookie", string(raw)) })
			jar, err := cookiejar.New(nil)
			require.NoError(t, err)
			origin, err := url.Parse("https://switch.example" + basePath + "/api/auth/login")
			require.NoError(t, err)
			jar.SetCookies(origin, response.Cookies())
			var oldRefresh string
			for _, cookie := range response.Cookies() {
				if cookie.Name == "whm_refresh" {
					assert.Equal(t, basePath+"/api/auth/refresh", cookie.Path)
					assert.True(t, cookie.HttpOnly)
					assert.True(t, cookie.Secure)
					assert.Equal(t, http.SameSiteLaxMode, cookie.SameSite)
					assert.Equal(t, 86400, cookie.MaxAge)
					oldRefresh = cookie.Value
				}
			}
			require.NotEmpty(t, oldRefresh)
			cookiesFor := func(path string) map[string]string {
				address, parseErr := url.Parse("https://switch.example" + basePath + path)
				require.NoError(t, parseErr)
				cookies := map[string]string{}
				for _, cookie := range jar.Cookies(address) {
					cookies[cookie.Name] = cookie.Value
				}
				return cookies
			}
			legacyCookies := cookiesFor("/api/auth/switch-org")
			assert.Empty(t, legacyCookies["whm_refresh"])
			legacy := laneAInvoke(g, fasthttp.MethodPost, "/api/auth/switch-org", legacyCookies,
				map[string]string{"X-CSRF-Token": legacyCookies["whm_csrf"]}, map[string]any{"organization_id": target.ID})
			assert.Equal(t, fasthttp.StatusConflict, legacy.Response.StatusCode())
			assert.Contains(t, string(legacy.Response.Body()), "switch_client_update_required")
			assert.Empty(t, legacy.Response.Header.Peek("Set-Cookie"))
			childCookies := cookiesFor("/api/auth/refresh/switch-org")
			assert.Equal(t, oldRefresh, childCookies["whm_refresh"])
			for _, headers := range []map[string]string{nil, {"Authorization": "Bearer " + childCookies["whm_access"]}} {
				forged := laneAInvoke(g, fasthttp.MethodPost, "/api/auth/refresh/switch-org", childCookies, headers,
					map[string]any{"organization_id": target.ID})
				assert.Equal(t, fasthttp.StatusForbidden, forged.Response.StatusCode(), "header auth cannot bypass switch cookie CSRF")
				assert.Empty(t, forged.Response.Header.Peek("Set-Cookie"))
			}
			anonymous := laneAInvoke(g, fasthttp.MethodPost, "/api/auth/refresh/switch-org",
				map[string]string{"whm_refresh": oldRefresh, "whm_csrf": childCookies["whm_csrf"]},
				map[string]string{"X-CSRF-Token": childCookies["whm_csrf"]}, map[string]any{"organization_id": target.ID})
			assert.Equal(t, fasthttp.StatusUnauthorized, anonymous.Response.StatusCode(), "child must remain a protected route")
			switched := laneAInvoke(g, fasthttp.MethodPost, "/api/auth/refresh/switch-org", childCookies,
				map[string]string{"X-CSRF-Token": childCookies["whm_csrf"]}, map[string]any{"organization_id": target.ID})
			require.Equal(t, fasthttp.StatusOK, switched.Response.StatusCode(), string(switched.Response.Body()))
			old := laneAInvoke(g, fasthttp.MethodPost, "/api/auth/refresh", map[string]string{"whm_refresh": oldRefresh}, nil, map[string]any{})
			assert.Equal(t, fasthttp.StatusUnauthorized, old.Response.StatusCode())
		})
	}
}

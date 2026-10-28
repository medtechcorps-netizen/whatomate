package main

import (
	"context"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/middleware"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func TestSessionTerminationThroughExhaustedAPIRateLimits(t *testing.T) {
	g, redis, _, jar := laneARouteStack(t, func(cfg *config.Config) {
		cfg.RateLimit = config.RateLimitConfig{Enabled: true, LoginMaxAttempts: 10, RegisterMaxAttempts: 10, RefreshMaxAttempts: 1, WindowSeconds: 60, APIMaxRequests: 2, APIWindowSeconds: 60}
	})
	parse := func(token string) *middleware.JWTClaims {
		parsed, err := jwt.ParseWithClaims(token, &middleware.JWTClaims{}, func(*jwt.Token) (any, error) { return []byte(testutil.TestJWTSecret), nil })
		require.NoError(t, err)
		return parsed.Claims.(*middleware.JWTClaims)
	}
	access := parse(jar["whm_access"])
	access.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Hour))
	expiredAccess, err := jwt.NewWithClaims(jwt.SigningMethodHS256, access).SignedString([]byte(testutil.TestJWTSecret))
	require.NoError(t, err)
	jar["whm_access"] = expiredAccess

	// Login used the first global slot. Refresh fills both the API and
	// refresh quotas, despite the access token being expired.
	refresh := laneAInvoke(g, fasthttp.MethodPost, "/api/auth/refresh", jar, nil, map[string]any{})
	require.Equal(t, fasthttp.StatusOK, refresh.Response.StatusCode(), string(refresh.Response.Body()))
	jar.absorb(t, refresh)
	activeAccess := jar["whm_access"]
	jar["whm_access"] = expiredAccess
	jti := parse(jar["whm_refresh"]).ID
	blocked := laneAInvoke(g, fasthttp.MethodPost, "/api/auth/refresh", jar, nil, map[string]any{})
	require.Equal(t, fasthttp.StatusTooManyRequests, blocked.Response.StatusCode())

	// CSRF proof is still needed before taking the independent quota.
	forged := laneAInvoke(g, fasthttp.MethodPost, "/api/auth/refresh", jar, nil, map[string]any{"revoke": true})
	assert.Equal(t, fasthttp.StatusTooManyRequests, forged.Response.StatusCode())
	exists, err := redis.Exists(context.Background(), "refresh:"+jti).Result()
	require.NoError(t, err)
	require.Equal(t, int64(1), exists)

	headers := map[string]string{"X-CSRF-Token": jar["whm_csrf"]}
	revoke := laneAInvoke(g, fasthttp.MethodPost, "/api/auth/refresh", jar, headers, map[string]any{"revoke": true, "keep_cookies": true})
	require.Equal(t, fasthttp.StatusOK, revoke.Response.StatusCode(), string(revoke.Response.Body()))
	revoke.Response.Header.VisitAllCookie(func(_, _ []byte) { t.Error("keep_cookies revocation must set no cookies") })
	exists, err = redis.Exists(context.Background(), "refresh:"+jti).Result()
	require.NoError(t, err)
	assert.Zero(t, exists)

	// The valid credential has only one independently bounded termination slot.
	overLimit := laneAInvoke(g, fasthttp.MethodPost, "/api/auth/refresh", jar, headers, map[string]any{"revoke": true, "keep_cookies": true})
	assert.Equal(t, fasthttp.StatusTooManyRequests, overLimit.Response.StatusCode())
	assert.NotEmpty(t, overLimit.Response.Header.Peek("Retry-After"))

	// Normal two-phase logout has an unexpired access cookie. Its cleanup
	// must not reuse the revocation slot just consumed at Max=1.
	activeCookies := map[string]string{"whm_access": activeAccess, "whm_csrf": jar["whm_csrf"]}
	activeLogout := laneAInvoke(g, fasthttp.MethodPost, "/api/auth/logout", activeCookies, headers, map[string]any{})
	require.Equal(t, fasthttp.StatusOK, activeLogout.Response.StatusCode(), string(activeLogout.Response.Body()))
	activeCleared := laneACookieJar{}
	activeCleared.absorb(t, activeLogout)
	require.Contains(t, activeCleared, "whm_refresh")
	assert.Empty(t, activeCleared["whm_access"])
	assert.Empty(t, activeCleared["whm_refresh"])
	assert.Empty(t, activeCleared["whm_csrf"])
	activeAgain := laneAInvoke(g, fasthttp.MethodPost, "/api/auth/logout", activeCookies, headers, map[string]any{})
	assert.Equal(t, fasthttp.StatusTooManyRequests, activeAgain.Response.StatusCode(), "authenticated cleanup retains its own finite quota")

	// The browser's refresh cookie path does not include /auth/logout.
	// Expired-access cleanup uses a separate bounded IP quota, not claims
	// from that expired token and not the already exhausted global bucket.
	logoutCookies := map[string]string{"whm_access": expiredAccess, "whm_csrf": jar["whm_csrf"]}
	invalidCredential := laneAInvoke(g, fasthttp.MethodPost, "/api/auth/logout", logoutCookies, headers, map[string]string{"refresh_token": "unsigned-credential"})
	assert.Equal(t, fasthttp.StatusTooManyRequests, invalidCredential.Response.StatusCode(), "anonymous cleanup cannot carry a refresh credential to revoke")
	logout := laneAInvoke(g, fasthttp.MethodPost, "/api/auth/logout", logoutCookies, headers, map[string]any{})
	require.Equal(t, fasthttp.StatusOK, logout.Response.StatusCode(), string(logout.Response.Body()))
	cleared := laneACookieJar{}
	cleared.absorb(t, logout)
	require.Contains(t, cleared, "whm_refresh")
	assert.Empty(t, cleared["whm_access"])
	assert.Empty(t, cleared["whm_refresh"])
	assert.Empty(t, cleared["whm_csrf"])
	again := laneAInvoke(g, fasthttp.MethodPost, "/api/auth/logout", logoutCookies, headers, map[string]string{"user_id": "arbitrary-owner"})
	assert.Equal(t, fasthttp.StatusTooManyRequests, again.Response.StatusCode(), "unsigned body identities must not select another quota")
}

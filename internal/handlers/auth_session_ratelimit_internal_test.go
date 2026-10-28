package handlers

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/shridarpatil/whatomate/internal/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"github.com/zerodha/logf"
)

const terminationTestSecret = "session-termination-test-secret"

func terminationJSONRequest(body string) *fastglue.Request {
	req := &fastglue.Request{RequestCtx: &fasthttp.RequestCtx{}}
	req.RequestCtx.Request.Header.SetMethod(fasthttp.MethodPost)
	req.RequestCtx.Request.Header.SetContentType("application/json")
	req.RequestCtx.Request.SetBodyString(body)
	return req
}

func terminationTestToken(t *testing.T, userID uuid.UUID, subject string, expires time.Time, secret string) string {
	t.Helper()
	claims := middleware.JWTClaims{UserID: userID, OrganizationID: uuid.New(), RegisteredClaims: jwt.RegisteredClaims{
		Subject: subject, ExpiresAt: jwt.NewNumericDate(expires),
	}}
	if subject == middleware.JWTSubjectRefresh {
		claims.ID = uuid.NewString()
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	require.NoError(t, err)
	return token
}

func terminationTestRequest(t *testing.T, token string) *fastglue.Request {
	t.Helper()
	req := terminationJSONRequest(`{"revoke":true,"keep_cookies":true}`)
	req.RequestCtx.Request.SetRequestURI("/api/auth/refresh")
	req.RequestCtx.Request.Header.SetMethod(fasthttp.MethodPost)
	req.RequestCtx.Request.Header.SetCookie("whm_refresh", token)
	req.RequestCtx.Request.Header.SetCookie("whm_csrf", "bound-csrf")
	req.RequestCtx.Request.Header.Set("X-CSRF-Token", "bound-csrf")
	return req
}

func TestSessionTerminationClassificationKeepsCredentialAndInputBoundaries(t *testing.T) {
	userID := uuid.New()
	token := terminationTestToken(t, userID, middleware.JWTSubjectRefresh, time.Now().Add(time.Hour), terminationTestSecret)
	for _, tc := range []struct {
		name    string
		change  func(*fastglue.Request)
		allowed bool
	}{
		{"valid refresh without access", func(*fastglue.Request) {}, true},
		{"valid refresh with expired access", func(r *fastglue.Request) {
			r.RequestCtx.Request.Header.SetCookie("whm_access", terminationTestToken(t, userID, middleware.JWTSubjectAccess, time.Now().Add(-time.Hour), terminationTestSecret))
		}, true},
		{"csrf missing", func(r *fastglue.Request) { r.RequestCtx.Request.Header.Del("X-CSRF-Token") }, false},
		{"csrf mismatch", func(r *fastglue.Request) { r.RequestCtx.Request.Header.Set("X-CSRF-Token", "different") }, false},
		{"ordinary refresh", func(r *fastglue.Request) { r.RequestCtx.Request.SetBodyString("{}") }, false},
		{"mixed SSO mode", func(r *fastglue.Request) {
			r.RequestCtx.Request.SetBodyString("{\"revoke\":true,\"sso_exchange\":\"grant\"}")
		}, false},
		{"wrong content type", func(r *fastglue.Request) { r.RequestCtx.Request.Header.SetContentType("text/plain") }, false},
		{"malformed JSON", func(r *fastglue.Request) { r.RequestCtx.Request.SetBodyString("{\"revoke\":true") }, false},
		{"wrong field type", func(r *fastglue.Request) {
			r.RequestCtx.Request.SetBodyString("{\"revoke\":true,\"keep_cookies\":\"true\"}")
		}, false},
		{"oversized", func(r *fastglue.Request) {
			r.RequestCtx.Request.SetBodyString("{\"revoke\":true,\"padding\":\"" + strings.Repeat("x", 16*1024) + "\"}")
		}, false},
		{"wrong method", func(r *fastglue.Request) { r.RequestCtx.Request.Header.SetMethod(fasthttp.MethodGet) }, false},
		{"wrong path", func(r *fastglue.Request) { r.RequestCtx.Request.SetRequestURI("/api/auth/switch-org") }, false},
		{"forged credential", func(r *fastglue.Request) {
			r.RequestCtx.Request.Header.SetCookie("whm_refresh", terminationTestToken(t, userID, middleware.JWTSubjectRefresh, time.Now().Add(time.Hour), "wrong-secret"))
		}, false},
		{"expired refresh", func(r *fastglue.Request) {
			r.RequestCtx.Request.Header.SetCookie("whm_refresh", terminationTestToken(t, userID, middleware.JWTSubjectRefresh, time.Now().Add(-time.Hour), terminationTestSecret))
		}, false},
		{"access credential cannot revoke", func(r *fastglue.Request) {
			r.RequestCtx.Request.Header.SetCookie("whm_refresh", terminationTestToken(t, userID, middleware.JWTSubjectAccess, time.Now().Add(time.Hour), terminationTestSecret))
		}, false},
		{"bad cookie overrides good body", func(r *fastglue.Request) {
			r.RequestCtx.Request.Header.SetCookie("whm_refresh", "forged")
			r.RequestCtx.Request.SetBodyString("{\"revoke\":true,\"refresh_token\":\"" + token + "\"}")
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := terminationTestRequest(t, token)
			tc.change(req)
			got, ok := sessionTerminationUser(req, terminationTestSecret)
			assert.Equal(t, tc.allowed, ok)
			if ok {
				assert.Equal(t, userID, got)
			} else {
				assert.Equal(t, uuid.Nil, got)
			}
			assert.Nil(t, req.RequestCtx.UserValue(middleware.ContextKeyUserID))
			classifiedUser, cleanup := SessionTerminationClassifier(terminationTestSecret)(req)
			assert.Equal(t, got, classifiedUser)
			assert.False(t, cleanup, "refresh/revoke requests never enter cookie-only cleanup")
		})
	}
}

func TestAPISessionRateLimitConcurrentUsersHaveIndependentBoundedQuotas(t *testing.T) {
	redisURL := os.Getenv("TEST_REDIS_URL")
	if redisURL == "" {
		t.Skip("TEST_REDIS_URL not set")
	}
	redisOpts, err := redis.ParseURL(redisURL)
	require.NoError(t, err)
	client := redis.NewClient(redisOpts)
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, client.Ping(ctx).Err())
	prefix := "termination-" + uuid.NewString()
	opts := middleware.RateLimitOpts{Redis: client, Log: logf.New(logf.Opts{Level: logf.ErrorLevel}), Max: 2, Window: time.Minute, KeyPrefix: prefix}
	apiOpts := opts
	apiOpts.KeyPrefix += "-api"
	apiOpts.Max = 1
	limit := middleware.APISessionRateLimit(apiOpts, opts, SessionTerminationClassifier(terminationTestSecret))

	// Fill the same unauthenticated IP's global bucket before any revocation.
	ordinary := terminationJSONRequest("{}")
	ordinary.RequestCtx.Request.SetRequestURI("/api/auth/refresh")
	require.NotNil(t, limit(ordinary))
	blocked := terminationJSONRequest("{}")
	blocked.RequestCtx.Request.SetRequestURI("/api/auth/refresh")
	require.Nil(t, limit(blocked))

	type result struct {
		user    int
		allowed bool
	}
	results := make(chan result, 20)
	for user := 0; user < 2; user++ {
		token := terminationTestToken(t, uuid.New(), middleware.JWTSubjectRefresh, time.Now().Add(time.Hour), terminationTestSecret)
		for i := 0; i < 10; i++ {
			req := terminationTestRequest(t, token)
			go func(user int) { results <- result{user, limit(req) != nil} }(user)
		}
	}
	counts := [2]int{}
	for i := 0; i < 20; i++ {
		got := <-results
		if got.allowed {
			counts[got.user]++
		}
	}
	assert.Equal(t, [2]int{2, 2}, counts, "shared NAT traffic cannot consume another user's revocation quota")

	t.Run("anonymous cleanup cannot select a signed user quota through proxy text", func(t *testing.T) {
		cleanupOpts := opts
		cleanupOpts.TrustProxy = true
		cleanupOpts.Max = 1
		cleanupAPI := apiOpts
		cleanupAPI.TrustProxy = true
		cleanup := middleware.APISessionRateLimit(cleanupAPI, cleanupOpts, SessionTerminationClassifier(terminationTestSecret))
		userID := uuid.New()
		makeCleanup := func(access, forwarded string) *fastglue.Request {
			req := terminationTestRequest(t, "")
			req.RequestCtx.Request.SetRequestURI("/api/auth/logout")
			req.RequestCtx.Request.SetBodyString("{}")
			req.RequestCtx.Request.Header.SetCookie("whm_access", access)
			req.RequestCtx.Request.Header.Set("X-Forwarded-For", forwarded)
			return req
		}
		forgedIP := "u:" + userID.String()
		require.NotNil(t, cleanup(makeCleanup("unsigned", forgedIP)))
		access := terminationTestToken(t, userID, middleware.JWTSubjectAccess, time.Now().Add(time.Hour), terminationTestSecret)
		require.NotNil(t, cleanup(makeCleanup(access, "192.0.2.1")), "anonymous proxy text must not exhaust the signed user's cleanup quota")
		assert.Nil(t, cleanup(makeCleanup(access, "192.0.2.1")), "the signed user retains an independent finite quota")
		assert.Nil(t, cleanup(makeCleanup("unsigned", forgedIP)), "anonymous cleanup also retains a finite quota")
	})
}

func TestSessionTerminationExpiredLogoutCannotChooseUserQuota(t *testing.T) {
	for _, secret := range []string{terminationTestSecret, "forged-secret"} {
		req := terminationTestRequest(t, "")
		req.RequestCtx.Request.SetRequestURI("/api/auth/logout")
		req.RequestCtx.Request.SetBodyString("{\"user_id\":\"" + uuid.NewString() + "\"}")
		req.RequestCtx.Request.Header.SetCookie("whm_access", terminationTestToken(t, uuid.New(), middleware.JWTSubjectAccess, time.Now().Add(-time.Hour), secret))
		_, _, valid := sessionTerminationCredential(req)
		require.True(t, valid, "bounded CSRF-bound cookie cleanup is still permitted")
		userID, ok := sessionTerminationUser(req, terminationTestSecret)
		assert.False(t, ok)
		assert.Equal(t, uuid.Nil, userID, "no expired or unsigned claims may select a user quota")
		classifiedUser, cleanup := SessionTerminationClassifier(terminationTestSecret)(req)
		assert.Equal(t, uuid.Nil, classifiedUser)
		assert.True(t, cleanup, "valid credential-free CSRF cleanup remains bounded anonymously")
	}
}

func TestAPISessionRateLimitMissingClassifierFailsClosed(t *testing.T) {
	// Nil Redis makes any accidental fallback into the ordinary quotas fail;
	// the missing trusted proof provider must refuse before touching a quota.
	limit := middleware.APISessionRateLimit(middleware.RateLimitOpts{}, middleware.RateLimitOpts{}, nil)
	for _, path := range []string{"/api/auth/logout", "/api/auth/refresh", "/api/contacts"} {
		req := terminationJSONRequest(`{}`)
		req.RequestCtx.Request.SetRequestURI(path)
		require.Nil(t, limit(req))
		assert.Equal(t, fasthttp.StatusServiceUnavailable, req.RequestCtx.Response.StatusCode())
		assert.Empty(t, req.RequestCtx.Response.Header.Peek("Set-Cookie"))
	}
}

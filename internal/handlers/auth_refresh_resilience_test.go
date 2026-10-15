package handlers_test

import (
	"context"
	"os"
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

func laneASeedRefreshJTI(t *testing.T, client *redis.Client, userID uuid.UUID) string {
	t.Helper()
	jti := uuid.NewString()
	require.NoError(t, client.Set(context.Background(), "refresh:"+jti, userID.String(), time.Hour).Err())
	return jti
}

func laneAJTIExists(t *testing.T, client *redis.Client, jti string) bool {
	t.Helper()
	count, err := client.Exists(context.Background(), "refresh:"+jti).Result()
	require.NoError(t, err)
	return count == 1
}

func laneAKeyExists(t *testing.T, client *redis.Client, key string) bool {
	t.Helper()
	count, err := client.Exists(context.Background(), key).Result()
	require.NoError(t, err)
	return count == 1
}

// laneARefresh calls RefreshToken with the token in the whm_refresh cookie.
func laneARefresh(t *testing.T, app *handlers.App, token string) *fastglue.Request {
	t.Helper()
	req := testutil.NewJSONRequest(t, map[string]any{})
	req.RequestCtx.Request.Header.SetCookie("whm_refresh", token)
	require.NoError(t, app.RefreshToken(req))
	return req
}

// laneARevoke calls the revoke variant of RefreshToken.
func laneARevoke(t *testing.T, app *handlers.App, token, csrfCookie, csrfHeader string) *fastglue.Request {
	t.Helper()
	req := testutil.NewJSONRequest(t, map[string]any{"revoke": true})
	req.RequestCtx.Request.Header.SetCookie("whm_refresh", token)
	if csrfCookie != "" {
		req.RequestCtx.Request.Header.SetCookie("whm_csrf", csrfCookie)
	}
	if csrfHeader != "" {
		req.RequestCtx.Request.Header.Set("X-CSRF-Token", csrfHeader)
	}
	require.NoError(t, app.RefreshToken(req))
	return req
}

func laneAJTIOf(t *testing.T, token string) string {
	t.Helper()
	parsed, err := jwt.ParseWithClaims(token, &middleware.JWTClaims{}, func(*jwt.Token) (any, error) {
		return []byte(testutil.TestJWTSecret), nil
	})
	require.NoError(t, err)
	return parsed.Claims.(*middleware.JWTClaims).ID
}

// laneALiveRefreshKeysFor counts registered refresh tokens owned by userID.
func laneALiveRefreshKeysFor(t *testing.T, client *redis.Client, userID uuid.UUID) int {
	t.Helper()
	ctx := context.Background()
	count := 0
	iter := client.Scan(ctx, 0, "refresh:*", 500).Iterator()
	for iter.Next(ctx) {
		if value, err := client.Get(ctx, iter.Val()).Result(); err == nil && value == userID.String() {
			count++
		}
	}
	require.NoError(t, iter.Err())
	return count
}

type laneARefreshFixture struct {
	app   *handlers.App
	user  *models.User
	jti   string
	token string
}

func newLaneARefreshFixture(t *testing.T) *laneARefreshFixture {
	t.Helper()
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID)
	jti := laneASeedRefreshJTI(t, app.Redis, user.ID)
	token := generateRefreshTokenWithJTI(t, testutil.TestJWTSecret, user, jti, time.Hour)
	return &laneARefreshFixture{app: app, user: user, jti: jti, token: token}
}

func TestLaneA_RefreshToken_GraceStoresNoTokensAndOutlivesClientRetries(t *testing.T) {
	f := newLaneARefreshFixture(t)

	req := laneARefresh(t, f.app, f.token)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
	successor := laneAJTIOf(t, testutil.GetResponseCookie(req, "whm_refresh"))

	ctx := context.Background()
	grace, err := f.app.Redis.Get(ctx, "refresh_grace:"+f.jti).Result()
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(grace, successor+"|"), "grace entry names the successor JTI")
	assert.NotContains(t, grace, "eyJ", "no JWT is stored in Redis")

	ttl, err := f.app.Redis.TTL(ctx, "refresh_grace:"+f.jti).Result()
	require.NoError(t, err)
	assert.Greater(t, ttl, 5*time.Minute, "longer than the client's retry horizon")
	assert.LessOrEqual(t, ttl, 10*time.Minute)
	assert.False(t, laneAJTIExists(t, f.app.Redis, f.jti), "the old refresh token must be consumed")
	assert.True(t, laneAJTIExists(t, f.app.Redis, successor))
}

func TestLaneA_RefreshToken_ConcurrentRotationNeverLogsOut(t *testing.T) {
	f := newLaneARefreshFixture(t)

	const callers = 4
	var wg sync.WaitGroup
	results := make([]*fastglue.Request, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := testutil.NewJSONRequest(t, map[string]any{})
			req.RequestCtx.Request.Header.SetCookie("whm_refresh", f.token)
			_ = f.app.RefreshToken(req)
			results[i] = req
		}(i)
	}
	wg.Wait()

	successor := ""
	for i, req := range results {
		require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), "caller %d: %s", i, testutil.GetResponseBody(req))
		jti := laneAJTIOf(t, testutil.GetResponseCookie(req, "whm_refresh"))
		if successor == "" {
			successor = jti
		}
		assert.Equal(t, successor, jti, "every caller gets the same successor: one token chain")
	}
	assert.Equal(t, 1, laneALiveRefreshKeysFor(t, f.app.Redis, f.user.ID), "losers drop their orphan successors")
}

func TestLaneA_RefreshToken_GraceClosesWhenSuccessorRotates(t *testing.T) {
	f := newLaneARefreshFixture(t)

	first := laneARefresh(t, f.app, f.token)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(first))
	second := laneARefresh(t, f.app, testutil.GetResponseCookie(first, "whm_refresh"))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(second))

	assert.False(t, laneAKeyExists(t, f.app.Redis, "refresh_grace:"+f.jti), "rotating the successor closes the old grace entry")
	replay := laneARefresh(t, f.app, f.token)
	testutil.AssertErrorResponse(t, replay, fasthttp.StatusUnauthorized, "revoked")
}

func TestLaneA_RefreshToken_GraceRequiresLiveSuccessor(t *testing.T) {
	f := newLaneARefreshFixture(t)

	first := laneARefresh(t, f.app, f.token)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(first))
	successor := laneAJTIOf(t, testutil.GetResponseCookie(first, "whm_refresh"))
	require.NoError(t, f.app.Redis.Del(context.Background(), "refresh:"+successor).Err())

	replay := laneARefresh(t, f.app, f.token)
	testutil.AssertErrorResponse(t, replay, fasthttp.StatusUnauthorized, "revoked")
	assert.False(t, laneAKeyExists(t, f.app.Redis, "refresh_grace:"+f.jti))
}

func TestLaneA_RefreshToken_GraceReplayRechecksTheAccount(t *testing.T) {
	f := newLaneARefreshFixture(t)

	first := laneARefresh(t, f.app, f.token)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(first))
	successor := laneAJTIOf(t, testutil.GetResponseCookie(first, "whm_refresh"))

	require.NoError(t, f.app.DB.Model(f.user).Update("is_active", false).Error)

	replay := laneARefresh(t, f.app, f.token)
	testutil.AssertErrorResponse(t, replay, fasthttp.StatusUnauthorized, "disabled")
	assert.Empty(t, testutil.GetResponseCookie(replay, "whm_refresh"))
	assert.False(t, laneAKeyExists(t, f.app.Redis, "refresh_grace:"+f.jti), "a definitive rejection drops the grace entry")
	assert.False(t, laneAJTIExists(t, f.app.Redis, successor), "and revokes the successor")
}

func TestLaneA_RefreshToken_DatabaseFailureIsRetryable(t *testing.T) {
	f := newLaneARefreshFixture(t)

	broken, err := gorm.Open(postgres.Open(os.Getenv("TEST_DATABASE_URL")), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	sqlDB, err := broken.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())

	healthyDB := f.app.DB
	f.app.DB = broken
	outage := laneARefresh(t, f.app, f.token)
	assert.Equal(t, fasthttp.StatusServiceUnavailable, testutil.GetResponseStatusCode(outage))
	assert.Empty(t, testutil.GetResponseCookie(outage, "whm_refresh"))
	assert.True(t, laneAJTIExists(t, f.app.Redis, f.jti), "a transient failure must not consume the refresh token")

	// Once the database is back, the same token still works.
	f.app.DB = healthyDB
	retry := laneARefresh(t, f.app, f.token)
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(retry))
}

func TestLaneA_RefreshToken_SessionStoreFailureIsRetryable(t *testing.T) {
	f := newLaneARefreshFixture(t)

	unreachable := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 200 * time.Millisecond,
		MaxRetries:  -1,
	})
	t.Cleanup(func() { _ = unreachable.Close() })
	f.app.Redis = unreachable

	req := laneARefresh(t, f.app, f.token)
	assert.Equal(t, fasthttp.StatusServiceUnavailable, testutil.GetResponseStatusCode(req))
	assert.Empty(t, testutil.GetResponseCookie(req, "whm_refresh"))
}

// laneARestrictedRedis returns a client for a Redis ACL user that may not run
// the given commands, to simulate a failing write.
func laneARestrictedRedis(t *testing.T, admin *redis.Client, denied ...string) *redis.Client {
	t.Helper()
	ctx := context.Background()
	name := "lane-a-" + uuid.NewString()[:8]
	rules := []any{"ACL", "SETUSER", name, "on", ">lane-a-pass", "~*", "+@all"}
	for _, command := range denied {
		rules = append(rules, "-"+command)
	}
	require.NoError(t, admin.Do(ctx, rules...).Err())
	t.Cleanup(func() { admin.Do(context.Background(), "ACL", "DELUSER", name) })
	opts := *admin.Options()
	opts.Username = name
	opts.Password = "lane-a-pass"
	client := redis.NewClient(&opts)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestLaneA_RefreshToken_UnregisteredSuccessorIsNeverIssued(t *testing.T) {
	f := newLaneARefreshFixture(t)
	admin := f.app.Redis
	f.app.Redis = laneARestrictedRedis(t, admin, "set")

	req := laneARefresh(t, f.app, f.token)
	assert.Equal(t, fasthttp.StatusServiceUnavailable, testutil.GetResponseStatusCode(req))
	assert.Empty(t, testutil.GetResponseCookie(req, "whm_refresh"))
	assert.True(t, laneAJTIExists(t, admin, f.jti), "the old token stays valid when the successor cannot be registered")
}

func TestLaneA_RefreshToken_RotationFailureKeepsOldTokenAndDropsSuccessor(t *testing.T) {
	f := newLaneARefreshFixture(t)
	admin := f.app.Redis
	f.app.Redis = laneARestrictedRedis(t, admin, "eval")

	req := laneARefresh(t, f.app, f.token)
	assert.Equal(t, fasthttp.StatusServiceUnavailable, testutil.GetResponseStatusCode(req))
	assert.True(t, laneAJTIExists(t, admin, f.jti))
	assert.Equal(t, 1, laneALiveRefreshKeysFor(t, admin, f.user.ID), "the unissued successor is dropped")
}

func TestLaneA_RefreshToken_RevokeRequiresCSRF(t *testing.T) {
	f := newLaneARefreshFixture(t)

	for _, tc := range []struct{ name, cookie, header string }{
		{"no token", "", ""},
		{"header only", "", "csrf-1"},
		{"cookie only", "csrf-1", ""},
		{"mismatch", "csrf-1", "csrf-2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := laneARevoke(t, f.app, f.token, tc.cookie, tc.header)
			testutil.AssertErrorResponse(t, req, fasthttp.StatusForbidden, "CSRF")
			assert.True(t, laneAJTIExists(t, f.app.Redis, f.jti))
		})
	}
}

func TestLaneA_RefreshToken_RevokeEndsTheWholeChain(t *testing.T) {
	f := newLaneARefreshFixture(t)

	first := laneARefresh(t, f.app, f.token)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(first))
	current := testutil.GetResponseCookie(first, "whm_refresh")

	revoke := laneARevoke(t, f.app, current, "csrf-1", "csrf-1")
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(revoke))
	assert.False(t, laneAJTIExists(t, f.app.Redis, laneAJTIOf(t, current)))

	// The cookies are expired in the response.
	cleared := 0
	revoke.RequestCtx.Response.Header.VisitAllCookie(func(_, value []byte) {
		if strings.Contains(string(value), "max-age=0") || strings.Contains(strings.ToLower(string(value)), "expires=") {
			cleared++
		}
	})
	assert.GreaterOrEqual(t, cleared, 3)

	testutil.AssertErrorResponse(t, laneARefresh(t, f.app, current), fasthttp.StatusUnauthorized, "revoked")
	testutil.AssertErrorResponse(t, laneARefresh(t, f.app, f.token), fasthttp.StatusUnauthorized, "revoked")
}

func TestLaneA_RefreshToken_RevokeOfARotatedTokenRevokesItsSuccessor(t *testing.T) {
	f := newLaneARefreshFixture(t)

	// The rotation response was lost: the browser still holds the old token.
	first := laneARefresh(t, f.app, f.token)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(first))
	successor := laneAJTIOf(t, testutil.GetResponseCookie(first, "whm_refresh"))

	revoke := laneARevoke(t, f.app, f.token, "csrf-1", "csrf-1")
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(revoke))
	assert.False(t, laneAJTIExists(t, f.app.Redis, successor))
	assert.False(t, laneAKeyExists(t, f.app.Redis, "refresh_grace:"+f.jti))
	testutil.AssertErrorResponse(t, laneARefresh(t, f.app, testutil.GetResponseCookie(first, "whm_refresh")), fasthttp.StatusUnauthorized, "revoked")
}

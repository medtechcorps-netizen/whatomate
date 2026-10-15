package handlers_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
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

func TestLaneA_RefreshToken_RecordsShortRotationGrace(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID)
	jti := laneASeedRefreshJTI(t, app.Redis, user.ID)
	token := generateRefreshTokenWithJTI(t, testutil.TestJWTSecret, user, jti, time.Hour)

	req := testutil.NewJSONRequest(t, map[string]string{"refresh_token": token})
	require.NoError(t, app.RefreshToken(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))

	ttl, err := app.Redis.TTL(context.Background(), "refresh_grace:"+jti).Result()
	require.NoError(t, err)
	assert.LessOrEqual(t, ttl, 30*time.Second)
	assert.Greater(t, ttl, 20*time.Second)
	assert.False(t, laneAJTIExists(t, app.Redis, jti), "the old refresh token must be consumed")
}

func TestLaneA_RefreshToken_GraceReplayRechecksTheAccount(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID)
	jti := laneASeedRefreshJTI(t, app.Redis, user.ID)
	token := generateRefreshTokenWithJTI(t, testutil.TestJWTSecret, user, jti, time.Hour)

	first := testutil.NewJSONRequest(t, map[string]string{"refresh_token": token})
	require.NoError(t, app.RefreshToken(first))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(first))

	require.NoError(t, app.DB.Model(user).Update("is_active", false).Error)

	replay := testutil.NewJSONRequest(t, map[string]string{"refresh_token": token})
	require.NoError(t, app.RefreshToken(replay))
	testutil.AssertErrorResponse(t, replay, fasthttp.StatusUnauthorized, "disabled")
	assert.Empty(t, testutil.GetResponseCookie(replay, "whm_refresh"))
	exists, err := app.Redis.Exists(context.Background(), "refresh_grace:"+jti).Result()
	require.NoError(t, err)
	assert.Zero(t, exists, "a definitive rejection must drop the grace entry")
}

func TestLaneA_RefreshToken_DatabaseFailureIsRetryable(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID)
	jti := laneASeedRefreshJTI(t, app.Redis, user.ID)
	token := generateRefreshTokenWithJTI(t, testutil.TestJWTSecret, user, jti, time.Hour)

	broken, err := gorm.Open(postgres.Open(os.Getenv("TEST_DATABASE_URL")), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	require.NoError(t, err)
	sqlDB, err := broken.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())

	healthyDB := app.DB
	app.DB = broken
	outage := testutil.NewJSONRequest(t, map[string]string{"refresh_token": token})
	require.NoError(t, app.RefreshToken(outage))
	assert.Equal(t, fasthttp.StatusServiceUnavailable, testutil.GetResponseStatusCode(outage))
	assert.Empty(t, testutil.GetResponseCookie(outage, "whm_refresh"))
	assert.True(t, laneAJTIExists(t, app.Redis, jti), "a transient failure must not consume the refresh token")

	// Once the database is back, the same token still works.
	app.DB = healthyDB
	retry := testutil.NewJSONRequest(t, map[string]string{"refresh_token": token})
	require.NoError(t, app.RefreshToken(retry))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(retry))
}

func TestLaneA_RefreshToken_SessionStoreFailureIsRetryable(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID)
	token := generateRefreshTokenWithJTI(t, testutil.TestJWTSecret, user, uuid.NewString(), time.Hour)

	unreachable := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 200 * time.Millisecond,
		MaxRetries:  -1,
	})
	t.Cleanup(func() { _ = unreachable.Close() })
	app.Redis = unreachable

	req := testutil.NewJSONRequest(t, map[string]string{"refresh_token": token})
	require.NoError(t, app.RefreshToken(req))
	assert.Equal(t, fasthttp.StatusServiceUnavailable, testutil.GetResponseStatusCode(req))
	assert.Empty(t, testutil.GetResponseCookie(req, "whm_refresh"))
}

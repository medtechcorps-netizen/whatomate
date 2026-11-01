package handlers_test

import (
	"context"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/middleware"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

// Match only the optional policy read, after membership and user admission.
// A healthy SELECT1 or earlier organization read must not satisfy this check.
func meIdlePolicyQuery(tx *gorm.DB) bool {
	return tx.Statement.Table == "organizations" && len(tx.Statement.Selects) == 2 &&
		tx.Statement.Selects[0] == "id" && tx.Statement.Selects[1] == "settings"
}

// Invoke the same authentication and tenant/handler chain registered for /api/me.
// These fixtures exercise real hosted PostgreSQL/Redis, not an HTTP transport.
func meIdlePolicyRoute(t *testing.T, app *handlers.App, user *models.User, refresh string) *fastglue.Request {
	t.Helper()
	claims := middleware.JWTClaims{UserID: user.ID, OrganizationID: user.OrganizationID, Email: user.Email,
		RegisteredClaims: jwt.RegisteredClaims{Subject: middleware.JWTSubjectAccess, ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testutil.TestJWTSecret))
	require.NoError(t, err)
	req := testutil.NewGETRequest(t)
	req.RequestCtx.Request.SetRequestURI("/api/me")
	req.RequestCtx.Request.Header.Set("Authorization", "Bearer "+token)
	req.RequestCtx.Request.Header.SetCookie("whm_refresh", refresh)
	require.NotNil(t, middleware.AuthWithDB(testutil.TestJWTSecret, app.DB)(req))
	require.Equal(t, user.ID, req.RequestCtx.UserValue("user_id"))
	require.NoError(t, app.Tenant((*handlers.App).GetCurrentUser)(req))
	return req
}

func TestMeIdlePolicyLookupFailurePreservesSessionAndRecovers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault error
	}{
		{"selective_postgres_failure", &pgconn.PgError{Code: "57014", Message: "authority-query-secret"}},
		{"cancelled", context.Canceled},
		{"deadline", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := newTestApp(t)
			org := testutil.CreateTestOrganization(t, app.DB)
			user := testutil.CreateTestUser(t, app.DB, org.ID)
			require.NoError(t, app.DB.Model(org).Update("settings", models.JSONB{"session_idle_timeout_minutes": 60}).Error)
			healthy := app.DB
			jti := laneASeedRefreshJTI(t, app.Redis, user.ID)
			refresh := generateRefreshTokenWithJTI(t, testutil.TestJWTSecret, user, jti, time.Hour)
			before := laneALiveRefreshKeysFor(t, app.Redis, user.ID)
			faulty, hits := authorityFaultDB(t, healthy, meIdlePolicyQuery, tc.fault)
			app.DB = faulty
			req := meIdlePolicyRoute(t, app, user, refresh)
			authorityNoSessionResponse(t, req)
			assert.Empty(t, req.RequestCtx.Response.Header.Peek("X-Session-Idle-Minutes"))
			assert.NotContains(t, string(testutil.GetResponseBody(req)), user.ID.String())
			assert.EqualValues(t, 1, hits.Load(), "only the actual policy query must fail")
			assert.True(t, laneAJTIExists(t, app.Redis, jti))
			assert.Equal(t, before, laneALiveRefreshKeysFor(t, app.Redis, user.ID))
			require.NoError(t, healthy.Exec("SELECT 1").Error)

			app.DB = healthy
			recovered := meIdlePolicyRoute(t, app, user, refresh)
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(recovered))
			assert.Equal(t, "60", string(recovered.RequestCtx.Response.Header.Peek("X-Session-Idle-Minutes")))
			assert.Empty(t, recovered.RequestCtx.Response.Header.Peek("Set-Cookie"))
			assert.True(t, laneAJTIExists(t, app.Redis, jti))
			assert.Equal(t, before, laneALiveRefreshKeysFor(t, app.Redis, user.ID))
		})
	}
}

func TestMeIdlePolicyInheritsCancellationAtThePolicyRead(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID)
	healthy := app.DB
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancellation occurs only after authentication/membership queries have
	// succeeded. The policy query must use the inherited GORM context.
	faulty, hits := authorityFaultDB(t, healthy, func(tx *gorm.DB) bool {
		if !meIdlePolicyQuery(tx) {
			return false
		}
		// Tenant adds a value binding; cancellation must still reach the actual policy query.
		require.Equal(t, parent.Done(), tx.Statement.Context.Done())
		require.NoError(t, tx.Statement.Context.Err())
		cancel()
		require.ErrorIs(t, tx.Statement.Context.Err(), context.Canceled)
		return true
	}, nil)
	app.DB = faulty.WithContext(parent)
	req := meIdlePolicyRoute(t, app, user, "unchanged-refresh")
	authorityNoSessionResponse(t, req)
	assert.Empty(t, req.RequestCtx.Response.Header.Peek("X-Session-Idle-Minutes"))
	assert.EqualValues(t, 1, hits.Load())
	require.ErrorIs(t, parent.Err(), context.Canceled)
	require.NoError(t, healthy.Exec("SELECT 1").Error)
	app.DB = healthy
	recovered := meIdlePolicyRoute(t, app, user, "unchanged-refresh")
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(recovered))
	assert.Equal(t, "240", string(recovered.RequestCtx.Response.Header.Peek("X-Session-Idle-Minutes")))
}

func TestMeIdlePolicyConfirmedOrganizationDeletionDoesNotUseDefault(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID)
	healthy := app.DB
	jti := laneASeedRefreshJTI(t, app.Redis, user.ID)
	refresh := generateRefreshTokenWithJTI(t, testutil.TestJWTSecret, user, jti, time.Hour)
	// Delete after the last membership proof, then let the real policy SELECT
	// establish absence. Do not substitute a fake record-not-found error.
	faulty, hits := authorityFaultDB(t, healthy, func(tx *gorm.DB) bool {
		if !meIdlePolicyQuery(tx) {
			return false
		}
		require.NoError(t, healthy.Model(org).UpdateColumn("deleted_at", time.Now().UTC()).Error)
		return true
	}, nil)
	app.DB = faulty
	req := meIdlePolicyRoute(t, app, user, refresh)
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))
	assert.Empty(t, req.RequestCtx.Response.Header.Peek("X-Session-Idle-Minutes"))
	assert.Empty(t, req.RequestCtx.Response.Header.Peek("Set-Cookie"))
	assert.NotContains(t, string(testutil.GetResponseBody(req)), user.ID.String())
	assert.EqualValues(t, 1, hits.Load())
	assert.True(t, laneAJTIExists(t, app.Redis, jti))
}

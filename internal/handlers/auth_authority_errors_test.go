package handlers_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shridarpatil/whatomate/internal/access"
	"github.com/shridarpatil/whatomate/internal/handlers"
	"github.com/shridarpatil/whatomate/internal/middleware"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// Open a separate GORM handle over the fixture pool: callbacks are private to
// this handle, so query faults cannot poison other tests using the shared DB.
// SQL and Redis still run against the real hosted test services.
func authorityFaultDB(t *testing.T, healthy *gorm.DB, match func(*gorm.DB) bool, fault error) (*gorm.DB, *atomic.Int32) {
	t.Helper()
	pool, err := healthy.DB()
	require.NoError(t, err)
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{Logger: healthy.Logger})
	require.NoError(t, err)
	hits := &atomic.Int32{}
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register("test:authority_read_failure", func(tx *gorm.DB) {
		if match(tx) {
			hits.Add(1)
			tx.AddError(fault)
		}
	}))
	// A healthy ping/SELECT cannot prove a particular authority query worked.
	require.NoError(t, db.Exec("SELECT 1").Error)
	return db, hits
}

func authorityResellerFixture(t *testing.T) (*handlers.App, *models.User, *models.Reseller) {
	t.Helper()
	app := newTestApp(t)
	reseller := testutil.CreateTestReseller(t, app.DB)
	org := testutil.CreateTestOrganizationForReseller(t, app.DB, reseller.ID)
	role := testutil.CreateTestRole(t, app.DB, org.ID, "authority-fixture", nil)
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithPassword("authority-password-123"), testutil.WithRoleID(&role.ID))
	member := models.ResellerMember{
		BaseModel: models.BaseModel{ID: uuid.New()}, ResellerID: reseller.ID,
		UserID: user.ID, Role: models.ResellerRoleAdmin, IsActive: true,
	}
	require.NoError(t, app.DB.Create(&member).Error)
	require.NoError(t, app.DB.Model(&models.UserOrganization{}).
		Where("user_id = ? AND organization_id = ?", user.ID, org.ID).
		Updates(map[string]any{"source": models.MembershipSourceReseller, "reseller_member_id": member.ID}).Error)
	return app, user, reseller
}

func authorityNoSessionResponse(t *testing.T, req *fastglue.Request) {
	t.Helper()
	require.Equal(t, fasthttp.StatusServiceUnavailable, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))
	assert.Empty(t, req.RequestCtx.Response.Header.Peek("Set-Cookie"), "an unavailable check neither issues nor clears cookies")
	assert.NotContains(t, string(testutil.GetResponseBody(req)), "authority-query-secret")
	assert.NotContains(t, string(testutil.GetResponseBody(req)), "is_reseller_admin")
}

func TestAuthorityLookupFailurePreservesRefreshChain(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault error
	}{
		{"postgres_statement_cancelled", &pgconn.PgError{Code: "57014", Message: "authority-query-secret"}},
		{"context_cancelled", context.Canceled},
		{"deadline", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, user, _ := authorityResellerFixture(t)
			healthy := app.DB
			jti := laneASeedRefreshJTI(t, app.Redis, user.ID)
			token := generateRefreshTokenWithJTI(t, testutil.TestJWTSecret, user, jti, time.Hour)
			first := laneARefresh(t, app, token)
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(first))
			successor := laneAJTIOf(t, testutil.GetResponseCookie(first, "whm_refresh"))
			grace, err := app.Redis.Get(context.Background(), "refresh_grace:"+jti).Result()
			require.NoError(t, err)
			var userDeadline time.Time
			app.DB, _ = authorityFaultDB(t, healthy, func(tx *gorm.DB) bool {
				if tx.Statement.Table == "users" {
					var bounded bool
					userDeadline, bounded = tx.Statement.Context.Deadline()
					require.True(t, bounded)
				}
				if tx.Statement.Table == "reseller_members" && len(tx.Statement.Joins) == 2 {
					deadline, bounded := tx.Statement.Context.Deadline()
					require.True(t, bounded, "refresh must forward its deadline past the initial user lookup")
					assert.Equal(t, userDeadline, deadline)
					return true
				}
				return false
			}, tc.fault)
			for _, credential := range []string{token, testutil.GetResponseCookie(first, "whm_refresh")} {
				outage := laneARefresh(t, app, credential)
				authorityNoSessionResponse(t, outage)
				assert.True(t, laneAJTIExists(t, app.Redis, successor))
				assert.Equal(t, grace, app.Redis.Get(context.Background(), "refresh_grace:"+jti).Val())
				assert.Equal(t, 1, laneALiveRefreshKeysFor(t, app.Redis, user.ID), "no successor minted during unavailable authority")
			}
			require.NoError(t, app.DB.Exec("SELECT 1").Error)
			app.DB = healthy
			retry := laneARefresh(t, app, token)
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(retry))
			assert.Equal(t, successor, laneAJTIOf(t, testutil.GetResponseCookie(retry, "whm_refresh")))
		})
	}
}

func TestAuthorityLookupFailureNeverFallsBack(t *testing.T) {
	app := newTestApp(t)
	home := testutil.CreateTestOrganization(t, app.DB)
	other := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, home.ID, testutil.WithPassword("authority-password-123"))
	require.NoError(t, app.DB.Create(&models.UserOrganization{
		BaseModel: models.BaseModel{ID: uuid.New()}, UserID: user.ID, OrganizationID: other.ID,
	}).Error)
	healthy := app.DB
	var seen atomic.Int32
	app.DB, _ = authorityFaultDB(t, healthy, func(tx *gorm.DB) bool {
		return tx.Statement.Table == "user_organizations" && seen.Add(1) == 1
	}, context.DeadlineExceeded)
	login := func() *fastglue.Request {
		req := testutil.NewJSONRequest(t, map[string]string{"email": user.Email, "password": "authority-password-123"})
		require.NoError(t, app.Login(req))
		return req
	}
	authorityNoSessionResponse(t, login())
	assert.EqualValues(t, 1, seen.Load(), "a failed home lookup must not query fallback memberships")
	assert.Zero(t, laneALiveRefreshKeysFor(t, app.Redis, user.ID))
	app.DB = healthy
	// Proven absence still allows the existing initial-login fallback policy.
	require.NoError(t, healthy.Where("user_id = ? AND organization_id = ?", user.ID, home.ID).Delete(&models.UserOrganization{}).Error)
	retry := login()
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(retry))
	parsed, err := jwt.ParseWithClaims(testutil.GetResponseCookie(retry, "whm_access"), &middleware.JWTClaims{}, func(*jwt.Token) (any, error) {
		return []byte(testutil.TestJWTSecret), nil
	})
	require.NoError(t, err)
	assert.Equal(t, other.ID, parsed.Claims.(*middleware.JWTClaims).OrganizationID)
}

func TestAuthorityLookupFailureNeverPublishesAuthenticationContext(t *testing.T) {
	for _, credential := range []string{"jwt", "api_key"} {
		t.Run(credential, func(t *testing.T) {
			app := newTestApp(t)
			org := testutil.CreateTestOrganization(t, app.DB)
			user := testutil.CreateTestUser(t, app.DB, org.ID)
			claims := middleware.JWTClaims{UserID: user.ID, OrganizationID: org.ID, Email: user.Email,
				RegisteredClaims: jwt.RegisteredClaims{Subject: middleware.JWTSubjectAccess, ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}}
			token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testutil.TestJWTSecret))
			require.NoError(t, err)
			key := "whm_" + strings.ReplaceAll(uuid.NewString(), "-", "")
			hash, err := bcrypt.GenerateFromPassword([]byte(key), bcrypt.MinCost)
			require.NoError(t, err)
			require.NoError(t, app.DB.Create(&models.APIKey{BaseModel: models.BaseModel{ID: uuid.New()},
				OrganizationID: org.ID, UserID: user.ID, Name: "authority", KeyPrefix: key[4:20], KeyHash: string(hash), IsActive: true}).Error)
			healthy := app.DB
			faulty, hits := authorityFaultDB(t, healthy, func(tx *gorm.DB) bool {
				return tx.Statement.Table == "reseller_members" && len(tx.Statement.Joins) == 1
			}, &pgconn.PgError{Code: "57014", Message: "authority-query-secret"})
			req := testutil.NewRequest(t)
			if credential == "jwt" {
				req.RequestCtx.Request.Header.Set("Authorization", "Bearer "+token)
			} else {
				req.RequestCtx.Request.Header.Set("X-API-Key", key)
			}
			assert.Nil(t, middleware.AuthWithDB(testutil.TestJWTSecret, faulty)(req))
			authorityNoSessionResponse(t, req)
			assert.EqualValues(t, 1, hits.Load())
			for _, contextKey := range []string{"user_id", "organization_id", "email", "role_id", "is_super_admin", "is_reseller_admin", "user", "organization"} {
				assert.Nil(t, req.RequestCtx.UserValue(contextKey), contextKey)
			}
			var stored models.APIKey
			require.NoError(t, healthy.Where("key_prefix = ?", key[4:20]).First(&stored).Error)
			assert.Nil(t, stored.LastUsedAt, "failed API-key admission must not update last-used")
			// A direct member does not depend on a reseller liveness query.
			member := &models.UserOrganization{UserID: user.ID, OrganizationID: org.ID, Source: models.MembershipSourceDirect}
			active, err := access.ResellerDerivedMembershipActiveChecked(faulty, member)
			require.NoError(t, err)
			assert.True(t, active)
			assert.EqualValues(t, 1, hits.Load())
		})
	}
}

func TestAuthorityLookupFailureHandlersDoNotReturnPartialSuccess(t *testing.T) {
	for _, endpoint := range []string{"login", "switch", "me", "organizations", "ws_token"} {
		t.Run(endpoint, func(t *testing.T) {
			app, user, _ := authorityResellerFixture(t)
			healthy := app.DB
			// Include another healthy direct membership: the switcher must not
			// return a partial success after omitting the unreadable derived one.
			other := testutil.CreateTestOrganization(t, healthy)
			require.NoError(t, healthy.Create(&models.UserOrganization{BaseModel: models.BaseModel{ID: uuid.New()}, UserID: user.ID, OrganizationID: other.ID}).Error)
			app.DB, _ = authorityFaultDB(t, healthy, func(tx *gorm.DB) bool {
				return tx.Statement.Table == "reseller_members"
			}, context.DeadlineExceeded)
			req := testutil.NewJSONRequest(t, map[string]any{"email": user.Email, "password": "authority-password-123", "organization_id": user.OrganizationID})
			testutil.SetAuthContext(req, user.OrganizationID, user.ID)
			var err error
			switch endpoint {
			case "login":
				err = app.Login(req)
			case "switch":
				switchAttachCredentials(t, req, user, "")
				err = app.SwitchOrg(req)
			case "me":
				err = app.GetCurrentUser(req)
			case "organizations":
				err = app.ListMyOrganizations(req)
			case "ws_token":
				err = app.GetWSToken(req)
			}
			require.NoError(t, err)
			authorityNoSessionResponse(t, req)
			assert.NotContains(t, string(testutil.GetResponseBody(req)), other.ID.String())
			assert.Zero(t, laneALiveRefreshKeysFor(t, app.Redis, user.ID))
		})
	}
}

func TestAuthorityConfirmedSuspensionStillRevokesRefresh(t *testing.T) {
	app, user, reseller := authorityResellerFixture(t)
	jti := laneASeedRefreshJTI(t, app.Redis, user.ID)
	token := generateRefreshTokenWithJTI(t, testutil.TestJWTSecret, user, jti, time.Hour)
	require.NoError(t, app.DB.Model(reseller).Update("status", models.ResellerStatusSuspended).Error)
	req := laneARefresh(t, app, token)
	require.Equal(t, fasthttp.StatusUnauthorized, testutil.GetResponseStatusCode(req))
	assert.False(t, laneAJTIExists(t, app.Redis, jti))
	assert.Empty(t, req.RequestCtx.Response.Header.Peek("Set-Cookie"))
}

func TestAuthorityPermissionReadFailureDoesNotIssuePartialSession(t *testing.T) {
	for _, endpoint := range []string{"login", "switch", "me"} {
		t.Run(endpoint, func(t *testing.T) {
			app, user, _ := authorityResellerFixture(t)
			faulty, hits := authorityFaultDB(t, app.DB, func(tx *gorm.DB) bool {
				return tx.Statement.Table == "permissions"
			}, context.Canceled)
			app.DB = faulty
			req := testutil.NewJSONRequest(t, map[string]any{"email": user.Email, "password": "authority-password-123", "organization_id": user.OrganizationID})
			testutil.SetAuthContext(req, user.OrganizationID, user.ID)
			var err error
			switch endpoint {
			case "login":
				err = app.Login(req)
			case "switch":
				switchAttachCredentials(t, req, user, "")
				err = app.SwitchOrg(req)
			case "me":
				err = app.GetCurrentUser(req)
			}
			require.NoError(t, err)
			authorityNoSessionResponse(t, req)
			assert.EqualValues(t, 1, hits.Load(), "the response permission query must actually fail")
			assert.Zero(t, laneALiveRefreshKeysFor(t, app.Redis, user.ID))
		})
	}
}

func TestAuthorityPrimaryLookupFailuresAreUnavailable(t *testing.T) {
	for _, endpoint := range []string{"login", "switch", "me", "ws_token", "api_key", "organizations"} {
		t.Run(endpoint, func(t *testing.T) {
			app := newTestApp(t)
			org := testutil.CreateTestOrganization(t, app.DB)
			user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithPassword("authority-password-123"))
			table := "users"
			if endpoint == "switch" {
				table = "organizations"
			}
			if endpoint == "api_key" {
				table = "api_keys"
			}
			if endpoint == "organizations" {
				table = "user_organizations"
			}
			faulty, hits := authorityFaultDB(t, app.DB, func(tx *gorm.DB) bool {
				return tx.Statement.Table == table
			}, context.DeadlineExceeded)
			app.DB = faulty
			req := testutil.NewJSONRequest(t, map[string]any{"email": user.Email, "password": "authority-password-123", "organization_id": org.ID})
			testutil.SetAuthContext(req, org.ID, user.ID)
			var err error
			switch endpoint {
			case "login":
				err = app.Login(req)
			case "switch":
				switchAttachCredentials(t, req, user, "")
				err = app.SwitchOrg(req)
			case "me":
				err = app.GetCurrentUser(req)
			case "ws_token":
				err = app.GetWSToken(req)
			case "organizations":
				err = app.ListMyOrganizations(req)
			case "api_key":
				req = testutil.NewRequest(t)
				req.RequestCtx.Request.Header.Set("X-API-Key", "whm_"+strings.Repeat("a", 32))
				assert.Nil(t, middleware.AuthWithDB(testutil.TestJWTSecret, faulty)(req))
			}
			require.NoError(t, err)
			authorityNoSessionResponse(t, req)
			assert.EqualValues(t, 1, hits.Load())
			assert.Zero(t, laneALiveRefreshKeysFor(t, app.Redis, user.ID))
		})
	}
}

func TestAuthorityResellerFailureInSSOExchangeIsUnavailable(t *testing.T) {
	app := newSSOApp(t)
	attempt := laneASSOCallback(t, app)
	reseller := testutil.CreateTestReseller(t, app.DB)
	member := models.ResellerMember{BaseModel: models.BaseModel{ID: uuid.New()}, ResellerID: reseller.ID,
		UserID: attempt.user.ID, Role: models.ResellerRoleAdmin, IsActive: true}
	require.NoError(t, app.DB.Create(&member).Error)
	require.NoError(t, app.DB.Model(&models.Organization{}).Where("id = ?", attempt.orgID).Update("reseller_id", reseller.ID).Error)
	require.NoError(t, app.DB.Model(&models.UserOrganization{}).
		Where("user_id = ? AND organization_id = ?", attempt.user.ID, attempt.orgID).
		Updates(map[string]any{"source": models.MembershipSourceReseller, "reseller_member_id": member.ID}).Error)
	faulty, hits := authorityFaultDB(t, app.DB, func(tx *gorm.DB) bool {
		return tx.Statement.Table == "reseller_members" && len(tx.Statement.Joins) == 2
	}, context.DeadlineExceeded)
	app.DB = faulty
	req := laneASSOExchange(t, app, attempt.code, map[string]string{attempt.bindName: attempt.binding})
	require.Equal(t, fasthttp.StatusServiceUnavailable, testutil.GetResponseStatusCode(req))
	assert.EqualValues(t, 1, hits.Load())
	assert.Empty(t, laneASessionCookies(req))
	assert.Zero(t, laneALiveRefreshKeysFor(t, app.Redis, attempt.user.ID))
	// Existing SSO grant/binding spending precedes the lookup and is preserved.
	laneAAssertBindingSpent(t, req, attempt, "/api/auth/refresh")
}

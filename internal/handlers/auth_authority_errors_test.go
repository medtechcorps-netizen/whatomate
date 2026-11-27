package handlers_test

import (
	"context"
	"encoding/json"
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
	"gorm.io/gorm/clause"
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

func TestTenantOverridePreservesUnavailableResellerMembership(t *testing.T) {
	app, user, reseller := authorityResellerFixture(t)
	app.Config.Database.RLSEnabled = true
	healthy := app.DB
	fallback := testutil.CreateTestOrganization(t, healthy)
	require.NoError(t, healthy.Create(&models.UserOrganization{
		BaseModel: models.BaseModel{ID: uuid.New()}, UserID: user.ID, OrganizationID: fallback.ID,
	}).Error)
	var entered []string
	route := app.Tenant(func(scoped *handlers.App, _ *fastglue.Request) error {
		var bound string
		err := scoped.DB.Raw("SELECT current_setting('app.current_organization_id')").Scan(&bound).Error
		entered = append(entered, bound)
		return err
	})
	request := func() *fastglue.Request {
		r := testutil.NewGETRequest(t)
		testutil.SetAuthContext(r, fallback.ID, user.ID)
		r.RequestCtx.Request.Header.Set("X-Organization-ID", user.OrganizationID.String())
		require.NoError(t, route(r))
		return r
	}

	faulty, hits := authorityFaultDB(t, healthy, func(tx *gorm.DB) bool {
		return tx.Statement.Table == "reseller_members" && len(tx.Statement.Joins) == 2
	}, context.DeadlineExceeded)
	app.DB = faulty
	authorityNoSessionResponse(t, request())
	require.EqualValues(t, 1, hits.Load())
	require.Empty(t, entered)

	app.DB = healthy
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request()))
	require.Equal(t, []string{user.OrganizationID.String()}, entered)
	require.NoError(t, healthy.Model(reseller).Update("status", models.ResellerStatusSuspended).Error)
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request()))
	require.Equal(t, []string{user.OrganizationID.String(), fallback.ID.String()}, entered)
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

func TestSelectedWorkspaceOutagePreservesAdmissionResponse(t *testing.T) {
	app := newTestApp(t)
	healthy := app.DB
	home := testutil.CreateTestOrganization(t, healthy)
	selected := testutil.CreateTestOrganization(t, healthy)
	admin := testutil.CreateTestUser(t, healthy, home.ID, testutil.WithSuperAdmin())
	request := func() *fastglue.Request {
		r := testutil.NewGETRequest(t)
		testutil.SetAuthContext(r, home.ID, admin.ID)
		testutil.SetHeader(r, "X-Organization-ID", selected.ID.String())
		require.NoError(t, app.GetEmbeddedSignupConfig(r))
		return r
	}
	for _, tc := range []struct {
		name  string
		match func(*gorm.DB) bool
	}{
		{"actor_role", func(tx *gorm.DB) bool { return tx.Statement.Table == "users" }},
		{"selected_existence", func(tx *gorm.DB) bool {
			_, count := tx.Statement.Dest.(*int64)
			return count && tx.Statement.Table == "organizations"
		}},
		{"selected_live_row", func(tx *gorm.DB) bool {
			_, org := tx.Statement.Dest.(*models.Organization)
			return org && len(tx.Statement.Selects) == 1 && tx.Statement.Selects[0] == "id"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			faulty, hits := authorityFaultDB(t, healthy, tc.match, &pgconn.PgError{Code: "57014", Message: "authority-query-secret"})
			app.DB = faulty
			authorityNoSessionResponse(t, request())
			require.EqualValues(t, 1, hits.Load())
			app.DB = healthy
			retry := request()
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(retry), string(testutil.GetResponseBody(retry)))
			require.Contains(t, string(testutil.GetResponseBody(retry)), selected.ID.String())
		})
	}
}

func TestResourcePermissionLookupOutagePrecedesHandlerWork(t *testing.T) {
	app, user, reseller := authorityResellerFixture(t)
	healthy := app.DB
	grants := []models.Permission{
		{Resource: models.ResourceAccounts, Action: models.ActionRead},
		{Resource: models.ResourceChat, Action: models.ActionWrite},
	}
	for i, grant := range grants {
		require.NoError(t, healthy.Clauses(clause.OnConflict{DoNothing: true}).Create(&grant).Error)
		require.NoError(t, healthy.Where("resource = ? AND action = ?", grant.Resource, grant.Action).First(&grants[i]).Error)
	}
	role := testutil.CreateTestRole(t, healthy, user.OrganizationID, "scope-checked", grants)
	require.NoError(t, healthy.Model(&models.UserOrganization{}).Where("user_id = ? AND organization_id = ?", user.ID, user.OrganizationID).Update("role_id", role.ID).Error)
	list := func() *fastglue.Request {
		r := testutil.NewGETRequest(t)
		testutil.SetAuthContext(r, user.OrganizationID, user.ID)
		require.NoError(t, app.ListAccounts(r))
		return r
	}
	// The missing contact ID stops the send route immediately after its permission check.
	send := func() *fastglue.Request {
		r := testutil.NewJSONRequest(t, map[string]any{})
		testutil.SetAuthContext(r, user.OrganizationID, user.ID)
		require.NoError(t, app.SendMessage(r))
		return r
	}
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(list()))
	for _, tc := range []struct {
		name string
		read func(*gorm.DB) bool
		cold bool
	}{
		{"reseller_membership_before_cached_grant", func(tx *gorm.DB) bool { return tx.Statement.Table == "reseller_members" }, false},
		{"workspace_role", func(tx *gorm.DB) bool {
			_, one := tx.Statement.Dest.(*models.UserOrganization)
			return one && tx.Statement.Table == "user_organizations"
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.cold {
				app.InvalidateUserPermissionsCache(user.ID)
			}
			faulty, hits := authorityFaultDB(t, healthy, tc.read, context.DeadlineExceeded)
			app.DB = faulty
			authorityNoSessionResponse(t, list())
			authorityNoSessionResponse(t, send())
			require.EqualValues(t, 2, hits.Load())
			app.DB = healthy
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(list()))
			require.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(send()))
		})
	}
	t.Run("forward_read_permission_after_chat_write", func(t *testing.T) {
		request := func() *fastglue.Request {
			r := testutil.NewJSONRequest(t, map[string]string{"forward_of_message_id": uuid.NewString()})
			testutil.SetAuthContext(r, user.OrganizationID, user.ID)
			testutil.SetPathParam(r, "id", uuid.NewString())
			require.NoError(t, app.SendMessage(r))
			return r
		}
		// The first reseller check authorizes chat:write. Only the separate
		// chat:read lookup inside the forward is unavailable.
		var reads atomic.Int32
		faulty, hits := authorityFaultDB(t, healthy, func(tx *gorm.DB) bool {
			return tx.Statement.Table == "reseller_members" && reads.Add(1) == 2
		}, context.DeadlineExceeded)
		app.DB = faulty
		outage := request()
		require.Equal(t, fasthttp.StatusServiceUnavailable, testutil.GetResponseStatusCode(outage))
		require.Equal(t, "forward_temporarily_unavailable", laneERefusal(t, outage).ErrorType)
		require.EqualValues(t, 1, hits.Load())
		app.DB = healthy
		require.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(request()))
		var sent int64
		require.NoError(t, healthy.Model(&models.Message{}).Where("organization_id = ?", user.OrganizationID).Count(&sent).Error)
		require.Zero(t, sent)
	})
	require.NoError(t, healthy.Model(reseller).Update("status", models.ResellerStatusSuspended).Error)
	require.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(list()))
	require.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(send()))
}

func TestContactDirectoryPermissionOutage(t *testing.T) {
	app, user, _ := authorityResellerFixture(t)
	healthy := app.DB
	role := testutil.CreateTestRoleWithKeys(t, healthy, user.OrganizationID, "directory-reader", []string{"contacts:read", "chat:read"})
	require.NoError(t, healthy.Model(&models.UserOrganization{}).
		Where("user_id = ? AND organization_id = ?", user.ID, user.OrganizationID).Update("role_id", role.ID).Error)
	contact := testutil.CreateTestContact(t, healthy, user.OrganizationID)

	list := func(includeActivity bool) *fastglue.Request {
		req := testutil.NewGETRequest(t)
		testutil.SetAuthContext(req, user.OrganizationID, user.ID)
		if includeActivity {
			testutil.SetQueryParam(req, "include_forward_activity", "true")
		}
		require.NoError(t, app.ListContacts(req))
		return req
	}
	for _, tc := range []struct {
		name            string
		includeActivity bool
		failOnRead      int32
	}{
		{"directory_scope", false, 1},
		{"requested_history_detail", true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Each lookup rechecks live reseller membership, including on a cache hit.
			// Fail only the selected read so ordinary directory queries still work.
			var reads atomic.Int32
			faulty, hits := authorityFaultDB(t, healthy, func(tx *gorm.DB) bool {
				return tx.Statement.Table == "reseller_members" && reads.Add(1) == tc.failOnRead
			}, context.DeadlineExceeded)
			app.DB = faulty
			t.Cleanup(func() { app.DB = healthy })
			outage := list(tc.includeActivity)
			require.EqualValues(t, 1, hits.Load())
			authorityNoSessionResponse(t, outage)
			require.NotContains(t, string(testutil.GetResponseBody(outage)), contact.ID.String())

			app.DB = healthy
			retry := list(tc.includeActivity)
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(retry))
			require.Contains(t, string(testutil.GetResponseBody(retry)), contact.ID.String())
		})
	}
	t.Run("workspace_selection_outage", func(t *testing.T) {
		other := testutil.CreateTestOrganization(t, healthy)
		faulty, hits := authorityFaultDB(t, healthy, func(tx *gorm.DB) bool {
			return tx.Statement.Table == "users"
		}, context.DeadlineExceeded)
		app.DB = faulty
		t.Cleanup(func() { app.DB = healthy })
		req := testutil.NewGETRequest(t)
		testutil.SetAuthContext(req, other.ID, user.ID)
		testutil.SetHeader(req, "X-Organization-ID", user.OrganizationID.String())
		require.NoError(t, app.ListContacts(req))
		require.EqualValues(t, 1, hits.Load())
		authorityNoSessionResponse(t, req)
	})
	t.Run("active_member_without_role_sees_assigned_contacts", func(t *testing.T) {
		viewer := testutil.CreateTestUser(t, healthy, user.OrganizationID)
		assigned := testutil.CreateTestContact(t, healthy, user.OrganizationID)
		require.NoError(t, healthy.Model(assigned).Update("assigned_user_id", viewer.ID).Error)
		req := testutil.NewGETRequest(t)
		testutil.SetAuthContext(req, user.OrganizationID, viewer.ID)
		testutil.SetQueryParam(req, "include_forward_activity", "true")
		require.NoError(t, app.ListContacts(req))
		require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
		require.Contains(t, string(testutil.GetResponseBody(req)), assigned.ID.String())
		require.NotContains(t, string(testutil.GetResponseBody(req)), contact.ID.String())
	})
	t.Run("phone_mask_setting_temporarily_unavailable", func(t *testing.T) {
		require.NoError(t, healthy.Model(&models.Organization{}).Where("id = ?", user.OrganizationID).
			Update("settings", models.JSONB{"mask_phone_numbers": true}).Error)
		faulty, hits := authorityFaultDB(t, healthy, func(tx *gorm.DB) bool {
			_, org := tx.Statement.Dest.(*models.Organization)
			return org && tx.Statement.Table == "organizations"
		}, context.DeadlineExceeded)
		app.DB = faulty
		t.Cleanup(func() { app.DB = healthy })
		outage := list(false)
		require.EqualValues(t, 1, hits.Load())
		require.Equal(t, fasthttp.StatusServiceUnavailable, testutil.GetResponseStatusCode(outage))
		require.NotContains(t, string(testutil.GetResponseBody(outage)), contact.PhoneNumber)
		require.True(t, app.ShouldMaskPhoneNumbers(user.OrganizationID))
		app.DB = healthy
		retry := list(false)
		require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(retry))
		require.Contains(t, string(testutil.GetResponseBody(retry)), contact.ID.String())
		require.NotContains(t, string(testutil.GetResponseBody(retry)), contact.PhoneNumber)
	})
	t.Run("membership_removal_is_a_confirmed_denial", func(t *testing.T) {
		require.NoError(t, healthy.Where("user_id = ? AND organization_id = ?", user.ID, user.OrganizationID).
			Delete(&models.UserOrganization{}).Error)
		require.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(list(false)))
	})
}

func TestChatExportActorPreservesLookupFailuresForAllRoutes(t *testing.T) {
	app, user, reseller := authorityResellerFixture(t)
	healthy := app.DB
	routes := []struct {
		name      string
		run       func(*fastglue.Request) error
		recovered int
	}{
		{"capabilities", app.GetChatExportCapabilities, fasthttp.StatusOK},
		{"preview", app.PreviewChatExport, fasthttp.StatusBadRequest},
		{"cancel", app.CancelChatExport, fasthttp.StatusBadRequest},
		{"download", app.DownloadChatExport, fasthttp.StatusBadRequest},
	}
	request := func(run func(*fastglue.Request) error) *fastglue.Request {
		// Empty selection and job ID stop authenticated requests before export work.
		r := testutil.NewJSONRequest(t, map[string]any{})
		testutil.SetAuthContext(r, user.OrganizationID, user.ID)
		testutil.SetHeader(r, "X-Organization-ID", user.OrganizationID.String())
		require.NoError(t, run(r))
		return r
	}
	for _, tc := range []struct {
		name   string
		match  func(*gorm.DB) bool
		fault  error
		status int
		code   string
	}{
		{"live_workspace", func(tx *gorm.DB) bool {
			_, org := tx.Statement.Dest.(*models.Organization)
			return org && len(tx.Statement.Selects) == 1 && tx.Statement.Selects[0] == "id"
		}, &pgconn.PgError{Code: "08006", Message: "authority-query-secret"}, 503, "chat_export_failed"},
		{"actor", func(tx *gorm.DB) bool { return tx.Statement.Table == "users" }, &pgconn.PgError{Code: "57014", Message: "authority-query-secret"}, 503, "chat_export_failed"},
		{"membership_deadline", func(tx *gorm.DB) bool { return tx.Statement.Table == "reseller_members" }, context.DeadlineExceeded, 408, "chat_export_timeout"},
		{"membership_cancelled", func(tx *gorm.DB) bool { return tx.Statement.Table == "reseller_members" }, context.Canceled, 408, "chat_export_cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			faulty, hits := authorityFaultDB(t, healthy, tc.match, tc.fault)
			app.DB = faulty
			for _, route := range routes {
				resp := request(route.run)
				require.Equal(t, tc.status, testutil.GetResponseStatusCode(resp), route.name)
				require.Equal(t, tc.code, laneERefusal(t, resp).ErrorType, route.name)
				require.Empty(t, resp.RequestCtx.Response.Header.Peek("Content-Disposition"))
				require.Empty(t, resp.RequestCtx.Response.Header.Peek("Set-Cookie"))
				require.NotContains(t, string(testutil.GetResponseBody(resp)), "authority-query-secret")
			}
			require.EqualValues(t, len(routes), hits.Load())
		})
	}
	app.DB = healthy
	for _, route := range routes {
		resp := request(route.run)
		require.Equal(t, route.recovered, testutil.GetResponseStatusCode(resp), route.name)
	}
	require.NoError(t, healthy.Model(reseller).Update("status", models.ResellerStatusSuspended).Error)
	for _, route := range routes {
		resp := request(route.run)
		require.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(resp), route.name)
		require.Equal(t, "chat_export_denied", laneERefusal(t, resp).ErrorType)
	}
}

func TestChatExportFreshPermissionOutageDoesNotReportDisabled(t *testing.T) {
	app, user, _ := authorityResellerFixture(t)
	healthy := app.DB
	export := models.Permission{Resource: models.ResourceChat, Action: models.ActionExport}
	require.NoError(t, healthy.Clauses(clause.OnConflict{DoNothing: true}).Create(&export).Error)
	request := func() *fastglue.Request {
		r := testutil.NewGETRequest(t)
		testutil.SetAuthContext(r, user.OrganizationID, user.ID)
		testutil.SetHeader(r, "X-Organization-ID", user.OrganizationID.String())
		require.NoError(t, app.GetChatExportCapabilities(r))
		return r
	}
	var reads atomic.Int32
	faulty, hits := authorityFaultDB(t, healthy, func(tx *gorm.DB) bool {
		// Actor admission succeeds. Only the fresh permission snapshot fails.
		return tx.Statement.Table == "reseller_members" && reads.Add(1) == 2
	}, &pgconn.PgError{Code: "08006", Message: "authority-query-secret"})
	app.DB = faulty
	outage := request()
	require.Equal(t, fasthttp.StatusServiceUnavailable, testutil.GetResponseStatusCode(outage))
	require.Equal(t, "chat_export_failed", laneERefusal(t, outage).ErrorType)
	require.NotContains(t, string(testutil.GetResponseBody(outage)), "authority-query-secret")
	require.NotContains(t, string(testutil.GetResponseBody(outage)), "native_inbox")
	require.EqualValues(t, 1, hits.Load())
	app.DB = healthy
	recovered := request()
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(recovered))
	var response struct {
		Data struct {
			Available *bool `json:"available"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(recovered), &response))
	require.NotNil(t, response.Data.Available)
	require.False(t, *response.Data.Available)
}

func TestOrgMemberLookupOutageIsRetryableForAssignmentsAndReports(t *testing.T) {
	app := newTestApp(t)
	db := app.DB
	reseller := testutil.CreateTestReseller(t, db)
	org := testutil.CreateTestOrganizationForReseller(t, db, reseller.ID)
	home := testutil.CreateTestOrganization(t, db)
	role := testutil.CreateAdminRole(t, db, org.ID)
	viewer := testutil.CreateTestUser(t, db, org.ID, testutil.WithRoleID(&role.ID))
	target := testutil.CreateTestUser(t, db, home.ID)
	addResellerDerivedMembership(t, app, target.ID, reseller, org.ID, nil)
	createTestTeamWithAgent(t, app, org.ID, target.ID)
	direct := testutil.CreateTestUser(t, db, org.ID)
	createTestTeamWithAgent(t, app, org.ID, direct.ID)
	account := testutil.CreateTestWhatsAppAccount(t, db, org.ID)
	contact := testutil.CreateTestContact(t, db, org.ID)
	handoffContact := testutil.CreateTestContact(t, db, org.ID)
	existing := createTestTransfer(t, app, org.ID, contact.ID, account.Name, models.TransferStatusActive, nil)
	request := func(t *testing.T, f func(*fastglue.Request) error, pathID uuid.UUID, values map[string]any) *fastglue.Request {
		t.Helper()
		var r *fastglue.Request
		if values == nil {
			r = testutil.NewGETRequest(t)
		} else {
			r = testutil.NewJSONRequest(t, values)
		}
		testutil.SetAuthContext(r, org.ID, viewer.ID)
		if pathID != uuid.Nil {
			testutil.SetPathParam(r, "id", pathID.String())
		}
		require.NoError(t, f(r))
		return r
	}
	summary := func(t *testing.T, expected bool) {
		t.Helper()
		response := request(t, app.GetAgentComparison, uuid.Nil, nil)
		require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(response))
		var report struct {
			Data struct {
				Agents []handlers.AgentPerformanceStats
			}
		}
		require.NoError(t, json.Unmarshal(testutil.GetResponseBody(response), &report))
		ids := make([]string, 0, len(report.Data.Agents))
		for _, a := range report.Data.Agents {
			ids = append(ids, a.AgentID)
		}
		require.Contains(t, ids, direct.ID.String())
		if expected {
			require.Contains(t, ids, target.ID.String())
		} else {
			require.NotContains(t, ids, target.ID.String())
		}
	}
	details := func(t *testing.T) *fastglue.Request { return request(t, app.GetAgentDetails, target.ID, nil) }
	assign := func(t *testing.T) *fastglue.Request {
		return request(t, app.AssignContact, contact.ID, map[string]any{"user_id": target.ID.String()})
	}
	update := func(t *testing.T) *fastglue.Request {
		return request(t, app.UpdateContact, contact.ID, map[string]any{"assigned_user_id": target.ID.String(), "profile_name": "Membership fixture"})
	}
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(details(t)))
	summary(t, true)
	t.Run("viewer_membership_before_personal_or_comparison_report", func(t *testing.T) {
		faulty, hits := authorityFaultDB(t, db, func(tx *gorm.DB) bool {
			return tx.Statement.Table == "user_organizations" && !tx.Statement.Unscoped
		}, context.DeadlineExceeded)
		app.DB = faulty
		t.Cleanup(func() { app.DB = db })
		authorityNoSessionResponse(t, request(t, app.GetAgentAnalytics, uuid.Nil, nil))
		authorityNoSessionResponse(t, request(t, app.GetAgentComparison, uuid.Nil, nil))
		require.EqualValues(t, 2, hits.Load())
	})
	for _, tc := range []struct {
		name  string
		match func(*gorm.DB) bool
	}{
		{"membership_rows", func(tx *gorm.DB) bool { return tx.Statement.Table == "user_organizations" && tx.Statement.Unscoped }},
		{"reseller_check", func(tx *gorm.DB) bool {
			return tx.Statement.Table == "reseller_members" && len(tx.Statement.Joins) == 2
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			faulty, hits := authorityFaultDB(t, db, tc.match, context.DeadlineExceeded)
			app.DB = faulty
			t.Cleanup(func() { app.DB = db })
			transfer := createTransferRequest(t, org.ID, viewer.ID, handoffContact.ID, account.Name, target.ID)
			require.NoError(t, app.CreateAgentTransfer(transfer))
			for _, r := range []*fastglue.Request{
				transfer, request(t, app.AssignAgentTransfer, existing.ID, map[string]any{"agent_id": target.ID.String()}),
				details(t), assign(t), update(t), request(t, app.GetAgentComparison, uuid.Nil, nil), request(t, app.GetAgentAnalytics, uuid.Nil, nil),
			} {
				authorityNoSessionResponse(t, r)
				require.NotContains(t, string(testutil.GetResponseBody(r)), target.ID.String())
				require.NotContains(t, string(testutil.GetResponseBody(r)), direct.ID.String())
			}
			require.EqualValues(t, 7, hits.Load())
			var stored models.Contact
			require.NoError(t, db.First(&stored, "id = ?", contact.ID).Error)
			require.Nil(t, stored.AssignedUserID)
			require.Equal(t, contact.ProfileName, stored.ProfileName)
			var storedTransfer models.AgentTransfer
			require.NoError(t, db.First(&storedTransfer, "id = ?", existing.ID).Error)
			require.Nil(t, storedTransfer.AgentID)
			var count int64
			require.NoError(t, db.Model(&models.AgentTransfer{}).Where("contact_id = ?", handoffContact.ID).Count(&count).Error)
			require.Zero(t, count)
		})
	}
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(assign(t)))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(details(t)))
	summary(t, true)
	suspendTestReseller(t, app, reseller.ID)
	assertNotFoundMessage(t, details(t), "Agent not found")
	require.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(assign(t)))
	require.Equal(t, fasthttp.StatusBadRequest, testutil.GetResponseStatusCode(update(t)))
	summary(t, false)
}

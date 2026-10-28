package handlers_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

func assertSessionIssueUnavailable(t *testing.T, req *fastglue.Request) {
	t.Helper()
	testutil.AssertErrorResponse(t, req, fasthttp.StatusServiceUnavailable, "temporarily unavailable")
	assert.Empty(t, laneASessionCookies(req), "failure must neither replace nor clear the browser's session")
	assert.NotContains(t, string(testutil.GetResponseBody(req)), "expires_in")
}

func TestSessionIssuanceLoginWriteFailurePreservesOldSessionAndRecovers(t *testing.T) {
	app := newTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	const password = "session-store-password"
	user := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithPassword(password))
	admin := app.Redis
	oldJTI := laneASeedRefreshJTI(t, admin, user.ID)
	app.Redis = laneARestrictedRedis(t, admin, "set")
	login := func() *fastglue.Request {
		req := testutil.NewJSONRequest(t, map[string]string{"email": user.Email, "password": password})
		req.RequestCtx.Request.Header.SetCookie("whm_access", "existing-access-cookie")
		require.NoError(t, app.Login(req))
		return req
	}
	assertSessionIssueUnavailable(t, login())
	assert.True(t, laneAJTIExists(t, admin, oldJTI))

	app.Redis = admin
	recovered := login()
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(recovered))
	jti := laneAJTIOf(t, testutil.GetResponseCookie(recovered, "whm_refresh"))
	require.NotEqual(t, oldJTI, jti)
	assert.True(t, laneAJTIExists(t, admin, jti))
	ttl, err := admin.TTL(context.Background(), "refresh:"+jti).Result()
	require.NoError(t, err)
	assert.InDelta(t, (7 * 24 * time.Hour).Seconds(), ttl.Seconds(), 10)
	assert.True(t, laneAJTIExists(t, admin, oldJTI), "issuing a separate login must not revoke another session")
}

func TestSessionIssuanceSwitchWriteFailurePreservesOldSession(t *testing.T) {
	f := newLaneARefreshFixture(t)
	target := testutil.CreateTestOrganization(t, f.app.DB)
	require.NoError(t, f.app.DB.Create(&models.UserOrganization{
		BaseModel: models.BaseModel{ID: uuid.New()}, UserID: f.user.ID, OrganizationID: target.ID,
	}).Error)
	admin := f.app.Redis
	f.app.Redis = laneARestrictedRedis(t, admin, "set")
	req := testutil.NewJSONRequest(t, map[string]any{"organization_id": target.ID.String()})
	testutil.SetAuthContext(req, f.user.OrganizationID, f.user.ID)
	require.NoError(t, f.app.SwitchOrg(req))
	assertSessionIssueUnavailable(t, req)
	assert.True(t, laneAJTIExists(t, admin, f.jti))
}

func TestSessionIssuanceRegistrationWriteFailurePreservesSavedMembership(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "new user"
		if existing {
			name = "existing user"
		}
		t.Run(name, func(t *testing.T) {
			app := newTestApp(t)
			org := testutil.CreateTestOrganization(t, app.DB)
			role := testutil.CreateTestRoleExact(t, app.DB, org.ID, "agent", true, true, nil)
			inviter := testutil.CreateTestUser(t, app.DB, org.ID, testutil.WithSuperAdmin())
			token := createTestOrganizationInvitation(t, app, org.ID, inviter.ID, &role.ID)
			email := testutil.UniqueEmail("session-store-register")
			const password = "session-store-register-password"
			var previousJTI string
			if existing {
				home := testutil.CreateTestOrganization(t, app.DB)
				user := testutil.CreateTestUser(t, app.DB, home.ID, testutil.WithEmail(email), testutil.WithPassword(password))
				previousJTI = laneASeedRefreshJTI(t, app.Redis, user.ID)
			}
			admin := app.Redis
			app.Redis = laneARestrictedRedis(t, admin, "set")
			req := testutil.NewJSONRequest(t, map[string]any{"email": email, "password": password, "full_name": "Store Failure Fixture", "invitation_token": token})
			require.NoError(t, app.Register(req))
			assertSessionIssueUnavailable(t, req)
			var user models.User
			require.NoError(t, app.DB.Where("email = ?", email).First(&user).Error)
			var membership models.UserOrganization
			require.NoError(t, app.DB.Where("user_id = ? AND organization_id = ?", user.ID, org.ID).First(&membership).Error)
			assert.Equal(t, &role.ID, membership.RoleID)
			assert.Equal(t, 0, laneALiveRefreshKeysFor(t, admin, user.ID)-btoi(existing), "no new usable refresh credential is issued")
			if previousJTI != "" {
				assert.True(t, laneAJTIExists(t, admin, previousJTI))
			}
			// The invitation was spent and the membership is saved: recovery is
			// a fresh sign-in, not a second registration or account rollback.
			app.Redis = admin
			retry := testutil.NewJSONRequest(t, map[string]string{"email": email, "password": password})
			require.NoError(t, app.Login(retry))
			assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(retry))
		})
	}
}

func btoi(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestSessionIssuanceSSORefreshWriteFailureDoesNotGrantSession(t *testing.T) {
	app := newSSOApp(t)
	attempt := laneASSOCallback(t, app)
	admin := app.Redis
	oldJTI := laneASeedRefreshJTI(t, admin, attempt.user.ID)
	app.Redis = laneARestrictedRedis(t, admin, "set")
	req := laneASSOExchange(t, app, attempt.code, map[string]string{attempt.bindName: attempt.binding})
	assertSessionIssueUnavailable(t, req)
	laneAAssertBindingSpent(t, req, attempt, "/api/auth/refresh")
	assert.True(t, laneAJTIExists(t, admin, oldJTI))
	assert.Equal(t, 1, laneALiveRefreshKeysFor(t, admin, attempt.user.ID))
	app.Redis = admin
	replay := laneASSOExchange(t, app, attempt.code, map[string]string{attempt.bindName: attempt.binding})
	assert.Equal(t, fasthttp.StatusUnauthorized, testutil.GetResponseStatusCode(replay), "spent SSO grant cannot be reused after a persistence fault")
}

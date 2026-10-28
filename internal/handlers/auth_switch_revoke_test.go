package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
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
)

func switchAttachCredentials(t *testing.T, req *fastglue.Request, user *models.User, refresh string) {
	t.Helper()
	claims := middleware.JWTClaims{UserID: user.ID, OrganizationID: user.OrganizationID,
		RegisteredClaims: jwt.RegisteredClaims{Subject: middleware.JWTSubjectAccess,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}}
	access, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testutil.TestJWTSecret))
	require.NoError(t, err)
	if refresh == "" {
		refresh = generateRefreshTokenWithJTI(t, testutil.TestJWTSecret, user, uuid.NewString(), time.Hour)
	}
	testutil.SetAuthContext(req, user.OrganizationID, user.ID)
	req.RequestCtx.Request.Header.SetCookie("whm_access", access)
	req.RequestCtx.Request.Header.SetCookie("whm_refresh", refresh)
	req.RequestCtx.Request.Header.SetCookie("whm_csrf", "switch-fixture-csrf")
	req.RequestCtx.Request.Header.Set("X-CSRF-Token", "switch-fixture-csrf")
}

func switchAttachRegisteredCredentials(t *testing.T, app *handlers.App, req *fastglue.Request, user *models.User) {
	t.Helper()
	jti := laneASeedRefreshJTI(t, app.Redis, user.ID)
	switchAttachCredentials(t, req, user, generateRefreshTokenWithJTI(t, testutil.TestJWTSecret, user, jti, time.Hour))
}

func switchTarget(t *testing.T, f *laneARefreshFixture) uuid.UUID {
	t.Helper()
	org := testutil.CreateTestOrganization(t, f.app.DB)
	require.NoError(t, f.app.DB.Create(&models.UserOrganization{BaseModel: models.BaseModel{ID: uuid.New()},
		UserID: f.user.ID, OrganizationID: org.ID}).Error)
	return org.ID
}

func switchRequest(t *testing.T, f *laneARefreshFixture, target uuid.UUID) *fastglue.Request {
	t.Helper()
	req := testutil.NewJSONRequest(t, map[string]any{"organization_id": target})
	switchAttachCredentials(t, req, f.user, f.token)
	return req
}

func switchErrorType(t *testing.T, req *fastglue.Request) string {
	t.Helper()
	var body struct {
		ErrorType string `json:"error_type"`
	}
	require.NoError(t, json.Unmarshal(testutil.GetResponseBody(req), &body))
	return body.ErrorType
}

func TestSwitchRefreshRetiresOnlyPresentedUnit(t *testing.T) {
	f := newLaneARefreshFixture(t)
	target := switchTarget(t, f)
	otherLogin := laneASeedRefreshJTI(t, f.app.Redis, f.user.ID)
	req := switchRequest(t, f, target)
	require.NoError(t, f.app.SwitchOrg(req))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req), string(testutil.GetResponseBody(req)))
	next := testutil.GetResponseCookie(req, "whm_refresh")
	nextJTI := laneAJTIOf(t, next)
	assert.False(t, laneAJTIExists(t, f.app.Redis, f.jti))
	assert.True(t, laneAJTIExists(t, f.app.Redis, nextJTI))
	assert.True(t, laneAJTIExists(t, f.app.Redis, otherLogin))
	assert.False(t, laneAKeyExists(t, f.app.Redis, "refresh_grace:"+f.jti))
	parsed, err := jwt.ParseWithClaims(next, &middleware.JWTClaims{}, func(*jwt.Token) (any, error) {
		return []byte(testutil.TestJWTSecret), nil
	})
	require.NoError(t, err)
	assert.Equal(t, target, parsed.Claims.(*middleware.JWTClaims).OrganizationID)
	assert.Equal(t, fasthttp.StatusUnauthorized, testutil.GetResponseStatusCode(laneARefresh(t, f.app, f.token)))
	laneARevoke(t, f.app, f.token, "csrf", "csrf")
	assert.True(t, laneAJTIExists(t, f.app.Redis, nextJTI), "stale old-chain revoke cannot follow the switch into the new unit")
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(laneARefresh(t, f.app, next)))
}

func TestSwitchRefreshRequiresRetainedDirectProof(t *testing.T) {
	for _, mode := range []string{"direct", "presented_successor", "expired_grace", "rotated_again", "wrong_owner", "wrong_backlink"} {
		t.Run(mode, func(t *testing.T) {
			f := newLaneARefreshFixture(t)
			target := switchTarget(t, f)
			rotated := laneARefresh(t, f.app, f.token)
			require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(rotated))
			liveToken := testutil.GetResponseCookie(rotated, "whm_refresh")
			live := laneAJTIOf(t, liveToken)
			switch mode {
			case "presented_successor":
				f.token = liveToken
			case "expired_grace":
				require.NoError(t, f.app.Redis.Del(context.Background(), "refresh_grace:"+f.jti).Err())
			case "rotated_again":
				next := laneARefresh(t, f.app, liveToken)
				require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(next))
				live = laneAJTIOf(t, testutil.GetResponseCookie(next, "whm_refresh"))
			case "wrong_owner":
				require.NoError(t, f.app.Redis.Set(context.Background(), "refresh:"+live, uuid.NewString(), time.Hour).Err())
			case "wrong_backlink":
				require.NoError(t, f.app.Redis.Set(context.Background(), "refresh_grace_from:"+live, uuid.NewString(), time.Hour).Err())
			}
			req := switchRequest(t, f, target)
			require.NoError(t, f.app.SwitchOrg(req))
			if mode == "direct" || mode == "presented_successor" {
				require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
				assert.False(t, laneAJTIExists(t, f.app.Redis, live))
				assert.False(t, laneAKeyExists(t, f.app.Redis, "refresh_grace:"+f.jti))
				assert.False(t, laneAKeyExists(t, f.app.Redis, "refresh_grace_from:"+live))
			} else {
				require.Equal(t, fasthttp.StatusConflict, testutil.GetResponseStatusCode(req))
				assert.Equal(t, "switch_session_recovery_required", switchErrorType(t, req))
				assert.Empty(t, req.RequestCtx.Response.Header.Peek("Set-Cookie"))
				assert.True(t, laneAJTIExists(t, f.app.Redis, live), "unproved history must not consume a descendant or foreign unit")
			}
		})
	}
}

func TestSwitchRefreshCredentialFencesAndLegacyRoute(t *testing.T) {
	for _, mode := range []string{"csrf", "api_key", "missing_refresh", "wrong_user", "wrong_organization", "wrong_kind", "access_kind", "expired", "nil_jti", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			f := newLaneARefreshFixture(t)
			req := switchRequest(t, f, switchTarget(t, f))
			switch mode {
			case "csrf":
				req.RequestCtx.Request.Header.Del("X-CSRF-Token")
			case "api_key":
				req.RequestCtx.Request.Header.Set("X-API-Key", "cannot-authorize-a-cookie-switch")
			case "missing_refresh":
				req.RequestCtx.Request.Header.DelCookie("whm_refresh")
			case "wrong_user":
				other := testutil.CreateTestUser(t, f.app.DB, f.user.OrganizationID)
				req.RequestCtx.Request.Header.SetCookie("whm_refresh", generateRefreshTokenWithJTI(t, testutil.TestJWTSecret, other, f.jti, time.Hour))
			case "wrong_organization":
				otherScope := *f.user
				otherScope.OrganizationID = uuid.New()
				req.RequestCtx.Request.Header.SetCookie("whm_refresh", generateRefreshTokenWithJTI(t, testutil.TestJWTSecret, &otherScope, f.jti, time.Hour))
			case "wrong_kind":
				req.RequestCtx.Request.Header.SetCookie("whm_refresh", string(req.RequestCtx.Request.Header.Cookie("whm_access")))
			case "access_kind":
				req.RequestCtx.Request.Header.SetCookie("whm_access", f.token)
			case "expired":
				req.RequestCtx.Request.Header.SetCookie("whm_refresh", generateRefreshTokenWithJTI(t, testutil.TestJWTSecret, f.user, f.jti, -time.Hour))
			case "nil_jti":
				req.RequestCtx.Request.Header.SetCookie("whm_refresh", generateRefreshTokenWithJTI(t, testutil.TestJWTSecret, f.user, uuid.Nil.String(), time.Hour))
			}
			if mode == "legacy" {
				require.NoError(t, f.app.SwitchOrgLegacy(req))
				assert.Equal(t, "switch_client_update_required", switchErrorType(t, req))
			} else {
				require.NoError(t, f.app.SwitchOrg(req))
			}
			assert.GreaterOrEqual(t, testutil.GetResponseStatusCode(req), 400)
			assert.Empty(t, req.RequestCtx.Response.Header.Peek("Set-Cookie"))
			assert.True(t, laneAJTIExists(t, f.app.Redis, f.jti))
			assert.Equal(t, 1, laneALiveRefreshKeysFor(t, f.app.Redis, f.user.ID))
		})
	}
}

func TestSwitchRefreshUnknownCommitNeverIssuesCookies(t *testing.T) {
	for _, mode := range []string{"lose", "retry"} {
		t.Run(mode, func(t *testing.T) {
			f := newLaneARefreshFixture(t)
			target := switchTarget(t, f)
			admin := f.app.Redis
			f.app.Redis = laneAHookedRedis(t, admin, &switchReplyFault{mode: mode})
			req := switchRequest(t, f, target)
			require.NoError(t, f.app.SwitchOrg(req))
			assert.Empty(t, req.RequestCtx.Response.Header.Peek("Set-Cookie"))
			assert.False(t, laneAJTIExists(t, admin, f.jti), "the lost first reply may have followed a committed transition")
			assert.Equal(t, 1, laneALiveRefreshKeysFor(t, admin, f.user.ID), "there must be no second issuance or restored old key")
			if mode == "lose" {
				assert.Equal(t, fasthttp.StatusServiceUnavailable, testutil.GetResponseStatusCode(req))
				assert.Equal(t, "switch_outcome_unknown", switchErrorType(t, req))
			} else {
				assert.Equal(t, fasthttp.StatusConflict, testutil.GetResponseStatusCode(req))
				assert.Equal(t, "switch_session_recovery_required", switchErrorType(t, req))
			}
		})
	}
}

func TestSwitchRefreshPartialScriptFailureDoesNotClaimPreservation(t *testing.T) {
	f := newLaneARefreshFixture(t)
	target := switchTarget(t, f)
	admin := f.app.Redis
	// Redis scripts do not roll back an earlier SET when a later command is
	// refused. The result must stay unknown even though this fixture can see
	// the old key still live, and the unissued new key must never reach cookies.
	f.app.Redis = laneARestrictedRedis(t, admin, "del")
	req := switchRequest(t, f, target)
	require.NoError(t, f.app.SwitchOrg(req))
	assert.Equal(t, fasthttp.StatusServiceUnavailable, testutil.GetResponseStatusCode(req))
	assert.Equal(t, "switch_outcome_unknown", switchErrorType(t, req))
	assert.Empty(t, req.RequestCtx.Response.Header.Peek("Set-Cookie"))
	assert.True(t, laneAJTIExists(t, admin, f.jti))
	assert.Equal(t, 2, laneALiveRefreshKeysFor(t, admin, f.user.ID))
}

type switchReplyFault struct {
	mode string
	once sync.Once
}

func (h *switchReplyFault) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *switchReplyFault) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *switchReplyFault) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		inject := false
		if cmd.Name() == "eval" {
			h.once.Do(func() { inject = true })
		}
		if !inject {
			return next(ctx, cmd)
		}
		if err := next(ctx, cmd); err != nil {
			return err
		}
		if h.mode == "retry" {
			return next(ctx, cmd)
		}
		err := fmt.Errorf("simulated lost switch reply")
		cmd.SetErr(err)
		return err
	}
}

type switchEvalGate struct {
	once    sync.Once
	reached chan struct{}
	proceed chan struct{}
}

func (g *switchEvalGate) DialHook(next redis.DialHook) redis.DialHook { return next }
func (g *switchEvalGate) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (g *switchEvalGate) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "eval" {
			g.once.Do(func() { close(g.reached) })
			select {
			case <-g.proceed:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return next(ctx, cmd)
	}
}

func TestSwitchRefreshConcurrentPredecessorChanges(t *testing.T) {
	for _, first := range []string{"refresh", "revoke", "other_switch", "new_login"} {
		t.Run(first, func(t *testing.T) {
			f := newLaneARefreshFixture(t)
			if first == "new_login" {
				// Same user, independent login: a scoped retirement must not
				// become a user-wide revoke when both operations overlap.
				f.user = testutil.CreateTestUser(t, f.app.DB, f.user.OrganizationID, testutil.WithPassword("switch-new-login-password"))
				f.jti = laneASeedRefreshJTI(t, f.app.Redis, f.user.ID)
				f.token = generateRefreshTokenWithJTI(t, testutil.TestJWTSecret, f.user, f.jti, time.Hour)
			}
			target := switchTarget(t, f)
			admin := f.app.Redis
			other := &handlers.App{DB: f.app.DB, Redis: admin, Config: f.app.Config, Log: f.app.Log}
			gate := &switchEvalGate{reached: make(chan struct{}), proceed: make(chan struct{})}
			f.app.Redis = laneAHookedRedis(t, admin, gate)
			req := switchRequest(t, f, target)
			done := make(chan error, 1)
			go func() { done <- f.app.SwitchOrg(req) }()
			var release sync.Once
			joined := false
			defer func() {
				release.Do(func() { close(gate.proceed) })
				if !joined {
					select {
					case <-done:
					case <-time.After(6 * time.Second):
						t.Error("owned switch did not stop before fixture cleanup")
					}
				}
			}()
			select {
			case <-gate.reached:
			case err := <-done:
				joined = true
				t.Fatalf("switch ended before transition barrier: %v", err)
			case <-time.After(3 * time.Second):
				t.Fatal("switch did not reach transition barrier")
			}
			independent := ""
			switch first {
			case "refresh":
				require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(laneARefresh(t, other, f.token)))
			case "revoke":
				laneARevoke(t, other, f.token, "csrf", "csrf")
			case "other_switch":
				winner := switchRequest(t, f, target)
				require.NoError(t, other.SwitchOrg(winner))
				require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(winner))
				independent = laneAJTIOf(t, testutil.GetResponseCookie(winner, "whm_refresh"))
			case "new_login":
				login := testutil.NewJSONRequest(t, map[string]string{"email": f.user.Email, "password": "switch-new-login-password"})
				require.NoError(t, other.Login(login))
				require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(login))
				independent = laneAJTIOf(t, testutil.GetResponseCookie(login, "whm_refresh"))
			}
			release.Do(func() { close(gate.proceed) })
			select {
			case err := <-done:
				joined = true
				require.NoError(t, err)
			case <-time.After(6 * time.Second):
				t.Fatal("switch did not settle after barrier release")
			}
			if first == "refresh" || first == "new_login" {
				assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(req))
			} else {
				assert.Equal(t, fasthttp.StatusConflict, testutil.GetResponseStatusCode(req))
				assert.Empty(t, req.RequestCtx.Response.Header.Peek("Set-Cookie"))
			}
			if independent != "" {
				assert.True(t, laneAJTIExists(t, admin, independent), fmt.Sprintf("%s result must survive old work", first))
			}
		})
	}
}

func TestSwitchRefreshWinsBeforeDelayedOrdinaryRotation(t *testing.T) {
	f := newLaneARefreshFixture(t)
	target := switchTarget(t, f)
	admin := f.app.Redis
	gate := &switchEvalGate{reached: make(chan struct{}), proceed: make(chan struct{})}
	rotator := &handlers.App{DB: f.app.DB, Redis: laneAHookedRedis(t, admin, gate), Config: f.app.Config, Log: f.app.Log}
	refresh := testutil.NewJSONRequest(t, map[string]any{})
	refresh.RequestCtx.Request.Header.SetCookie("whm_refresh", f.token)
	done := make(chan error, 1)
	go func() { done <- rotator.RefreshToken(refresh) }()
	var release sync.Once
	joined := false
	defer func() {
		release.Do(func() { close(gate.proceed) })
		if !joined {
			select {
			case <-done:
			case <-time.After(6 * time.Second):
				t.Error("owned refresh did not stop before fixture cleanup")
			}
		}
	}()
	select {
	case <-gate.reached:
	case err := <-done:
		joined = true
		t.Fatalf("refresh ended before transition barrier: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("refresh did not reach transition barrier")
	}
	switchReq := switchRequest(t, f, target)
	require.NoError(t, f.app.SwitchOrg(switchReq))
	require.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(switchReq))
	next := laneAJTIOf(t, testutil.GetResponseCookie(switchReq, "whm_refresh"))
	release.Do(func() { close(gate.proceed) })
	select {
	case err := <-done:
		joined = true
		require.NoError(t, err)
	case <-time.After(6 * time.Second):
		t.Fatal("refresh did not settle after barrier release")
	}
	assert.Equal(t, fasthttp.StatusUnauthorized, testutil.GetResponseStatusCode(refresh))
	assert.Empty(t, testutil.GetResponseCookie(refresh, "whm_refresh"))
	assert.True(t, laneAJTIExists(t, admin, next))
	assert.False(t, laneAKeyExists(t, admin, "refresh_grace:"+f.jti))
}

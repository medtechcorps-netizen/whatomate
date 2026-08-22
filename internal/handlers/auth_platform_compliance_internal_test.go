package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/config"
	"github.com/shridarpatil/whatomate/internal/middleware"
	"github.com/shridarpatil/whatomate/internal/models"
	ws "github.com/shridarpatil/whatomate/internal/websocket"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func authFenceTestApp(t *testing.T) *App {
	t.Helper()
	return &App{
		DB:  testutil.SetupTestDB(t),
		Log: testutil.NopLogger(),
		Config: &config.Config{JWT: config.JWTConfig{
			Secret: testutil.TestJWTSecret, AccessExpiryMins: 15, RefreshExpiryDays: 7,
		}},
	}
}

func authFenceRefreshTestApp(t *testing.T) *App {
	t.Helper()
	app := authFenceTestApp(t)
	app.Redis = testutil.SetupTestRedis(t)
	if app.Redis == nil {
		t.Skip("TEST_REDIS_URL not set")
	}
	return app
}

func validateAuthFenceWebSocketToken(
	validate ws.AuthenticateFn,
	token string,
) (uuid.UUID, uuid.UUID, error) {
	return validate(token)
}

func signAuthFenceToken(
	t *testing.T,
	userID, organizationID uuid.UUID,
	subject, jti string,
	method jwt.SigningMethod,
) string {
	t.Helper()
	return signAuthFenceTokenWithOptions(
		t,
		userID,
		organizationID,
		subject,
		jti,
		method,
		testutil.TestJWTSecret,
		time.Hour,
	)
}

func signAuthFenceTokenWithOptions(
	t *testing.T,
	userID, organizationID uuid.UUID,
	subject, jti string,
	method jwt.SigningMethod,
	secret string,
	expiresIn time.Duration,
) string {
	t.Helper()
	claims := middleware.JWTClaims{
		UserID:         userID,
		OrganizationID: organizationID,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        jti,
			Subject:   subject,
			Issuer:    "whatomate",
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(expiresIn)),
		},
	}
	token := jwt.NewWithClaims(method, claims)
	signed, err := token.SignedString([]byte(secret))
	require.NoError(t, err)
	return signed
}

func TestSwitchOrgRejectsSuperAdminPlatformComplianceOrganization(t *testing.T) {
	app := authFenceTestApp(t)
	home := testutil.CreateTestOrganization(t, app.DB)
	_, target := createMetaInstagramPlatformComplianceOrganization(t, app.DB)
	superAdmin := testutil.CreateTestUser(t, app.DB, home.ID, testutil.WithSuperAdmin())

	request := testutil.NewJSONRequest(t, map[string]any{"organization_id": target.ID.String()})
	request.RequestCtx.SetUserValue(middleware.ContextKeyUserID, superAdmin.ID)

	require.NoError(t, app.SwitchOrg(request))
	assert.Equal(t, fasthttp.StatusForbidden, testutil.GetResponseStatusCode(request))
	assert.Empty(t, testutil.GetResponseCookie(request, cookieAccessName))
	assert.Empty(t, testutil.GetResponseCookie(request, cookieRefreshName))
}

func TestRefreshRejectsPurposeOrganizationAndConsumesCredentialWithoutRotation(t *testing.T) {
	app := authFenceRefreshTestApp(t)
	home := testutil.CreateTestOrganization(t, app.DB)
	_, target := createMetaInstagramPlatformComplianceOrganization(t, app.DB)
	superAdmin := testutil.CreateTestUser(t, app.DB, home.ID, testutil.WithSuperAdmin())

	jti := uuid.NewString()
	require.NoError(t, app.Redis.Set(context.Background(), refreshTokenKey(jti), superAdmin.ID.String(), time.Hour).Err())
	refreshToken := signAuthFenceToken(
		t, superAdmin.ID, target.ID, middleware.JWTSubjectRefresh, jti, jwt.SigningMethodHS256,
	)
	request := testutil.NewJSONRequest(t, RefreshRequest{RefreshToken: refreshToken})

	require.NoError(t, app.RefreshToken(request))
	assert.Equal(t, fasthttp.StatusUnauthorized, testutil.GetResponseStatusCode(request))
	assert.Empty(t, testutil.GetResponseCookie(request, cookieAccessName))
	assert.Empty(t, testutil.GetResponseCookie(request, cookieRefreshName))
	exists, err := app.Redis.Exists(context.Background(), refreshTokenKey(jti)).Result()
	require.NoError(t, err)
	assert.Zero(t, exists, "a rejected purpose-org refresh credential must not remain replayable")
}

func TestRefreshRejectsAccessCredentialBeforeRedisConsumption(t *testing.T) {
	app := authFenceRefreshTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID)

	tests := []struct {
		name    string
		subject string
		jti     string
		seedJTI bool
	}{
		{name: "access credential", subject: middleware.JWTSubjectAccess, jti: uuid.NewString(), seedJTI: true},
		{name: "websocket credential", subject: middleware.JWTSubjectWebSocket, jti: uuid.NewString(), seedJTI: true},
		{name: "typed refresh missing JTI", subject: middleware.JWTSubjectRefresh},
		{name: "legacy token missing JTI"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.seedJTI {
				require.NoError(t, app.Redis.Set(
					context.Background(), refreshTokenKey(test.jti), user.ID.String(), time.Hour,
				).Err())
			}
			token := signAuthFenceToken(
				t, user.ID, org.ID, test.subject, test.jti, jwt.SigningMethodHS256,
			)
			request := testutil.NewJSONRequest(t, RefreshRequest{RefreshToken: token})

			require.NoError(t, app.RefreshToken(request))
			assert.Equal(t, fasthttp.StatusUnauthorized, testutil.GetResponseStatusCode(request))
			assert.Empty(t, testutil.GetResponseCookie(request, cookieRefreshName))
			if test.seedJTI {
				exists, err := app.Redis.Exists(context.Background(), refreshTokenKey(test.jti)).Result()
				require.NoError(t, err)
				assert.Equal(t, int64(1), exists, "wrong-type credentials must be rejected before refresh rotation")
			}
		})
	}
}

func TestRefreshAcceptsLegacyJTIAndRotatesToTypedRefresh(t *testing.T) {
	app := authFenceRefreshTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID)
	legacyJTI := uuid.NewString()
	require.NoError(t, app.Redis.Set(
		context.Background(), refreshTokenKey(legacyJTI), user.ID.String(), time.Hour,
	).Err())
	legacyRefresh := signAuthFenceToken(
		t, user.ID, org.ID, "", legacyJTI, jwt.SigningMethodHS256,
	)
	request := testutil.NewJSONRequest(t, RefreshRequest{RefreshToken: legacyRefresh})

	require.NoError(t, app.RefreshToken(request))
	assert.Equal(t, fasthttp.StatusOK, testutil.GetResponseStatusCode(request))
	rotated := testutil.GetResponseCookie(request, cookieRefreshName)
	require.NotEmpty(t, rotated)
	parsed, err := jwt.ParseWithClaims(rotated, &middleware.JWTClaims{}, func(*jwt.Token) (any, error) {
		return []byte(testutil.TestJWTSecret), nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	require.NoError(t, err)
	claims, ok := parsed.Claims.(*middleware.JWTClaims)
	require.True(t, ok)
	assert.Equal(t, middleware.JWTSubjectRefresh, claims.Subject)
	assert.NotEmpty(t, claims.ID)
	assert.NotEqual(t, legacyJTI, claims.ID)
	oldExists, err := app.Redis.Exists(context.Background(), refreshTokenKey(legacyJTI)).Result()
	require.NoError(t, err)
	assert.Zero(t, oldExists)
	newExists, err := app.Redis.Exists(context.Background(), refreshTokenKey(claims.ID)).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(1), newExists)
}

func TestValidateWSTokenRejectsRawAndCrossUseJWTs(t *testing.T) {
	app := authFenceTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID)
	validate := app.validateWSTokenFn()

	tests := []struct {
		name    string
		subject string
		jti     string
		method  jwt.SigningMethod
	}{
		{name: "typed access", subject: middleware.JWTSubjectAccess, method: jwt.SigningMethodHS256},
		{name: "typed refresh", subject: middleware.JWTSubjectRefresh, jti: uuid.NewString(), method: jwt.SigningMethodHS256},
		{name: "legacy untyped", method: jwt.SigningMethodHS256},
		{name: "wrong signing algorithm", subject: middleware.JWTSubjectWebSocket, method: jwt.SigningMethodHS384},
		{name: "websocket token with JTI", subject: middleware.JWTSubjectWebSocket, jti: uuid.NewString(), method: jwt.SigningMethodHS256},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			token := signAuthFenceToken(t, user.ID, org.ID, test.subject, test.jti, test.method)
			gotUser, gotOrg, err := validateAuthFenceWebSocketToken(validate, token)
			require.Equal(t, jwt.ErrTokenInvalidClaims, err)
			assert.Equal(t, uuid.Nil, gotUser)
			assert.Equal(t, uuid.Nil, gotOrg)
		})
	}

	t.Run("parser failures are masked", func(t *testing.T) {
		invalidTokens := map[string]string{
			"malformed": "not.a.jwt",
			"expired": signAuthFenceTokenWithOptions(
				t, user.ID, org.ID, middleware.JWTSubjectWebSocket, "",
				jwt.SigningMethodHS256, testutil.TestJWTSecret, -time.Hour,
			),
			"bad signature": signAuthFenceTokenWithOptions(
				t, user.ID, org.ID, middleware.JWTSubjectWebSocket, "",
				jwt.SigningMethodHS256, "different-synthetic-secret-at-least-32-bytes", time.Hour,
			),
		}
		for name, token := range invalidTokens {
			t.Run(name, func(t *testing.T) {
				gotUser, gotOrg, err := validateAuthFenceWebSocketToken(validate, token)
				require.Equal(t, jwt.ErrTokenInvalidClaims, err)
				assert.Equal(t, uuid.Nil, gotUser)
				assert.Equal(t, uuid.Nil, gotOrg)
			})
		}
	})
}

func TestValidateWSTokenUsesCurrentDatabaseAuthorityAndPurpose(t *testing.T) {
	app := authFenceTestApp(t)
	home := testutil.CreateTestOrganization(t, app.DB)
	target := testutil.CreateTestOrganization(t, app.DB)
	superAdmin := testutil.CreateTestUser(t, app.DB, home.ID, testutil.WithSuperAdmin())
	token := signAuthFenceToken(
		t, superAdmin.ID, target.ID, middleware.JWTSubjectWebSocket, "", jwt.SigningMethodHS256,
	)
	validate := app.validateWSTokenFn()

	gotUser, gotOrg, err := validateAuthFenceWebSocketToken(validate, token)
	require.NoError(t, err)
	assert.Equal(t, superAdmin.ID, gotUser)
	assert.Equal(t, target.ID, gotOrg)

	_, purpose := createMetaInstagramPlatformComplianceOrganization(t, app.DB)
	purposeToken := signAuthFenceToken(
		t, superAdmin.ID, purpose.ID, middleware.JWTSubjectWebSocket, "", jwt.SigningMethodHS256,
	)
	gotUser, gotOrg, err = validateAuthFenceWebSocketToken(validate, purposeToken)
	require.Error(t, err)
	assert.Equal(t, uuid.Nil, gotUser)
	assert.Equal(t, uuid.Nil, gotOrg)
}

func TestValidateWSTokenUsesCurrentMembership(t *testing.T) {
	app := authFenceTestApp(t)
	org := testutil.CreateTestOrganization(t, app.DB)
	user := testutil.CreateTestUser(t, app.DB, org.ID)
	token := signAuthFenceToken(
		t, user.ID, org.ID, middleware.JWTSubjectWebSocket, "", jwt.SigningMethodHS256,
	)
	validate := app.validateWSTokenFn()

	gotUser, gotOrg, err := validateAuthFenceWebSocketToken(validate, token)
	require.NoError(t, err)
	assert.Equal(t, user.ID, gotUser)
	assert.Equal(t, org.ID, gotOrg)

	require.NoError(t, app.DB.Where(
		"user_id = ? AND organization_id = ?", user.ID, org.ID,
	).Delete(&models.UserOrganization{}).Error)
	gotUser, gotOrg, err = validateAuthFenceWebSocketToken(validate, token)
	require.Error(t, err)
	assert.Equal(t, uuid.Nil, gotUser)
	assert.Equal(t, uuid.Nil, gotOrg)
}

func TestGetWSTokenRejectsStalePurposeOrganizationContext(t *testing.T) {
	app := authFenceTestApp(t)
	home := testutil.CreateTestOrganization(t, app.DB)
	_, target := createMetaInstagramPlatformComplianceOrganization(t, app.DB)
	superAdmin := testutil.CreateTestUser(t, app.DB, home.ID, testutil.WithSuperAdmin())
	request := testutil.NewGETRequest(t)
	testutil.SetAuthContext(request, target.ID, superAdmin.ID)

	require.NoError(t, app.GetWSToken(request))
	assert.Equal(t, fasthttp.StatusUnauthorized, testutil.GetResponseStatusCode(request))
	assert.Empty(t, testutil.GetResponseCookie(request, cookieAccessName))
}

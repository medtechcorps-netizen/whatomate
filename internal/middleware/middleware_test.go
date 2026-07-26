package middleware_test

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/middleware"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/test/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

const testJWTSecret = "test-secret-key-must-be-at-least-32-chars"

// newTestRequest creates a fastglue request for testing.
func newTestRequest() *fastglue.Request {
	ctx := &fasthttp.RequestCtx{}
	return &fastglue.Request{RequestCtx: ctx}
}

// generateTestToken creates a valid JWT token for testing.
func generateTestToken(t *testing.T, userID, orgID uuid.UUID, email string, roleID *uuid.UUID, expiry time.Duration) string {
	t.Helper()

	claims := middleware.JWTClaims{
		UserID:         userID,
		OrganizationID: orgID,
		Email:          email,
		RoleID:         roleID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(expiry)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString([]byte(testJWTSecret))
	require.NoError(t, err)
	return tokenString
}

func TestCORS(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		origin     string
		wantOrigin string
	}{
		{
			name:       "with origin header",
			origin:     "https://example.com",
			wantOrigin: "https://example.com",
		},
		{
			name:       "without origin header",
			origin:     "",
			wantOrigin: "", // No origin sent = no CORS header set
		},
		{
			name:       "localhost origin",
			origin:     "http://localhost:3000",
			wantOrigin: "http://localhost:3000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := newTestRequest()
			if tt.origin != "" {
				req.RequestCtx.Request.Header.Set("Origin", tt.origin)
			}

			corsMiddleware := middleware.CORS(nil)
			result := corsMiddleware(req)

			require.NotNil(t, result, "CORS middleware should return request")

			// Check CORS headers
			assert.Equal(t, tt.wantOrigin, string(result.RequestCtx.Response.Header.Peek("Access-Control-Allow-Origin")))
			assert.Contains(t, string(result.RequestCtx.Response.Header.Peek("Access-Control-Allow-Methods")), "GET")
			assert.Contains(t, string(result.RequestCtx.Response.Header.Peek("Access-Control-Allow-Methods")), "POST")
			assert.Contains(t, string(result.RequestCtx.Response.Header.Peek("Access-Control-Allow-Headers")), "Authorization")
			assert.Contains(t, string(result.RequestCtx.Response.Header.Peek("Access-Control-Allow-Headers")), "X-API-Key")
			assert.Contains(t, string(result.RequestCtx.Response.Header.Peek("Access-Control-Allow-Headers")), "X-Organization-ID")
			if tt.origin != "" {
				assert.Equal(t, "true", string(result.RequestCtx.Response.Header.Peek("Access-Control-Allow-Credentials")))
			}
		})
	}
}

func TestRecovery(t *testing.T) {
	t.Parallel()

	log := testutil.NopLogger()
	recoveryMiddleware := middleware.Recovery(log)

	t.Run("normal request passes through", func(t *testing.T) {
		t.Parallel()

		req := newTestRequest()
		result := recoveryMiddleware(req)

		require.NotNil(t, result, "should return request")
	})

	// Note: Testing panic recovery is tricky because the panic happens
	// after the middleware returns. The Recovery middleware is designed
	// to wrap handlers, not to be tested in isolation.
}

func TestAuth_ValidJWT(t *testing.T) {
	t.Parallel()

	userID := uuid.New()
	orgID := uuid.New()
	email := "test@example.com"
	roleID := uuid.New()

	token := generateTestToken(t, userID, orgID, email, &roleID, time.Hour)

	req := newTestRequest()
	req.RequestCtx.Request.Header.Set("Authorization", "Bearer "+token)

	authMiddleware := middleware.Auth(testJWTSecret)
	result := authMiddleware(req)

	require.NotNil(t, result, "should return request for valid token")

	// Verify context values were set
	gotUserID, ok := result.RequestCtx.UserValue(middleware.ContextKeyUserID).(uuid.UUID)
	require.True(t, ok, "user_id should be uuid.UUID")
	assert.Equal(t, userID, gotUserID)

	gotOrgID, ok := result.RequestCtx.UserValue(middleware.ContextKeyOrganizationID).(uuid.UUID)
	require.True(t, ok, "organization_id should be uuid.UUID")
	assert.Equal(t, orgID, gotOrgID)

	gotEmail, ok := result.RequestCtx.UserValue(middleware.ContextKeyEmail).(string)
	require.True(t, ok, "email should be string")
	assert.Equal(t, email, gotEmail)

	gotRoleID, ok := result.RequestCtx.UserValue(middleware.ContextKeyRoleID).(uuid.UUID)
	require.True(t, ok, "role_id should be uuid.UUID")
	assert.Equal(t, roleID, gotRoleID)
}

func TestAuth_ExpiredJWT(t *testing.T) {
	t.Parallel()

	userID := uuid.New()
	orgID := uuid.New()
	roleID := uuid.New()

	// Create an expired token
	token := generateTestToken(t, userID, orgID, "test@example.com", &roleID, -time.Hour)

	req := newTestRequest()
	req.RequestCtx.Request.Header.Set("Authorization", "Bearer "+token)

	authMiddleware := middleware.Auth(testJWTSecret)
	result := authMiddleware(req)

	assert.Nil(t, result, "should return nil for expired token")
	assert.Equal(t, fasthttp.StatusUnauthorized, req.RequestCtx.Response.StatusCode())
}

func TestAuth_InvalidJWT(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		header string
	}{
		{
			name:   "missing authorization header",
			header: "",
		},
		{
			name:   "invalid format - no Bearer prefix",
			header: "invalid-token",
		},
		{
			name:   "invalid format - wrong prefix",
			header: "Basic some-token",
		},
		{
			name:   "malformed token",
			header: "Bearer not.a.valid.jwt",
		},
		{
			name:   "wrong secret",
			header: "Bearer " + generateTokenWithSecret(t, "wrong-secret-key-that-is-long-enough"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := newTestRequest()
			if tt.header != "" {
				req.RequestCtx.Request.Header.Set("Authorization", tt.header)
			}

			authMiddleware := middleware.Auth(testJWTSecret)
			result := authMiddleware(req)

			assert.Nil(t, result, "should return nil for invalid token")
			assert.Equal(t, fasthttp.StatusUnauthorized, req.RequestCtx.Response.StatusCode())
		})
	}
}

func TestAuth_DifferentRoleIDs(t *testing.T) {
	t.Parallel()

	roleIDs := []*uuid.UUID{
		func() *uuid.UUID { id := uuid.New(); return &id }(),
		func() *uuid.UUID { id := uuid.New(); return &id }(),
		func() *uuid.UUID { id := uuid.New(); return &id }(),
	}

	for i, roleID := range roleIDs {
		t.Run("role_"+roleID.String()[:8], func(t *testing.T) {
			t.Parallel()

			userID := uuid.New()
			orgID := uuid.New()
			token := generateTestToken(t, userID, orgID, "test@example.com", roleIDs[i], time.Hour)

			req := newTestRequest()
			req.RequestCtx.Request.Header.Set("Authorization", "Bearer "+token)

			authMiddleware := middleware.Auth(testJWTSecret)
			result := authMiddleware(req)

			require.NotNil(t, result)

			gotRoleID := result.RequestCtx.UserValue(middleware.ContextKeyRoleID).(uuid.UUID)
			assert.Equal(t, *roleIDs[i], gotRoleID)
		})
	}
}

func TestAuth_NilRoleID(t *testing.T) {
	t.Parallel()

	userID := uuid.New()
	orgID := uuid.New()
	token := generateTestToken(t, userID, orgID, "test@example.com", nil, time.Hour)

	req := newTestRequest()
	req.RequestCtx.Request.Header.Set("Authorization", "Bearer "+token)

	authMiddleware := middleware.Auth(testJWTSecret)
	result := authMiddleware(req)

	require.NotNil(t, result, "should return request for valid token with nil roleID")

	// Verify roleID is not set in context when nil
	gotRoleID := result.RequestCtx.UserValue(middleware.ContextKeyRoleID)
	assert.Nil(t, gotRoleID, "role_id should not be set when nil in claims")
}

func TestRequirePermission(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		hasPermission bool
		wantAllowed   bool
	}{
		{
			name:          "user with permission allowed",
			hasPermission: true,
			wantAllowed:   true,
		},
		{
			name:          "user without permission denied",
			hasPermission: false,
			wantAllowed:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			userID := uuid.New()
			req := newTestRequest()
			req.RequestCtx.SetUserValue(middleware.ContextKeyUserID, userID)

			// Create a mock permission checker
			checker := func(uid uuid.UUID, resource, action string) bool {
				return tt.hasPermission
			}

			permMiddleware := middleware.RequirePermission(checker, "contacts", "read")
			result := permMiddleware(req)

			if tt.wantAllowed {
				assert.NotNil(t, result, "should allow access")
			} else {
				assert.Nil(t, result, "should deny access")
				assert.Equal(t, fasthttp.StatusForbidden, req.RequestCtx.Response.StatusCode())
			}
		})
	}
}

func TestRequirePermission_NoUserInContext(t *testing.T) {
	t.Parallel()

	req := newTestRequest()
	// Don't set any user in context

	checker := func(uid uuid.UUID, resource, action string) bool {
		return true
	}

	permMiddleware := middleware.RequirePermission(checker, "contacts", "read")
	result := permMiddleware(req)

	assert.Nil(t, result, "should deny when user not in context")
	assert.Equal(t, fasthttp.StatusUnauthorized, req.RequestCtx.Response.StatusCode())
}

func TestRequireAnyPermission(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		allowedPerms map[string]bool
		checkPerms   []string
		wantAllowed  bool
	}{
		{
			name:         "user with first permission allowed",
			allowedPerms: map[string]bool{"contacts:read": true},
			checkPerms:   []string{"contacts:read", "contacts:write"},
			wantAllowed:  true,
		},
		{
			name:         "user with second permission allowed",
			allowedPerms: map[string]bool{"contacts:write": true},
			checkPerms:   []string{"contacts:read", "contacts:write"},
			wantAllowed:  true,
		},
		{
			name:         "user without any permission denied",
			allowedPerms: map[string]bool{},
			checkPerms:   []string{"contacts:read", "contacts:write"},
			wantAllowed:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			userID := uuid.New()
			req := newTestRequest()
			req.RequestCtx.SetUserValue(middleware.ContextKeyUserID, userID)

			// Create a mock permission checker
			checker := func(uid uuid.UUID, resource, action string) bool {
				perm := resource + ":" + action
				return tt.allowedPerms[perm]
			}

			permMiddleware := middleware.RequireAnyPermission(checker, tt.checkPerms...)
			result := permMiddleware(req)

			if tt.wantAllowed {
				assert.NotNil(t, result, "should allow access")
			} else {
				assert.Nil(t, result, "should deny access")
				assert.Equal(t, fasthttp.StatusForbidden, req.RequestCtx.Response.StatusCode())
			}
		})
	}
}

func TestGetUserID(t *testing.T) {
	t.Parallel()

	t.Run("user ID exists", func(t *testing.T) {
		t.Parallel()

		expectedID := uuid.New()
		req := newTestRequest()
		req.RequestCtx.SetUserValue(middleware.ContextKeyUserID, expectedID)

		userID, ok := middleware.GetUserID(req)

		assert.True(t, ok)
		assert.Equal(t, expectedID, userID)
	})

	t.Run("user ID not set", func(t *testing.T) {
		t.Parallel()

		req := newTestRequest()

		_, ok := middleware.GetUserID(req)

		assert.False(t, ok)
	})

	t.Run("wrong type in context", func(t *testing.T) {
		t.Parallel()

		req := newTestRequest()
		req.RequestCtx.SetUserValue(middleware.ContextKeyUserID, "not-a-uuid")

		_, ok := middleware.GetUserID(req)

		assert.False(t, ok)
	})
}

func TestGetOrganizationID(t *testing.T) {
	t.Parallel()

	t.Run("organization ID exists", func(t *testing.T) {
		t.Parallel()

		expectedID := uuid.New()
		req := newTestRequest()
		req.RequestCtx.SetUserValue(middleware.ContextKeyOrganizationID, expectedID)

		orgID, ok := middleware.GetOrganizationID(req)

		assert.True(t, ok)
		assert.Equal(t, expectedID, orgID)
	})

	t.Run("organization ID not set", func(t *testing.T) {
		t.Parallel()

		req := newTestRequest()

		_, ok := middleware.GetOrganizationID(req)

		assert.False(t, ok)
	})
}

func TestRequestLogger(t *testing.T) {
	t.Parallel()

	log := testutil.NopLogger()
	loggerMiddleware := middleware.RequestLogger(log)

	req := newTestRequest()
	result := loggerMiddleware(req)

	require.NotNil(t, result)

	// Check that request_start was set
	startTime, ok := result.RequestCtx.UserValue("request_start").(time.Time)
	assert.True(t, ok, "request_start should be set")
	assert.WithinDuration(t, time.Now(), startTime, time.Second)
}

func TestSecurityHeaders(t *testing.T) {
	t.Parallel()

	req := newTestRequest()
	result := middleware.SecurityHeaders()(req)

	require.NotNil(t, result)
	headers := &result.RequestCtx.Response.Header
	assert.Equal(t, "nosniff", string(headers.Peek("X-Content-Type-Options")))
	assert.Equal(t, "DENY", string(headers.Peek("X-Frame-Options")))
	assert.Equal(t, "strict-origin-when-cross-origin", string(headers.Peek("Referrer-Policy")))
	assert.Equal(t, "max-age=31536000; includeSubDomains", string(headers.Peek("Strict-Transport-Security")))
	assert.Contains(t, string(headers.Peek("Content-Security-Policy")), "default-src 'self'")
	assert.Contains(t, string(headers.Peek("Content-Security-Policy")), "frame-ancestors 'none'")
}

func TestJWTClaims(t *testing.T) {
	t.Parallel()

	userID := uuid.New()
	orgID := uuid.New()
	roleID := uuid.New()

	claims := middleware.JWTClaims{
		UserID:         userID,
		OrganizationID: orgID,
		Email:          "test@example.com",
		RoleID:         &roleID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Subject:   userID.String(),
		},
	}

	// Create and sign token
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString([]byte(testJWTSecret))
	require.NoError(t, err)

	// Parse token back
	parsedToken, err := jwt.ParseWithClaims(tokenString, &middleware.JWTClaims{}, func(token *jwt.Token) (any, error) {
		return []byte(testJWTSecret), nil
	})
	require.NoError(t, err)
	require.True(t, parsedToken.Valid)

	parsedClaims, ok := parsedToken.Claims.(*middleware.JWTClaims)
	require.True(t, ok)

	assert.Equal(t, userID, parsedClaims.UserID)
	assert.Equal(t, orgID, parsedClaims.OrganizationID)
	assert.Equal(t, "test@example.com", parsedClaims.Email)
	require.NotNil(t, parsedClaims.RoleID)
	assert.Equal(t, roleID, *parsedClaims.RoleID)
}

func TestAuth_MultipleMiddlewareChain(t *testing.T) {
	t.Parallel()

	// Test that Auth works correctly when chained with other middleware
	userID := uuid.New()
	orgID := uuid.New()
	roleID := uuid.New()
	token := generateTestToken(t, userID, orgID, "test@example.com", &roleID, time.Hour)

	req := newTestRequest()
	req.RequestCtx.Request.Header.Set("Authorization", "Bearer "+token)
	req.RequestCtx.Request.Header.Set("Origin", "https://example.com")

	// Apply CORS first
	corsMiddleware := middleware.CORS(nil)
	req = corsMiddleware(req)
	require.NotNil(t, req)

	// Then Auth
	authMiddleware := middleware.Auth(testJWTSecret)
	req = authMiddleware(req)
	require.NotNil(t, req)

	// Then RequirePermission (replaces RequireRole)
	checker := func(uid uuid.UUID, resource, action string) bool {
		return uid == userID // Allow the authenticated user
	}
	permMiddleware := middleware.RequirePermission(checker, "contacts", "read")
	req = permMiddleware(req)
	require.NotNil(t, req)

	// Verify all context values are still present
	assert.Equal(t, userID, req.RequestCtx.UserValue(middleware.ContextKeyUserID))
	assert.Equal(t, orgID, req.RequestCtx.UserValue(middleware.ContextKeyOrganizationID))
	assert.Equal(t, roleID, req.RequestCtx.UserValue(middleware.ContextKeyRoleID))

	// Verify CORS headers are still present
	assert.Equal(t, "https://example.com", string(req.RequestCtx.Response.Header.Peek("Access-Control-Allow-Origin")))
}

func TestAuthWithDB_RejectsTokenAfterOrganizationMembershipIsRemoved(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	user := testutil.CreateTestUser(t, db, org.ID)
	token := generateTestToken(t, user.ID, org.ID, user.Email, user.RoleID, time.Hour)

	require.NoError(t, db.Where("user_id = ? AND organization_id = ?", user.ID, org.ID).
		Delete(&models.UserOrganization{}).Error)

	req := newTestRequest()
	req.RequestCtx.Request.Header.Set("Authorization", "Bearer "+token)

	result := middleware.AuthWithDB(testJWTSecret, db)(req)

	assert.Nil(t, result)
	assert.Equal(t, fasthttp.StatusUnauthorized, req.RequestCtx.Response.StatusCode())
}

func TestAuthWithDB_UsesCurrentOrganizationRoleInsteadOfStaleTokenRole(t *testing.T) {
	db := testutil.SetupTestDB(t)
	org := testutil.CreateTestOrganization(t, db)
	oldRole := testutil.CreateTestRole(t, db, org.ID, "old-role", nil)
	newRole := testutil.CreateTestRole(t, db, org.ID, "new-role", nil)
	user := testutil.CreateTestUser(t, db, org.ID, testutil.WithRoleID(&oldRole.ID))
	token := generateTestToken(t, user.ID, org.ID, user.Email, &oldRole.ID, time.Hour)

	require.NoError(t, db.Model(&models.UserOrganization{}).
		Where("user_id = ? AND organization_id = ?", user.ID, org.ID).
		Update("role_id", newRole.ID).Error)

	req := newTestRequest()
	req.RequestCtx.Request.Header.Set("Authorization", "Bearer "+token)

	result := middleware.AuthWithDB(testJWTSecret, db)(req)

	require.NotNil(t, result)
	assert.Equal(t, newRole.ID, result.RequestCtx.UserValue(middleware.ContextKeyRoleID))
}

func TestAuthWithDB_RejectsSuspendedResellerDerivedMembership(t *testing.T) {
	db := testutil.SetupTestDB(t)
	reseller := testutil.CreateTestReseller(t, db)
	org := testutil.CreateTestOrganizationForReseller(t, db, reseller.ID)
	role := testutil.CreateTestRole(t, db, org.ID, "reseller-admin", nil)
	user := testutil.CreateTestUser(t, db, org.ID, testutil.WithRoleID(&role.ID))
	member := models.ResellerMember{
		BaseModel:  models.BaseModel{ID: uuid.New()},
		ResellerID: reseller.ID,
		UserID:     user.ID,
		Role:       models.ResellerRoleAdmin,
		IsActive:   true,
	}
	require.NoError(t, db.Create(&member).Error)
	require.NoError(t, db.Model(&models.UserOrganization{}).
		Where("user_id = ? AND organization_id = ?", user.ID, org.ID).
		Updates(map[string]any{
			"source":             models.MembershipSourceReseller,
			"reseller_member_id": member.ID,
		}).Error)
	token := generateTestToken(t, user.ID, org.ID, user.Email, &role.ID, time.Hour)

	activeRequest := newTestRequest()
	activeRequest.RequestCtx.Request.Header.Set("Authorization", "Bearer "+token)
	activeResult := middleware.AuthWithDB(testJWTSecret, db)(activeRequest)
	require.NotNil(t, activeResult)
	assert.Equal(t, true, activeResult.RequestCtx.UserValue(middleware.ContextKeyIsResellerAdmin))

	require.NoError(t, db.Model(reseller).Update("status", models.ResellerStatusSuspended).Error)
	suspendedRequest := newTestRequest()
	suspendedRequest.RequestCtx.Request.Header.Set("Authorization", "Bearer "+token)
	suspendedResult := middleware.AuthWithDB(testJWTSecret, db)(suspendedRequest)

	assert.Nil(t, suspendedResult)
	assert.Equal(t, fasthttp.StatusUnauthorized, suspendedRequest.RequestCtx.Response.StatusCode())
}

func TestAuthWithDB_PreservesDirectAccessWhenResellerIsSuspended(t *testing.T) {
	db := testutil.SetupTestDB(t)
	reseller := testutil.CreateTestReseller(t, db)
	org := testutil.CreateTestOrganizationForReseller(t, db, reseller.ID)
	user := testutil.CreateTestUser(t, db, org.ID)
	member := models.ResellerMember{
		BaseModel:  models.BaseModel{ID: uuid.New()},
		ResellerID: reseller.ID,
		UserID:     user.ID,
		Role:       models.ResellerRoleAdmin,
		IsActive:   true,
	}
	require.NoError(t, db.Create(&member).Error)
	require.NoError(t, db.Model(reseller).Update("status", models.ResellerStatusSuspended).Error)
	token := generateTestToken(t, user.ID, org.ID, user.Email, user.RoleID, time.Hour)

	request := newTestRequest()
	request.RequestCtx.Request.Header.Set("Authorization", "Bearer "+token)
	result := middleware.AuthWithDB(testJWTSecret, db)(request)

	require.NotNil(t, result)
	assert.Equal(t, false, result.RequestCtx.UserValue(middleware.ContextKeyIsResellerAdmin))
}

// generateTokenWithSecret creates a token signed with a specific secret.
func generateTokenWithSecret(t *testing.T, secret string) string {
	t.Helper()

	roleID := uuid.New()
	claims := middleware.JWTClaims{
		UserID:         uuid.New(),
		OrganizationID: uuid.New(),
		Email:          "test@example.com",
		RoleID:         &roleID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString([]byte(secret))
	require.NoError(t, err)
	return tokenString
}

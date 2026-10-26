package handlers

import (
	"context"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/access"
	"github.com/shridarpatil/whatomate/internal/database"
	"github.com/shridarpatil/whatomate/internal/middleware"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// LoginRequest represents login credentials
type LoginRequest struct {
	Email    string `json:"email" validate:"required,email"`
	Password string `json:"password" validate:"required,min=12"`
}

// RegisterRequest represents registration data
type RegisterRequest struct {
	Email           string `json:"email" validate:"required,email"`
	Password        string `json:"password" validate:"required,min=12"`
	FullName        string `json:"full_name" validate:"required"`
	InvitationToken string `json:"invitation_token" validate:"required"`
}

// CookieAuthResponse represents authentication response when tokens are in cookies.
// No tokens in the body — only the expiry hint and user object.
type CookieAuthResponse struct {
	ExpiresIn int         `json:"expires_in"`
	User      models.User `json:"user"`
}

// RefreshRequest represents token refresh request
type RefreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// Login authenticates a user and returns tokens
func (a *App) Login(r *fastglue.Request) error {
	var req LoginRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}

	// Find user by email with role preloaded
	var user models.User
	if err := a.DB.Preload("Role").Where("email = ?", req.Email).First(&user).Error; err != nil {
		// Run dummy bcrypt to prevent timing-based account enumeration
		_ = bcrypt.CompareHashAndPassword([]byte("$2a$10$xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"), []byte(req.Password))
		return sendOrganizationAccessError(r, err, fasthttp.StatusUnauthorized, "Invalid credentials")
	}

	// Check if user is active
	if !user.IsActive {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Account is disabled", nil, "")
	}

	// Verify password
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Password)); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Invalid credentials", nil, "")
	}

	return a.issueLoginSession(r, &user)
}

// issueLoginSession issues the session a sign-in gives an authenticated,
// active user: the membership, organization and role resolved now (the user's
// organization, or another active membership), the role's permissions, the
// session cookies and the {expires_in, user} answer. Login and the SSO code
// exchange both end here, so they issue the same thing.
func (a *App) issueLoginSession(r *fastglue.Request, user *models.User) error {
	// The membership table is authoritative. If the user's legacy home
	// organization is no longer available, login may fall back to another
	// active membership.
	if err := a.applyOrganizationMembership(user, user.OrganizationID, true); err != nil {
		return sendOrganizationAccessError(r, err, fasthttp.StatusUnauthorized, "No active organization membership")
	}

	// Load permissions for the role that belongs to the selected organization.
	if user.Role != nil && user.RoleID != nil {
		cachedPerms, err := a.GetRolePermissionsCached(*user.RoleID)
		if err != nil {
			return sendOrganizationAccessError(r, err, fasthttp.StatusUnauthorized, "Role is no longer available")
		}
		permissions := make([]models.Permission, 0, len(cachedPerms))
		for _, p := range cachedPerms {
			for i := len(p) - 1; i >= 0; i-- {
				if p[i] == ':' {
					permissions = append(permissions, models.Permission{
						Resource: p[:i],
						Action:   p[i+1:],
					})
					break
				}
			}
		}
		user.Role.Permissions = permissions
	}

	// Generate tokens
	accessToken, err := a.generateAccessToken(user)
	if err != nil {
		a.Log.Error("Failed to generate access token", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to generate token", nil, "")
	}

	refreshToken, err := a.generateRefreshToken(user)
	if err != nil {
		a.Log.Error("Failed to generate refresh token", "error", err)
		return sendRefreshTokenIssueError(r, refreshToken)
	}

	a.setAuthCookies(r, accessToken, refreshToken.token)

	return r.SendEnvelope(CookieAuthResponse{
		ExpiresIn: a.Config.JWT.AccessExpiryMins * 60,
		User:      *user,
	})
}

// Register creates a new user in an existing organization
func (a *App) Register(r *fastglue.Request) error {
	var req RegisterRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}

	invitation, err := a.consumeOrganizationInvitation(req.InvitationToken)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "A valid organization invitation is required", nil, "")
	}
	organizationID := invitation.OrganizationID

	// Validate the organization exists
	var org models.Organization
	if err := a.DB.Where("id = ?", organizationID).First(&org).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Organization not found", nil, "")
	}

	// Invitations lock the role at creation time and the role must still belong
	// to the same organization when the invitation is redeemed.
	var defaultRole models.CustomRole
	if err := a.DB.Where("id = ? AND organization_id = ?", invitation.RoleID, organizationID).First(&defaultRole).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Invitation role is no longer available", nil, "")
	}

	// Check if email already exists
	var existingUser models.User
	if err := a.DB.Where("email = ?", req.Email).First(&existingUser).Error; err == nil {
		// User exists — verify password and add to this org
		if err := bcrypt.CompareHashAndPassword([]byte(existingUser.PasswordHash), []byte(req.Password)); err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "An account with this email already exists. Please sign in and ask your organization admin to add you.", nil, "")
		}

		// Check if user account is disabled
		if !existingUser.IsActive {
			return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Account is disabled", nil, "")
		}

		// Check if already a member of this org
		var count int64
		a.DB.Model(&models.UserOrganization{}).
			Where("user_id = ? AND organization_id = ?", existingUser.ID, organizationID).
			Count(&count)
		if count > 0 {
			return r.SendErrorEnvelope(fasthttp.StatusConflict, "You are already a member of this organization", nil, "")
		}

		// Add as member with default role
		userOrg := models.UserOrganization{
			UserID:         existingUser.ID,
			OrganizationID: organizationID,
			RoleID:         &defaultRole.ID,
			IsDefault:      false,
		}
		if err := a.DB.Create(&userOrg).Error; err != nil {
			a.Log.Error("Failed to add existing user to organization", "error", err)
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to join organization", nil, "")
		}

		a.Log.Info("Existing user joined organization", "user_id", existingUser.ID, "org_id", organizationID)

		// Set org context to the new org for token generation
		existingUser.OrganizationID = organizationID
		existingUser.Role = &defaultRole
		existingUser.RoleID = &defaultRole.ID

		accessToken, err := a.generateAccessToken(&existingUser)
		if err != nil {
			a.Log.Error("Failed to generate access token", "error", err)
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to generate token", nil, "")
		}
		refreshToken, err := a.generateRefreshToken(&existingUser)
		if err != nil {
			a.Log.Error("Failed to generate refresh token", "error", err)
			return sendRefreshTokenIssueError(r, refreshToken)
		}

		a.setAuthCookies(r, accessToken, refreshToken.token)

		return r.SendEnvelope(CookieAuthResponse{
			ExpiresIn: a.Config.JWT.AccessExpiryMins * 60,
			User:      existingUser,
		})
	}

	// New user — run dummy bcrypt to prevent timing-based account enumeration
	_ = bcrypt.CompareHashAndPassword([]byte("$2a$10$xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"), []byte(req.Password))

	// Create account
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		a.Log.Error("Failed to hash password", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create account", nil, "")
	}

	tx := a.DB.Begin()
	if tx.Error != nil {
		a.Log.Error("Failed to begin transaction", "error", tx.Error)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create account", nil, "")
	}

	user := models.User{
		OrganizationID: organizationID,
		Email:          req.Email,
		PasswordHash:   string(hashedPassword),
		FullName:       req.FullName,
		RoleID:         &defaultRole.ID,
		IsActive:       true,
	}

	if err := tx.Create(&user).Error; err != nil {
		tx.Rollback()
		a.Log.Error("Failed to create user", "error", err, "email", req.Email, "org_id", organizationID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create account", nil, "")
	}

	userOrg := models.UserOrganization{
		UserID:         user.ID,
		OrganizationID: organizationID,
		RoleID:         &defaultRole.ID,
		IsDefault:      true,
	}
	if err := tx.Create(&userOrg).Error; err != nil {
		tx.Rollback()
		a.Log.Error("Failed to create user organization entry", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create account", nil, "")
	}

	if err := tx.Commit().Error; err != nil {
		a.Log.Error("Failed to commit transaction", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create account", nil, "")
	}

	a.Log.Info("Registration completed", "user_id", user.ID, "org_id", organizationID)

	user.Role = &defaultRole

	accessToken, err := a.generateAccessToken(&user)
	if err != nil {
		a.Log.Error("Failed to generate access token", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to generate token", nil, "")
	}
	refreshToken, err := a.generateRefreshToken(&user)
	if err != nil {
		a.Log.Error("Failed to generate refresh token", "error", err)
		return sendRefreshTokenIssueError(r, refreshToken)
	}

	a.setAuthCookies(r, accessToken, refreshToken.token)

	return r.SendEnvelope(CookieAuthResponse{
		ExpiresIn: a.Config.JWT.AccessExpiryMins * 60,
		User:      user,
	})
}

// RefreshToken refreshes the access token using the refresh token, with
// single-use rotation.
//
// Rotation is atomic (one Redis script consumes the old token and records the
// grace entry), so a retry or a concurrent request that presents the same
// token after it was rotated gets the pair it was exchanged for, never a 401
// and never a second token chain. The grace entry stores only the successor's
// JTI and timestamps, not a token, lasts until the successor is consumed or
// revoked (at most 10 minutes), and is honoured only while the successor is
// still live.
//
// A body of {"revoke": true} revokes the presented refresh token instead
// (the logout path: the whm_refresh cookie is only sent to this route). It
// requires the CSRF double-submit header. With "keep_cookies": true it
// revokes without touching the cookies: a closing page sends it with
// keepalive, and its answer may arrive after a newer session has set its own
// cookies, which it must not clear. The next page clears the old ones.
//
// A body of {"sso_exchange": code} exchanges the one-time code of an SSO
// callback for the session instead (see exchangeSSOSession).
func (a *App) RefreshToken(r *fastglue.Request) error {
	// Failures are logged with a reason code and the user ID only — never a
	// token value. Definitive rejections answer 401, and the client ends the
	// session. Infrastructure failures answer 503 without consuming the
	// refresh token, so the client keeps the user signed in and retries.
	reject := func(status int, message, reason string, userID any) error {
		if status == fasthttp.StatusServiceUnavailable {
			a.Log.Error("Token refresh failed", "reason", reason, "user_id", userID)
		} else {
			a.Log.Warn("Token refresh rejected", "reason", reason, "user_id", userID)
		}
		return r.SendErrorEnvelope(status, message, nil, "")
	}

	var body struct {
		RefreshToken string `json:"refresh_token"`
		Revoke       bool   `json:"revoke"`
		KeepCookies  bool   `json:"keep_cookies"`
		SSOExchange  string `json:"sso_exchange"`
	}
	_ = r.Decode(&body, "json")

	// The SSO callback page exchanges its one-time code for the session; no
	// refresh token is involved.
	if body.SSOExchange != "" {
		return a.exchangeSSOSession(r, body.SSOExchange)
	}

	// Read refresh token from cookie first, fall back to JSON body.
	refreshTokenStr := string(r.RequestCtx.Request.Header.Cookie(cookieRefreshName))
	if refreshTokenStr == "" {
		refreshTokenStr = body.RefreshToken
	}

	parseRefresh := func() (*middleware.JWTClaims, bool) {
		token, err := jwt.ParseWithClaims(
			refreshTokenStr,
			&middleware.JWTClaims{},
			func(token *jwt.Token) (any, error) {
				return []byte(a.Config.JWT.Secret), nil
			},
			jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		)
		if err != nil || !token.Valid {
			return nil, false
		}
		claims, ok := token.Claims.(*middleware.JWTClaims)
		return claims, ok
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if body.Revoke {
		// The refresh route is exempt from the CSRF middleware, so the revoke
		// variant checks the double-submit token itself: a cross-site page
		// must not be able to sign the user out.
		csrfCookie := string(r.RequestCtx.Request.Header.Cookie(cookieCSRFName))
		csrfHeader := string(r.RequestCtx.Request.Header.Peek("X-CSRF-Token"))
		if csrfCookie == "" || csrfHeader == "" || csrfCookie != csrfHeader {
			return reject(fasthttp.StatusForbidden, "CSRF token mismatch", "revoke_csrf_mismatch", nil)
		}
		if claims, ok := parseRefresh(); ok && claims.ID != "" {
			if err := a.revokeRefreshChain(ctx, claims.ID); err != nil {
				if !body.KeepCookies {
					a.clearAuthCookies(r)
				}
				return reject(fasthttp.StatusServiceUnavailable, "Session store unavailable", "revoke_store_unavailable", claims.UserID)
			}
			a.Log.Info("Refresh token revoked", "reason", "logout", "user_id", claims.UserID)
		}
		if !body.KeepCookies {
			a.clearAuthCookies(r)
		}
		return r.SendEnvelope(map[string]string{"status": "revoked"})
	}

	if refreshTokenStr == "" {
		return reject(fasthttp.StatusUnauthorized, "Missing refresh token", "missing_token", nil)
	}
	claims, ok := parseRefresh()
	if !ok {
		return reject(fasthttp.StatusUnauthorized, "Invalid refresh token", "invalid_token", nil)
	}
	if !middleware.IsRefreshJWTClaims(claims) {
		return reject(fasthttp.StatusUnauthorized, "Invalid token claims", "invalid_claims", nil)
	}

	oldKey := refreshTokenKey(claims.ID)
	graceKey := refreshGraceKey(claims.ID)

	// Neither the token nor a grace entry left: revoked or long since used.
	live, err := a.Redis.Exists(ctx, oldKey, graceKey).Result()
	if err != nil {
		return reject(fasthttp.StatusServiceUnavailable, "Session store unavailable", "session_store_unavailable", claims.UserID)
	}
	if live == 0 {
		return reject(fasthttp.StatusUnauthorized, "Refresh token has been revoked", "revoked_or_reused", claims.UserID)
	}

	// A definitive rejection revokes the credential and anything it was
	// already rotated into, so it cannot be replayed.
	consume := func() {
		revokeCtx, revokeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer revokeCancel()
		_ = a.revokeRefreshChain(revokeCtx, claims.ID)
	}

	// Get user
	var user models.User
	if err := a.DB.WithContext(ctx).Where("id = ?", claims.UserID).First(&user).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			consume()
			return reject(fasthttp.StatusUnauthorized, "User not found", "user_not_found", claims.UserID)
		}
		return reject(fasthttp.StatusServiceUnavailable, "Session check unavailable", "user_lookup_failed", claims.UserID)
	}

	if !user.IsActive {
		consume()
		return reject(fasthttp.StatusUnauthorized, "Account is disabled", "account_disabled", claims.UserID)
	}

	if err := a.applyOrganizationMembershipDB(a.DB.WithContext(ctx), &user, claims.OrganizationID, false); err != nil {
		if !organizationAccessDenied(err) {
			return reject(fasthttp.StatusServiceUnavailable, "Session check unavailable", "membership_lookup_failed", claims.UserID)
		}
		consume()
		return reject(fasthttp.StatusUnauthorized, "Organization access is no longer available", "membership_revoked", claims.UserID)
	}

	// Mint and register the successor before the old token is consumed. It
	// starts with a short lifetime that a successful rotation replaces with
	// the full one: if the rotation never runs, or its outcome is unknown, the
	// unissued successor expires on its own while the old token stays valid
	// for the client's retry. A token the server never registered is never
	// handed out.
	now := time.Now()
	successorJTI := uuid.New().String()
	successorExpiry := now.Add(time.Duration(a.Config.JWT.RefreshExpiryDays) * 24 * time.Hour)
	successorKey := refreshTokenKey(successorJTI)
	if err := a.Redis.Set(ctx, successorKey, user.ID.String(), 2*time.Minute).Err(); err != nil {
		return reject(fasthttp.StatusServiceUnavailable, "Session store unavailable", "successor_store_failed", claims.UserID)
	}
	dropSuccessor := func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer dropCancel()
		a.Redis.Del(dropCtx, successorKey, refreshGraceFromKey(successorJTI))
	}
	issueSuccessor := func() error {
		newRefreshToken, err := a.signRefreshToken(&user, successorJTI, now, successorExpiry)
		if err != nil {
			a.Log.Error("Failed to generate refresh token", "error", err)
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to generate token", nil, "")
		}
		accessToken, err := a.generateAccessToken(&user)
		if err != nil {
			a.Log.Error("Failed to generate access token", "error", err)
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to generate token", nil, "")
		}
		a.setAuthCookies(r, accessToken, newRefreshToken)
		return r.SendEnvelope(CookieAuthResponse{
			ExpiresIn: a.Config.JWT.AccessExpiryMins * 60,
			User:      user,
		})
	}

	// Atomically consume the old token, register the successor for its full
	// lifetime (again, so a winner never reports a token whose short-lived
	// key was lost meanwhile) and record the grace entry; if the old token is
	// already gone, read the grace entry instead. The script is idempotent:
	// when a lost reply makes the client library send it again, the second
	// run finds this request's own grace entry and reports the same win.
	rotation, err := a.Redis.Eval(ctx, `
if redis.call('DEL', KEYS[1]) == 1 then
  local previous = redis.call('GET', KEYS[3])
  if previous then redis.call('DEL', 'refresh_grace:' .. previous) end
  redis.call('DEL', KEYS[3])
  redis.call('SET', KEYS[2], ARGV[1], 'EX', ARGV[2])
  redis.call('SET', KEYS[4], ARGV[3], 'EX', ARGV[2])
  redis.call('SET', KEYS[5], ARGV[6], 'EX', ARGV[5])
  return 'W'
end
local grace = redis.call('GET', KEYS[2])
if grace then
  if string.sub(grace, 1, string.len(ARGV[4]) + 1) == ARGV[4] .. '|' then return 'W' end
  return 'G' .. grace
end
return 'N'`,
		[]string{oldKey, graceKey, refreshGraceFromKey(claims.ID), refreshGraceFromKey(successorJTI), successorKey},
		fmt.Sprintf("%s|%d|%d", successorJTI, now.Unix(), successorExpiry.Unix()),
		int((10 * time.Minute).Seconds()),
		claims.ID,
		successorJTI,
		int(successorExpiry.Sub(now).Seconds()),
		user.ID.String(),
	).Text()
	if err != nil {
		// The outcome is unknown: the script may have run. Keep the
		// successor (it expires on its own if the rotation never happened);
		// the client's retry is served from the grace entry or rotates anew.
		return reject(fasthttp.StatusServiceUnavailable, "Session store unavailable", "rotation_store_failed", claims.UserID)
	}

	if rotation == "W" {
		return issueSuccessor()
	}

	grace := ""
	if len(rotation) > 1 && rotation[0] == 'G' {
		grace = rotation[1:]
	}
	// A rotation in flight elsewhere may not have recorded its grace entry
	// yet: re-read briefly before concluding the token was revoked.
	for attempt := 0; grace == "" && attempt < 6; attempt++ {
		time.Sleep(250 * time.Millisecond)
		grace, err = a.Redis.Eval(ctx, `return redis.call('GET', KEYS[1]) or ''`, []string{graceKey}).Text()
		if err != nil {
			return reject(fasthttp.StatusServiceUnavailable, "Session store unavailable", "grace_store_unavailable", claims.UserID)
		}
	}
	graceJTI, graceIssuedAt, graceExpiry, ok := parseRefreshGrace(grace)
	if ok && graceJTI == successorJTI {
		// This request's own rotation, seen again.
		return issueSuccessor()
	}
	// Another request rotated this token first: this request's successor is
	// an orphan, and the client gets the pair the token was exchanged for.
	dropSuccessor()
	if !ok {
		return reject(fasthttp.StatusUnauthorized, "Refresh token has been revoked", "revoked_or_reused", claims.UserID)
	}
	// The grace entry is honoured only while its successor is still live;
	// logout or the successor's own rotation closes it.
	successorLive, err := a.Redis.Exists(ctx, refreshTokenKey(graceJTI)).Result()
	if err != nil {
		return reject(fasthttp.StatusServiceUnavailable, "Session store unavailable", "grace_store_unavailable", claims.UserID)
	}
	if successorLive == 0 {
		a.Redis.Del(ctx, graceKey)
		return reject(fasthttp.StatusUnauthorized, "Refresh token has been revoked", "revoked_or_reused", claims.UserID)
	}
	graceRefreshToken, err := a.signRefreshToken(&user, graceJTI, graceIssuedAt, graceExpiry)
	if err != nil {
		a.Log.Error("Failed to generate refresh token", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to generate token", nil, "")
	}
	accessToken, err := a.generateAccessToken(&user)
	if err != nil {
		a.Log.Error("Failed to generate access token", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to generate token", nil, "")
	}
	a.Log.Info("Token refresh replayed within rotation grace", "reason", "rotation_grace", "user_id", claims.UserID)
	a.setAuthCookies(r, accessToken, graceRefreshToken)
	return r.SendEnvelope(CookieAuthResponse{
		ExpiresIn: a.Config.JWT.AccessExpiryMins * 60,
		User:      user,
	})
}

// refreshGraceKey returns the Redis key that maps a rotated refresh token's
// JTI to its successor ("<jti>|<issued unix>|<expiry unix>").
func refreshGraceKey(jti string) string {
	return "refresh_grace:" + jti
}

// refreshGraceFromKey returns the Redis key that maps a refresh token's JTI to
// the JTI it was rotated from, so rotating it can close that grace entry.
func refreshGraceFromKey(jti string) string {
	return "refresh_grace_from:" + jti
}

// parseRefreshGrace parses a grace entry written by RefreshToken.
func parseRefreshGrace(value string) (jti string, issuedAt, expiresAt time.Time, ok bool) {
	for i := 0; i < len(value); i++ {
		if value[i] != '|' {
			continue
		}
		var issuedUnix, expiryUnix int64
		if n, err := fmt.Sscanf(value[i+1:], "%d|%d", &issuedUnix, &expiryUnix); err != nil || n != 2 {
			return "", time.Time{}, time.Time{}, false
		}
		if _, err := uuid.Parse(value[:i]); err != nil {
			return "", time.Time{}, time.Time{}, false
		}
		return value[:i], time.Unix(issuedUnix, 0), time.Unix(expiryUnix, 0), true
	}
	return "", time.Time{}, time.Time{}, false
}

// signRefreshToken signs a refresh token for jti without registering it.
// HMAC signing is deterministic, so the same claims give the same token.
func (a *App) signRefreshToken(user *models.User, jti string, issuedAt, expiresAt time.Time) (string, error) {
	claims := middleware.JWTClaims{
		UserID:         user.ID,
		OrganizationID: user.OrganizationID,
		Email:          user.Email,
		RoleID:         user.RoleID,
		IsSuperAdmin:   user.IsSuperAdmin,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        jti,
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(issuedAt),
			Issuer:    "whatomate",
			Subject:   middleware.JWTSubjectRefresh,
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(a.Config.JWT.Secret))
}

// revokeRefreshChain revokes a refresh token JTI together with the successor
// it may already have been rotated into, and closes the grace entries that
// point to or from it.
func (a *App) revokeRefreshChain(ctx context.Context, jti string) error {
	return a.Redis.Eval(ctx, `
local grace = redis.call('GET', KEYS[2])
if grace then
  local successor = string.match(grace, '^([^|]+)')
  if successor then redis.call('DEL', 'refresh:' .. successor, 'refresh_grace_from:' .. successor) end
end
local previous = redis.call('GET', KEYS[3])
if previous then redis.call('DEL', 'refresh_grace:' .. previous) end
redis.call('DEL', KEYS[1], KEYS[2], KEYS[3])
return 1`,
		[]string{refreshTokenKey(jti), refreshGraceKey(jti), refreshGraceFromKey(jti)},
	).Err()
}

// ssoOrganizationMember tells whether user may sign in through the SSO of
// organization orgID. The membership table is the authority, as it is for a
// login (applyOrganizationMembership): an active membership there (one derived
// from a reseller only while that is active). The legacy users.organization_id
// grants nothing by itself (removing a member deletes only the membership),
// except for a super admin's own organization. The callback checks it, and the
// exchange again (the grant waits up to ssoExchangeTTL).
func (a *App) ssoOrganizationMember(user *models.User, orgID uuid.UUID) (bool, error) {
	if user.IsSuperAdmin && user.OrganizationID == orgID {
		return true, nil
	}
	var memberships []models.UserOrganization
	if err := a.DB.Where("user_id = ? AND organization_id = ?", user.ID, orgID).
		Limit(1).Find(&memberships).Error; err != nil {
		return false, err
	}
	if len(memberships) == 0 {
		return false, nil
	}
	// As for a login, a super admin's membership counts whatever its source.
	if user.IsSuperAdmin {
		return true, nil
	}
	return access.ResellerDerivedMembershipActiveChecked(a.DB, &memberships[0])
}

// applyOrganizationMembership updates a user with the current organization and
// role from user_organizations. Only initial login may fall back to a different
// active membership; refresh and organization switching must honor the exact
// organization already selected.
func (a *App) applyOrganizationMembership(user *models.User, orgID uuid.UUID, allowFallback bool) error {
	return a.applyOrganizationMembershipDB(a.DB, user, orgID, allowFallback)
}

// applyOrganizationMembershipDB preserves a caller's bound database context
// through every authority lookup, including reseller and role queries.
func (a *App) applyOrganizationMembershipDB(db *gorm.DB, user *models.User, orgID uuid.UUID, allowFallback bool) error {
	var membership models.UserOrganization
	membershipErr := db.Where("user_id = ? AND organization_id = ?", user.ID, orgID).First(&membership).Error
	if membershipErr != nil && membershipErr != gorm.ErrRecordNotFound {
		return membershipErr
	}
	if !user.IsSuperAdmin && membershipErr == nil {
		active, err := access.ResellerDerivedMembershipActiveChecked(db, &membership)
		if err != nil {
			return err
		}
		if !active {
			membershipErr = gorm.ErrRecordNotFound
		}
	}
	if membershipErr != nil && allowFallback && !user.IsSuperAdmin {
		var memberships []models.UserOrganization
		if err := db.Where("user_id = ?", user.ID).
			Order("is_default DESC, created_at ASC").
			Find(&memberships).Error; err != nil {
			return err
		}
		for index := range memberships {
			active, err := access.ResellerDerivedMembershipActiveChecked(db, &memberships[index])
			if err != nil {
				return err
			}
			if active {
				membership = memberships[index]
				orgID = membership.OrganizationID
				membershipErr = nil
				break
			}
		}
	}

	if !user.IsSuperAdmin && membershipErr != nil {
		return membershipErr
	}

	compliance, err := database.IsPlatformComplianceOrganization(db, orgID)
	if err != nil {
		return err
	}
	if compliance {
		return database.ErrPlatformComplianceTenant
	}

	isResellerAdmin, err := access.IsResellerAdminChecked(db, user.ID)
	if err != nil {
		return err
	}
	roleID := user.RoleID
	if membershipErr == nil {
		roleID = membership.RoleID
	}

	var selectedRole *models.CustomRole
	if roleID != nil {
		var role models.CustomRole
		roleQuery := db.Where("id = ?", *roleID)
		if !user.IsSuperAdmin || membershipErr == nil {
			roleQuery = roleQuery.Where("organization_id = ?", orgID)
		}
		if err := roleQuery.First(&role).Error; err != nil {
			return err
		}
		selectedRole = &role
	}
	// Do not publish a partially resolved identity on lookup failure.
	user.OrganizationID = orgID
	user.IsResellerAdmin = isResellerAdmin
	user.RoleID = roleID
	user.Role = selectedRole

	return nil
}

func (a *App) generateAccessToken(user *models.User) (string, error) {
	claims := middleware.JWTClaims{
		UserID:         user.ID,
		OrganizationID: user.OrganizationID,
		Email:          user.Email,
		RoleID:         user.RoleID,
		IsSuperAdmin:   user.IsSuperAdmin,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Duration(a.Config.JWT.AccessExpiryMins) * time.Minute)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    "whatomate",
			Subject:   middleware.JWTSubjectAccess,
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(a.Config.JWT.Secret))
}

// refreshTokenIssueResult is an internal issuance outcome, never a response payload.
// The storage classification is separate from the wrapped error cause and adds
// no eagerly initialized sentinel or error-interface implementation.
type refreshTokenIssueResult struct {
	token            string
	storeUnavailable bool
}

func sendRefreshTokenIssueError(r *fastglue.Request, issue refreshTokenIssueResult) error {
	if issue.storeUnavailable {
		// Registration may already have saved the account and membership, and
		// an SSO exchange has consumed its one-time grant. Start a new sign-in;
		// do not report success or clear any session the browser already holds.
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Sign-in is temporarily unavailable, please try signing in again", nil, "")
	}
	return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to generate token", nil, "")
}

func (a *App) generateRefreshToken(user *models.User) (refreshTokenIssueResult, error) {
	if a.Redis == nil {
		return refreshTokenIssueResult{storeUnavailable: true}, fmt.Errorf("refresh token store unavailable")
	}
	jti := uuid.New().String()
	expiry := time.Duration(a.Config.JWT.RefreshExpiryDays) * 24 * time.Hour

	claims := middleware.JWTClaims{
		UserID:         user.ID,
		OrganizationID: user.OrganizationID,
		Email:          user.Email,
		RoleID:         user.RoleID,
		IsSuperAdmin:   user.IsSuperAdmin,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        jti,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(expiry)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    "whatomate",
			Subject:   middleware.JWTSubjectRefresh,
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(a.Config.JWT.Secret))
	if err != nil {
		return refreshTokenIssueResult{}, err
	}

	if signed == "" {
		return refreshTokenIssueResult{}, fmt.Errorf("refresh token signing returned an empty credential")
	}

	// Store JTI in Redis so it can be revoked
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Redis.Set(ctx, refreshTokenKey(jti), user.ID.String(), expiry).Err(); err != nil {
		return refreshTokenIssueResult{storeUnavailable: true}, fmt.Errorf("refresh token store unavailable: %w", err)
	}

	return refreshTokenIssueResult{token: signed}, nil
}

// refreshTokenKey returns the Redis key for a refresh token JTI.
func refreshTokenKey(jti string) string {
	return fmt.Sprintf("refresh:%s", jti)
}

// SwitchOrgRequest represents the request body for switching organization
type SwitchOrgRequest struct {
	OrganizationID uuid.UUID `json:"organization_id"`
}

// SwitchOrg generates new tokens for a different organization the user belongs to
func (a *App) SwitchOrg(r *fastglue.Request) error {
	userID, ok := r.RequestCtx.UserValue("user_id").(uuid.UUID)
	if !ok {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	var req SwitchOrgRequest
	if err := a.decodeRequest(r, &req); err != nil {
		return nil
	}

	if req.OrganizationID == uuid.Nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "organization_id is required", nil, "")
	}

	// Verify the organization exists
	var org models.Organization
	if err := a.DB.Where("id = ?", req.OrganizationID).First(&org).Error; err != nil {
		return sendOrganizationAccessError(r, err, fasthttp.StatusNotFound, "Organization not found")
	}

	// Get the user
	var user models.User
	if err := a.DB.Where("id = ?", userID).First(&user).Error; err != nil {
		return sendOrganizationAccessError(r, err, fasthttp.StatusNotFound, "User not found")
	}

	// Resolve the target membership from the database. Reseller-derived
	// memberships must still be backed by an active reseller assignment.
	if err := a.applyOrganizationMembership(&user, req.OrganizationID, false); err != nil {
		return sendOrganizationAccessError(r, err, fasthttp.StatusForbidden, "You are not a member of this organization")
	}

	// Preload role with permissions for the response
	if user.Role != nil && user.RoleID != nil {
		cachedPerms, err := a.GetRolePermissionsCached(*user.RoleID)
		if err != nil {
			return sendOrganizationAccessError(r, err, fasthttp.StatusForbidden, "Role is no longer available")
		}
		permissions := make([]models.Permission, 0, len(cachedPerms))
		for _, p := range cachedPerms {
			parts := splitPermission(p)
			if len(parts) == 2 {
				permissions = append(permissions, models.Permission{
					Resource: parts[0],
					Action:   parts[1],
				})
			}
		}
		user.Role.Permissions = permissions
	}

	// Generate new tokens with the target org
	accessToken, err := a.generateAccessToken(&user)
	if err != nil {
		a.Log.Error("Failed to generate access token", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to generate token", nil, "")
	}

	refreshToken, err := a.generateRefreshToken(&user)
	if err != nil {
		a.Log.Error("Failed to generate refresh token", "error", err)
		return sendRefreshTokenIssueError(r, refreshToken)
	}

	a.setAuthCookies(r, accessToken, refreshToken.token)

	return r.SendEnvelope(CookieAuthResponse{
		ExpiresIn: a.Config.JWT.AccessExpiryMins * 60,
		User:      user,
	})
}

// LogoutRequest represents logout request body
type LogoutRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// Logout invalidates the user's refresh token
func (a *App) Logout(r *fastglue.Request) error {
	// Read refresh token from cookie first, fall back to body.
	refreshTokenStr := string(r.RequestCtx.Request.Header.Cookie(cookieRefreshName))
	if refreshTokenStr == "" {
		var req LogoutRequest
		_ = r.Decode(&req, "json")
		refreshTokenStr = req.RefreshToken
	}

	if refreshTokenStr != "" {
		// Only a genuine refresh token is revoked, and with it everything it
		// was already rotated into: after a lost rotation response the
		// client may still present the predecessor, whose grace entry would
		// otherwise keep handing out the successor. An absent, forged or
		// expired token still logs out; it just revokes nothing.
		token, err := jwt.ParseWithClaims(
			refreshTokenStr,
			&middleware.JWTClaims{},
			func(token *jwt.Token) (any, error) {
				return []byte(a.Config.JWT.Secret), nil
			},
			jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		)
		if err == nil && token.Valid {
			if claims, ok := token.Claims.(*middleware.JWTClaims); ok && claims.ID != "" && middleware.IsRefreshJWTClaims(claims) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := a.revokeRefreshChain(ctx, claims.ID); err != nil {
					a.Log.Error("Logout could not revoke the refresh token", "reason", "revoke_store_unavailable", "user_id", claims.UserID)
					// The token is still live: say so, so the client retries
					// rather than trusting a logout that left it usable. This
					// browser's own cookies are cleared either way.
					a.clearAuthCookies(r)
					return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Logout could not revoke the session, please retry", nil, "")
				}
			}
		}
	}

	a.clearAuthCookies(r)

	return r.SendEnvelope(map[string]string{"status": "logged_out"})
}

func generateSlug(name string) string {
	// Simple slug generation - in production, use a proper slugify library
	slug := ""
	for _, c := range name {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			slug += string(c)
		} else if c >= 'A' && c <= 'Z' {
			slug += string(c + 32)
		} else if c == ' ' || c == '-' {
			slug += "-"
		}
	}
	return slug + "-" + uuid.New().String()[:8]
}

// GetWSToken returns a short-lived single-use JWT for WebSocket authentication.
// This is needed because httpOnly cookies cannot be read by JavaScript to pass
// as a query parameter to the WebSocket connection URL.
func (a *App) GetWSToken(r *fastglue.Request) error {
	userID, ok := r.RequestCtx.UserValue("user_id").(uuid.UUID)
	if !ok {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	orgID, ok := r.RequestCtx.UserValue("organization_id").(uuid.UUID)
	if !ok {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	// Re-resolve the principal and selected organization at issuance time. The
	// request context identifies the candidate only; current database state is
	// authoritative for active status, membership, and purpose classification.
	var user models.User
	if err := a.DB.Where("id = ?", userID).First(&user).Error; err != nil {
		return sendOrganizationAccessError(r, err, fasthttp.StatusUnauthorized, "Unauthorized")
	}
	if !user.IsActive {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	if err := a.applyOrganizationMembership(&user, orgID, false); err != nil {
		return sendOrganizationAccessError(r, err, fasthttp.StatusUnauthorized, "Organization access is no longer available")
	}

	claims := middleware.JWTClaims{
		UserID:         user.ID,
		OrganizationID: user.OrganizationID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(30 * time.Second)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    "whatomate",
			Subject:   middleware.JWTSubjectWebSocket,
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(a.Config.JWT.Secret))
	if err != nil {
		a.Log.Error("Failed to generate WS token", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to generate token", nil, "")
	}

	return r.SendEnvelope(map[string]string{"token": signed})
}

// SessionTerminationClassifier provides the fixed session proof used by API quotas.
// Only a verified UUID and credential-free cleanup eligibility leave this layer;
// raw cookies/tokens and unsigned identity fields never choose a quota key.
func SessionTerminationClassifier(secret string) func(*fastglue.Request) (uuid.UUID, bool) {
	return func(r *fastglue.Request) (uuid.UUID, bool) {
		_, accessOnly, valid := sessionTerminationCredential(r)
		if !valid {
			return uuid.Nil, false
		}
		userID, _ := sessionTerminationUser(r, secret)
		return userID, accessOnly
	}
}

func sessionTerminationUser(r *fastglue.Request, secret string) (uuid.UUID, bool) {
	credential, accessCredential, ok := sessionTerminationCredential(r)
	if !ok {
		return uuid.Nil, false
	}
	token, err := jwt.ParseWithClaims(credential, &middleware.JWTClaims{}, func(*jwt.Token) (any, error) {
		return []byte(secret), nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), jwt.WithExpirationRequired())
	if err != nil || !token.Valid {
		return uuid.Nil, false
	}
	claims, ok := token.Claims.(*middleware.JWTClaims)
	if !ok || claims.UserID == uuid.Nil || claims.OrganizationID == uuid.Nil ||
		(accessCredential && !middleware.IsAccessJWTClaims(claims)) ||
		(!accessCredential && !middleware.IsRefreshJWTClaims(claims)) {
		return uuid.Nil, false
	}
	return claims.UserID, true
}

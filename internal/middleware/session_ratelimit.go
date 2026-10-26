package middleware

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

// APISessionRateLimit keeps ordinary API and refresh limits, while a proven
// session termination uses its own per-user quota. Shared-IP refresh traffic
// must not prevent a user revoking their session. This only selects a quota:
// handlers still validate credentials, CSRF and revocation before changing state.
func APISessionRateLimit(apiOpts, refreshOpts RateLimitOpts, secret string) fastglue.FastMiddleware {
	apiLimit := UserAwareRateLimit(apiOpts)
	refreshLimit := RateLimit(refreshOpts)
	terminationOpts := refreshOpts
	terminationOpts.KeyPrefix = refreshOpts.KeyPrefix + "_termination"
	cleanupOpts := refreshOpts
	cleanupOpts.KeyPrefix = refreshOpts.KeyPrefix + "_cleanup"
	anonymousCleanupOpts := cleanupOpts
	anonymousCleanupOpts.KeyPrefix += "_anonymous"
	cleanupLimit := RateLimit(anonymousCleanupOpts)
	return func(r *fastglue.Request) *fastglue.Request {
		if string(r.RequestCtx.Path()) == "/api/auth/logout" {
			if _, accessOnly, ok := sessionTerminationCredential(r); ok && accessOnly {
				// Cookie clearing follows revocation and has its own quota, even
				// if the revoke used the last slot. No refresh credential is
				// present, so Logout can only clear cookies, never revoke.
				if userID, verified := sessionTerminationUser(r, secret); verified {
					return enforce(cleanupOpts, r, fmt.Sprintf("ratelimit:%s:u:%s", cleanupOpts.KeyPrefix, userID))
				}
				// Expired or absent access claims select no identity. Cleanup
				// remains independently bounded per IP.
				return cleanupLimit(r)
			}
		}
		if userID, ok := sessionTerminationUser(r, secret); ok {
			return enforce(terminationOpts, r, fmt.Sprintf("ratelimit:%s:u:%s", terminationOpts.KeyPrefix, userID))
		}
		if apiLimit(r) == nil {
			return nil
		}
		if string(r.RequestCtx.Method()) == fasthttp.MethodPost &&
			string(r.RequestCtx.Path()) == "/api/auth/refresh" {
			return refreshLimit(r)
		}
		return r
	}
}

// A bounded, valid JSON request and matching CSRF proof identify termination.
// The credential is separately verified before selecting a user quota.
func sessionTerminationCredential(r *fastglue.Request) (string, bool, bool) {
	path := string(r.RequestCtx.Path())
	if string(r.RequestCtx.Method()) != fasthttp.MethodPost ||
		(path != "/api/auth/refresh" && path != "/api/auth/logout") {
		return "", false, false
	}
	bodyBytes := r.RequestCtx.PostBody()
	if len(bodyBytes) == 0 || len(bodyBytes) > 16*1024 {
		return "", false, false
	}
	mediaType, _, _ := strings.Cut(string(r.RequestCtx.Request.Header.ContentType()), ";")
	if !strings.EqualFold(strings.TrimSpace(mediaType), "application/json") {
		return "", false, false
	}
	var body struct {
		RefreshToken string `json:"refresh_token"`
		Revoke       bool   `json:"revoke"`
		KeepCookies  bool   `json:"keep_cookies"`
		SSOExchange  string `json:"sso_exchange"`
	}
	if err := json.Unmarshal(bodyBytes, &body); err != nil ||
		body.SSOExchange != "" || (path == "/api/auth/refresh" && !body.Revoke) {
		return "", false, false
	}
	csrfCookie := r.RequestCtx.Request.Header.Cookie("whm_csrf")
	csrfHeader := r.RequestCtx.Request.Header.Peek("X-CSRF-Token")
	if len(csrfCookie) == 0 || subtle.ConstantTimeCompare(csrfCookie, csrfHeader) != 1 {
		return "", false, false
	}

	// Match the handler's cookie-before-body precedence. Browser logout lacks
	// the path-scoped refresh cookie, so a valid access cookie can identify its
	// cookie-clearing request; it grants no other API access.
	credential := string(r.RequestCtx.Request.Header.Cookie("whm_refresh"))
	if credential == "" {
		credential = body.RefreshToken
	}
	accessCredential := false
	if credential == "" && path == "/api/auth/logout" {
		credential = string(r.RequestCtx.Request.Header.Cookie("whm_access"))
		accessCredential = true
	}
	return credential, accessCredential, true
}

func sessionTerminationUser(r *fastglue.Request, secret string) (uuid.UUID, bool) {
	credential, accessCredential, ok := sessionTerminationCredential(r)
	if !ok {
		return uuid.Nil, false
	}
	token, err := jwt.ParseWithClaims(credential, &JWTClaims{}, func(*jwt.Token) (any, error) {
		return []byte(secret), nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), jwt.WithExpirationRequired())
	if err != nil || !token.Valid {
		return uuid.Nil, false
	}
	claims, ok := token.Claims.(*JWTClaims)
	if !ok || claims.UserID == uuid.Nil || claims.OrganizationID == uuid.Nil ||
		(accessCredential && !IsAccessJWTClaims(claims)) ||
		(!accessCredential && !IsRefreshJWTClaims(claims)) {
		return uuid.Nil, false
	}
	return claims.UserID, true
}

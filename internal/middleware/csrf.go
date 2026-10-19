package middleware

import (
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

// CSRFProtection returns a middleware that validates CSRF tokens on mutating
// requests that use cookie-based authentication.
func CSRFProtection() fastglue.FastMiddleware {
	return func(r *fastglue.Request) *fastglue.Request {
		method := string(r.RequestCtx.Method())

		// Only validate mutating methods.
		if method != "POST" && method != "PUT" && method != "DELETE" && method != "PATCH" {
			return r
		}

		// The token refresh endpoint is exempt. Each of its actions needs a
		// credential a cross-site POST never carries, and a cross-site page
		// cannot read the response:
		// - a refresh rotates the caller's own cookies with whm_refresh,
		//   SameSite=Lax and scoped to this path;
		// - a revoke ({"revoke": true}) checks the CSRF header in the handler;
		// - the SSO code exchange ({"sso_exchange": code}), which issues a
		//   session, needs its attempt's binding cookie (SameSite=Lax, httpOnly,
		//   this path) and a JSON body, which a cross-site form cannot send.
		// Without the exemption, a refresh that carries a freshly rotated
		// whm_access cookie (path /api covers this route) but no X-CSRF-Token
		// header is rejected with 403, which logs the user out. The API is
		// served at /api/... (a base path is stripped before routing), matching
		// the public-route checks in the auth middleware.
		if method == "POST" && string(r.RequestCtx.Path()) == "/api/auth/refresh" {
			return r
		}

		// Skip if the request uses header-based auth (API key or Bearer token).
		// These are not automatically attached by the browser, so CSRF is not a concern.
		if len(r.RequestCtx.Request.Header.Peek("Authorization")) > 0 ||
			len(r.RequestCtx.Request.Header.Peek("X-API-Key")) > 0 {
			return r
		}

		// Skip if there is no access cookie — the auth middleware will reject
		// the request with 401 anyway.
		cookieVal := r.RequestCtx.Request.Header.Cookie("whm_access")
		if len(cookieVal) == 0 {
			return r
		}

		// Double-submit: compare whm_csrf cookie with X-CSRF-Token header.
		csrfCookie := string(r.RequestCtx.Request.Header.Cookie("whm_csrf"))
		csrfHeader := string(r.RequestCtx.Request.Header.Peek("X-CSRF-Token"))

		if csrfCookie == "" || csrfHeader == "" || csrfCookie != csrfHeader {
			r.RequestCtx.SetStatusCode(fasthttp.StatusForbidden)
			r.RequestCtx.SetContentType("application/json")
			r.RequestCtx.SetBodyString(`{"status":"error","message":"CSRF token mismatch"}`)
			return nil
		}

		return r
	}
}

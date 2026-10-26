package middleware

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"github.com/zerodha/logf"
)

// RateLimitOpts configures the rate limit middleware.
type RateLimitOpts struct {
	Redis      *redis.Client
	Log        logf.Logger
	Max        int           // Maximum attempts within the window.
	Window     time.Duration // Fixed window duration.
	KeyPrefix  string        // Redis key prefix (e.g., "login", "register").
	TrustProxy bool          // Trust X-Forwarded-For / X-Real-IP headers.
}

// RateLimit returns a fastglue middleware that enforces a fixed-window
// rate limit per client IP using Redis INCR + EXPIRE.
// It fails open: if Redis is unavailable the request is allowed through.
func RateLimit(opts RateLimitOpts) fastglue.FastMiddleware {
	return func(r *fastglue.Request) *fastglue.Request {
		ip := extractClientIP(r, opts.TrustProxy)
		key := fmt.Sprintf("ratelimit:%s:%s", opts.KeyPrefix, ip)
		return enforce(opts, r, key)
	}
}

// UserAwareRateLimit returns a middleware that keys by authenticated user ID
// when available, falling back to client IP for unauthenticated requests.
// This prevents shared-IP environments (VPNs, offices) from pooling quotas.
func UserAwareRateLimit(opts RateLimitOpts) fastglue.FastMiddleware {
	return func(r *fastglue.Request) *fastglue.Request {
		identity := ""
		if uid, ok := r.RequestCtx.UserValue(ContextKeyUserID).(uuid.UUID); ok && uid != uuid.Nil {
			identity = "u:" + uid.String()
		} else {
			identity = "ip:" + extractClientIP(r, opts.TrustProxy)
		}
		key := fmt.Sprintf("ratelimit:%s:%s", opts.KeyPrefix, identity)
		return enforce(opts, r, key)
	}
}

// enforce applies the fixed-window limit for an already-computed key. It writes
// a 429 envelope and returns nil when the limit is exceeded, returns the request
// unchanged otherwise, and fails open (allows the request) if Redis is down.
func enforce(opts RateLimitOpts, r *fastglue.Request, key string) *fastglue.Request {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	count, err := opts.Redis.Incr(ctx, key).Result()
	if err != nil {
		// Fail open — log and allow request.
		opts.Log.Error("Rate limit Redis INCR failed", "error", err, "key", key)
		return r
	}

	// Set expiry on first increment (new window).
	if count == 1 {
		if err := opts.Redis.Expire(ctx, key, opts.Window).Err(); err != nil {
			opts.Log.Error("Rate limit Redis EXPIRE failed", "error", err, "key", key)
		}
	}

	if count > int64(opts.Max) {
		// Look up remaining TTL for Retry-After header.
		ttl, err := opts.Redis.TTL(ctx, key).Result()
		if err != nil || ttl < 0 {
			ttl = opts.Window
		}
		retryAfter := max(int(ttl.Seconds()), 1)

		r.RequestCtx.Response.Header.Set("Retry-After", fmt.Sprintf("%d", retryAfter))
		_ = r.SendErrorEnvelope(fasthttp.StatusTooManyRequests,
			"Too many requests. Please try again later.", nil, "")
		return nil
	}

	return r
}

// extractClientIP returns the client IP address from the request.
// When trustProxy is true, it checks X-Forwarded-For and X-Real-IP headers first.
func extractClientIP(r *fastglue.Request, trustProxy bool) string {
	if trustProxy {
		// X-Forwarded-For may contain a chain: "client, proxy1, proxy2"
		if xff := string(r.RequestCtx.Request.Header.Peek("X-Forwarded-For")); xff != "" {
			parts := strings.SplitN(xff, ",", 2)
			ip := strings.TrimSpace(parts[0])
			if ip != "" {
				return ip
			}
		}
		if realIP := string(r.RequestCtx.Request.Header.Peek("X-Real-IP")); realIP != "" {
			return strings.TrimSpace(realIP)
		}
	}

	// Fall back to RemoteAddr (strip port).
	addr := r.RequestCtx.RemoteAddr().String()
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

// APISessionRateLimit keeps ordinary API and refresh limits, while a proven
// session termination uses its own per-user quota. The trusted classifier is
// fixed by server setup; client fields cannot supply quota keys or policy.
func APISessionRateLimit(apiOpts, refreshOpts RateLimitOpts, classify func(*fastglue.Request) (uuid.UUID, bool)) fastglue.FastMiddleware {
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
		if classify == nil {
			_ = r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "Session authorization is unavailable", nil, "")
			return nil
		}
		userID, cookieCleanup := classify(r)
		if string(r.RequestCtx.Path()) == "/api/auth/logout" && cookieCleanup {
			// Cookie clearing follows revocation and has its own quota. The
			// classifier proves CSRF and the absence of a refresh credential.
			if userID != uuid.Nil {
				return enforce(cleanupOpts, r, fmt.Sprintf("ratelimit:%s:u:%s", cleanupOpts.KeyPrefix, userID))
			}
			// Expired/absent access claims select no identity. Cleanup remains
			// independently bounded per IP, in a separate key namespace.
			return cleanupLimit(r)
		}
		if userID != uuid.Nil {
			return enforce(terminationOpts, r, fmt.Sprintf("ratelimit:%s:u:%s", terminationOpts.KeyPrefix, userID))
		}
		if apiLimit(r) == nil {
			return nil
		}
		if string(r.RequestCtx.Method()) == fasthttp.MethodPost && string(r.RequestCtx.Path()) == "/api/auth/refresh" {
			return refreshLimit(r)
		}
		return r
	}
}

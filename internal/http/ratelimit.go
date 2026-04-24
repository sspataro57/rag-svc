package http

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/treetop/rag-svc/internal/auth"
)

const (
	rateLimitWindow  = 10 * time.Second
	rateLimitPadding = 10 * time.Second // TTL margin past the window
)

// RateLimitOptions configure the per-user rate limit middleware.
type RateLimitOptions struct {
	// PerUserLimit caps requests per window per user. Zero disables.
	PerUserLimit int
	// KeyPrefix namespaces the Redis keys (e.g., "rl:search").
	KeyPrefix string
	// Redis is optional — when nil, the middleware is a no-op. This is
	// intentional: retrieval should never fail because the cache tier is
	// unreachable, and middleware composition stays clean.
	Redis  redis.UniversalClient
	Logger *slog.Logger
}

// RateLimitMiddleware enforces a per-user sliding-ish rate limit using
// Redis INCR + EXPIRE on a fixed 10-second bucket. Fails open on Redis
// errors — the spec treats Redis as a cache tier, not a gate.
func RateLimitMiddleware(opts RateLimitOptions) func(http.Handler) http.Handler {
	if opts.PerUserLimit <= 0 || opts.Redis == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	if opts.KeyPrefix == "" {
		opts.KeyPrefix = "rl"
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, ok := auth.UserFromContext(r.Context())
			if !ok {
				// Unauthenticated request reached the rate limiter.
				// Should never happen if middleware order is correct; fail
				// closed so we don't silently bypass limits.
				http.Error(w, `{"error":"unauthenticated"}`, http.StatusUnauthorized)
				return
			}

			bucket := time.Now().Unix() / int64(rateLimitWindow/time.Second)
			key := fmt.Sprintf("%s:%s:%d", opts.KeyPrefix, user.Email, bucket)

			count, err := opts.Redis.Incr(r.Context(), key).Result()
			if err != nil {
				log.Warn("rate-limit: Redis INCR failed, failing open", "err", err, "user", user.Email)
				next.ServeHTTP(w, r)
				return
			}
			if count == 1 {
				// First increment in this bucket — set TTL. Use a little
				// padding past the window so concurrent clients don't
				// race the expiration.
				_ = opts.Redis.Expire(r.Context(), key, rateLimitWindow+rateLimitPadding).Err()
			}
			if count > int64(opts.PerUserLimit) {
				// Compute seconds until bucket expiry so the caller knows
				// when to retry.
				nextBucketUnix := (bucket + 1) * int64(rateLimitWindow/time.Second)
				retryAfterMS := (nextBucketUnix * 1000) - time.Now().UnixMilli()
				if retryAfterMS < 0 {
					retryAfterMS = 0
				}
				w.Header().Set("Retry-After", strconv.FormatInt(retryAfterMS/1000+1, 10))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				fmt.Fprintf(w, `{"error":"rate_limited","retry_after_ms":%d}`, retryAfterMS)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ensure the middleware still compiles when redis.Nil sentinel is referenced
// (defense against import pruning during refactors).
var _ = errors.Is
var _ = redis.Nil

package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/treetop/rag-svc/internal/auth"
)

func newMiniredis(t *testing.T) (redis.UniversalClient, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	c := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = c.Close() })
	return c, mr
}

// Wrap a handler with a context-injection middleware so ratelimit sees a user.
func withUser(email string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := auth.WithUser(r.Context(), auth.User{Email: email})
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

func TestRateLimit_AllowsUnderThreshold(t *testing.T) {
	rdb, _ := newMiniredis(t)
	mw := RateLimitMiddleware(RateLimitOptions{PerUserLimit: 3, Redis: rdb, KeyPrefix: "rl:test"})

	called := 0
	handler := withUser("u@example.com", mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called++
		w.WriteHeader(http.StatusOK)
	})))

	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", "/search", nil))
		if w.Code != http.StatusOK {
			t.Errorf("request %d: got %d want 200", i, w.Code)
		}
	}
	if called != 3 {
		t.Errorf("inner handler calls: got %d want 3", called)
	}
}

func TestRateLimit_Returns429OverThreshold(t *testing.T) {
	rdb, _ := newMiniredis(t)
	mw := RateLimitMiddleware(RateLimitOptions{PerUserLimit: 2, Redis: rdb, KeyPrefix: "rl:test"})

	handler := withUser("heavy@example.com", mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))

	// First 2 pass, 3rd fails.
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", "/search", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("pre-limit req %d: got %d", i, w.Code)
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/search", nil))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("overlimit: got %d want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Errorf("Retry-After missing")
	}
	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["error"] != "rate_limited" {
		t.Errorf("error field: got %v", body["error"])
	}
	if _, ok := body["retry_after_ms"].(float64); !ok {
		t.Errorf("retry_after_ms missing or wrong type: %v", body["retry_after_ms"])
	}
}

func TestRateLimit_DifferentUsersIndependent(t *testing.T) {
	rdb, _ := newMiniredis(t)
	mw := RateLimitMiddleware(RateLimitOptions{PerUserLimit: 1, Redis: rdb, KeyPrefix: "rl:test"})

	innerA := withUser("a@x.com", mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })))
	innerB := withUser("b@x.com", mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })))

	for _, h := range []http.Handler{innerA, innerB} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/search", nil))
		if w.Code != http.StatusOK {
			t.Errorf("first request for user: got %d", w.Code)
		}
	}
	// A is over limit; B still has budget... wait, B already used theirs above.
	// Use fresh users for this leg.
	innerC := withUser("c@x.com", mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })))
	w := httptest.NewRecorder()
	innerC.ServeHTTP(w, httptest.NewRequest("GET", "/search", nil))
	if w.Code != http.StatusOK {
		t.Errorf("user C first request: got %d", w.Code)
	}

	// A's second request should be rate-limited.
	w = httptest.NewRecorder()
	innerA.ServeHTTP(w, httptest.NewRequest("GET", "/search", nil))
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("user A second request: got %d want 429", w.Code)
	}
}

func TestRateLimit_FailsOpenOnRedisError(t *testing.T) {
	rdb, mr := newMiniredis(t)
	// Stopping miniredis so INCR errors out.
	mr.Close()

	mw := RateLimitMiddleware(RateLimitOptions{PerUserLimit: 1, Redis: rdb, KeyPrefix: "rl:test"})
	called := false
	h := withUser("u@x.com", mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})))

	// Multiple requests should all succeed — fail-open.
	for i := 0; i < 5; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/search", nil))
		if w.Code != http.StatusOK {
			t.Errorf("req %d: got %d (expected pass-through on Redis error)", i, w.Code)
		}
	}
	if !called {
		t.Error("handler never invoked — fail-open broken")
	}
}

func TestRateLimit_DisabledWhenLimitZero(t *testing.T) {
	rdb, _ := newMiniredis(t)
	mw := RateLimitMiddleware(RateLimitOptions{PerUserLimit: 0, Redis: rdb})
	h := withUser("u@x.com", mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))

	for i := 0; i < 10; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/search", nil))
		if w.Code != http.StatusOK {
			t.Errorf("req %d: got %d", i, w.Code)
		}
	}
}

func TestRateLimit_RejectsRequestsWithoutUserInContext(t *testing.T) {
	rdb, _ := newMiniredis(t)
	mw := RateLimitMiddleware(RateLimitOptions{PerUserLimit: 5, Redis: rdb})
	h := mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("must not reach handler without user")
	}))
	w := httptest.NewRecorder()
	// No auth.WithUser on this context.
	h.ServeHTTP(w, httptest.NewRequest("GET", "/search", nil).WithContext(context.Background()))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("code: got %d want 401", w.Code)
	}
}

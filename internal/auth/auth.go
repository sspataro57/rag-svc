// Package auth provides the session middleware and user-context plumbing
// used by rag-svc's authenticated HTTP endpoints.
//
// Step 4 implements stub-mode only: when OIDC_ISSUER is empty, the
// middleware parses the `rag_svc_session` cookie as `stub:<email>` and
// trusts the caller. Step 8 (web UI) will plug in real OIDC ID-token
// validation behind the same Middleware + context interface.
package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// User represents the authenticated caller. Email is the primary identifier
// (OIDC `email` claim in real mode, arbitrary string after `stub:` in dev).
type User struct {
	Email string
}

// ctxKey is unexported so external packages can't poke values into our
// context slot.
type ctxKey struct{}

// WithUser returns a copy of ctx with u attached. Exposed mostly for tests.
func WithUser(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

// UserFromContext returns the user stored by Middleware. The second return
// value is false when no user is in context (unauthenticated codepath).
func UserFromContext(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(ctxKey{}).(User)
	return u, ok
}

// ErrNoSession is returned by ParseStubCookie when the cookie is missing.
var ErrNoSession = errors.New("auth: no session cookie")

// ErrInvalidSession is returned when the cookie value is malformed.
var ErrInvalidSession = errors.New("auth: invalid session cookie")

const stubPrefix = "stub:"

// ParseStubCookie extracts the email from a `stub:<email>` session cookie.
// Exposed so tests can round-trip without hitting a real HTTP path.
func ParseStubCookie(value string) (string, error) {
	if value == "" {
		return "", ErrNoSession
	}
	if !strings.HasPrefix(value, stubPrefix) {
		return "", ErrInvalidSession
	}
	email := strings.TrimPrefix(value, stubPrefix)
	// Minimal shape check — we don't validate deliverability, just that it
	// looks like an email. A caller who wants to lock this down further
	// can layer validation above.
	if !strings.Contains(email, "@") || strings.ContainsAny(email, " \t\r\n") {
		return "", ErrInvalidSession
	}
	return email, nil
}

// Options configure Middleware.
type Options struct {
	// CookieName is the session cookie name (default: rag_svc_session).
	CookieName string
	// StubMode selects stub parsing. When false, the middleware will reject
	// all requests — real OIDC validation plugs in here in step 8.
	StubMode bool
}

func (o Options) withDefaults() Options {
	if o.CookieName == "" {
		o.CookieName = "rag_svc_session"
	}
	return o
}

// Middleware enforces authentication. On success it injects User into the
// request context and calls next. On failure browsers are redirected to
// /login (so the chat UI works); API clients (curl, the extension) get
// 401 with `WWW-Authenticate: Cookie realm="rag-svc"`.
func Middleware(opts Options) func(http.Handler) http.Handler {
	opts = opts.withDefaults()
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			email, err := extractEmail(r, opts)
			if err != nil {
				unauthenticated(w, r)
				return
			}
			ctx := WithUser(r.Context(), User{Email: email})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func extractEmail(r *http.Request, opts Options) (string, error) {
	c, err := r.Cookie(opts.CookieName)
	if err != nil {
		return "", ErrNoSession
	}
	if opts.StubMode {
		return ParseStubCookie(c.Value)
	}
	// Real OIDC validation will live here in step 8.
	return "", ErrInvalidSession
}

package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/treetop/rag-svc/internal/store"
)

// Principal identifies the caller of an MCP request. It's the bearer-
// token analog of the OIDC User — the rest of the pipeline doesn't care
// which identity kind made the request.
type Principal struct {
	TokenID   string // tokens.id (uuid string)
	TokenName string // tokens.name (for logging)
}

type principalCtxKey struct{}

// WithPrincipal attaches p to ctx.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalCtxKey{}, p)
}

// PrincipalFromContext returns the attached Principal if any.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalCtxKey{}).(Principal)
	return p, ok
}

// TokenLookup abstracts the store interface the middleware needs so
// tests can substitute a fake.
type TokenLookup interface {
	LookupActiveToken(ctx context.Context, rawToken string) (store.Token, error)
	TouchLastUsed(ctx context.Context, id [16]byte) error
}

// BearerMiddleware validates `Authorization: Bearer …`. Missing or
// invalid → 401. On success it attaches the Principal to ctx and
// touches the token's last_used_at timestamp.
//
// Unlike the OIDC/stub middleware this one speaks only JSON — MCP
// clients (Claude Code, custom tooling) aren't browsers, so a 401 with a
// JSON-RPC-shaped error body is strictly more useful than a redirect.
func BearerMiddleware(s *store.Store, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := extractBearer(r.Header.Get("Authorization"))
			if raw == "" {
				unauthorizedJSON(w, "missing bearer token")
				return
			}
			tok, err := s.LookupActiveToken(r.Context(), raw)
			if err != nil {
				if errors.Is(err, store.ErrTokenNotFound) {
					unauthorizedJSON(w, "invalid or revoked token")
					return
				}
				logger.Error("bearer: lookup failed", "err", err)
				http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
				return
			}
			// Fire-and-forget last_used update. Log but don't fail the
			// request if it errors — observability is nice-to-have.
			go func() {
				if err := s.TouchLastUsed(context.Background(), tok.ID); err != nil {
					logger.Warn("bearer: touch last_used", "err", err, "token_id", tok.ID)
				}
			}()
			ctx := WithPrincipal(r.Context(), Principal{
				TokenID:   tok.ID.String(),
				TokenName: tok.Name,
			})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// extractBearer returns the raw token or "" when absent/malformed. Case-
// insensitive on the scheme; rejects multi-value headers.
func extractBearer(h string) string {
	h = strings.TrimSpace(h)
	if h == "" {
		return ""
	}
	// RFC 7235 format: "Bearer <token>". Comma-separated alt schemes
	// aren't supported — `Authorization: Bearer X, Basic Y` is weird
	// and we treat it as malformed.
	parts := strings.SplitN(h, " ", 2)
	if len(parts) != 2 {
		return ""
	}
	if !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

func unauthorizedJSON(w http.ResponseWriter, message string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="rag-svc"`)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32001,"message":"` + message + `"}}`))
}

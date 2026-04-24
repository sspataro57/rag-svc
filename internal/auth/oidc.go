package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// OIDCConfig wires the provider + client credentials the OIDC handlers
// need. SessionCookie and RedirectURL default to the values CLAUDE.md
// prescribes; callers can override.
type OIDCConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	CookieName   string
	CookieDomain string // optional; empty = host-only cookie
	// Scopes is additive to openid+email+profile.
	ExtraScopes []string
}

// OIDC bundles the verifier + oauth2 config used by /login and
// /auth/callback. The verifier is reused per request.
type OIDC struct {
	cfg      OIDCConfig
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	oauth    *oauth2.Config

	mu    sync.Mutex
	nonce map[string]time.Time
}

// NewOIDC discovers the provider and wires an ID-token verifier. Call
// this at process startup; it reaches out to the issuer's
// `.well-known/openid-configuration` endpoint.
func NewOIDC(ctx context.Context, cfg OIDCConfig) (*OIDC, error) {
	if cfg.CookieName == "" {
		cfg.CookieName = "rag_svc_session"
	}
	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc: discover %s: %w", cfg.Issuer, err)
	}
	scopes := append([]string{oidc.ScopeOpenID, "email", "profile"}, cfg.ExtraScopes...)
	oc := &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  cfg.RedirectURL,
		Scopes:       scopes,
	}
	verifier := provider.Verifier(&oidc.Config{ClientID: cfg.ClientID})
	return &OIDC{
		cfg:      cfg,
		provider: provider,
		verifier: verifier,
		oauth:    oc,
		nonce:    map[string]time.Time{},
	}, nil
}

// HandleLogin redirects the user to the OIDC issuer's authorize endpoint.
// The state cookie is signed-ish by being a random 32-byte token we
// remember in memory with a 10-minute TTL.
func (o *OIDC) HandleLogin(w http.ResponseWriter, r *http.Request) {
	state := randToken()
	o.mu.Lock()
	o.nonce[state] = time.Now().Add(10 * time.Minute)
	// Opportunistic GC.
	for k, exp := range o.nonce {
		if time.Now().After(exp) {
			delete(o.nonce, k)
		}
	}
	o.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     "rag_svc_state",
		Value:    state,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
		MaxAge:   600,
	})
	http.Redirect(w, r, o.oauth.AuthCodeURL(state), http.StatusFound)
}

// HandleCallback finishes the authorize flow: verifies state, exchanges
// the code, validates the ID token, and writes the session cookie.
func (o *OIDC) HandleCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	stateCookie, err := r.Cookie("rag_svc_state")
	if err != nil {
		http.Error(w, "missing state cookie", http.StatusBadRequest)
		return
	}
	qState := r.URL.Query().Get("state")
	if qState == "" || qState != stateCookie.Value {
		http.Error(w, "state mismatch", http.StatusBadRequest)
		return
	}
	o.mu.Lock()
	exp, ok := o.nonce[qState]
	delete(o.nonce, qState)
	o.mu.Unlock()
	if !ok || time.Now().After(exp) {
		http.Error(w, "stale state", http.StatusBadRequest)
		return
	}

	tok, err := o.oauth.Exchange(ctx, r.URL.Query().Get("code"))
	if err != nil {
		http.Error(w, "token exchange failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	rawID, _ := tok.Extra("id_token").(string)
	if rawID == "" {
		http.Error(w, "no id_token in response", http.StatusBadGateway)
		return
	}
	idTok, err := o.verifier.Verify(ctx, rawID)
	if err != nil {
		http.Error(w, "id_token verify failed: "+err.Error(), http.StatusUnauthorized)
		return
	}
	var claims struct {
		Email    string `json:"email"`
		Verified bool   `json:"email_verified"`
	}
	if err := idTok.Claims(&claims); err != nil {
		http.Error(w, "claims parse failed", http.StatusInternalServerError)
		return
	}
	if claims.Email == "" {
		http.Error(w, "no email claim", http.StatusForbidden)
		return
	}

	// Clear the state cookie.
	http.SetCookie(w, &http.Cookie{Name: "rag_svc_state", Value: "", Path: "/", MaxAge: -1})
	// Set the session cookie. We store the raw ID token; the middleware
	// re-verifies it on every request. Far simpler than standing up a
	// server-side session table for v1.
	http.SetCookie(w, &http.Cookie{
		Name:     o.cfg.CookieName,
		Value:    rawID,
		Path:     "/",
		Domain:   o.cfg.CookieDomain,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
		// Same lifetime as the token; the middleware will bounce
		// expired tokens to /login.
		Expires: idTok.Expiry,
	})
	// Land the user on /chat — that's the one page a freshly-logged-in
	// user probably wants.
	http.Redirect(w, r, "/chat", http.StatusFound)
}

// Verify validates a raw ID token and extracts the email claim. Used by
// the session middleware in real-OIDC mode.
func (o *OIDC) Verify(ctx context.Context, rawIDToken string) (string, error) {
	idTok, err := o.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return "", err
	}
	var claims struct {
		Email string `json:"email"`
	}
	if err := idTok.Claims(&claims); err != nil {
		return "", err
	}
	if claims.Email == "" {
		return "", errors.New("auth: id token has no email")
	}
	return claims.Email, nil
}

// MiddlewareWithOIDC returns auth middleware that uses OIDC validation
// when o is non-nil, falling back to stub parsing when o is nil.
func MiddlewareWithOIDC(o *OIDC, cookieName string) func(http.Handler) http.Handler {
	if o == nil {
		return Middleware(Options{CookieName: cookieName, StubMode: true})
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := r.Cookie(cookieName)
			if err != nil {
				unauthenticated(w, r)
				return
			}
			email, err := o.Verify(r.Context(), c.Value)
			if err != nil {
				unauthenticated(w, r)
				return
			}
			ctx := WithUser(r.Context(), User{Email: email})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func unauthenticated(w http.ResponseWriter, r *http.Request) {
	// Browser requests (Accept: text/html) get a redirect to /login for
	// UX. API clients (extension, curl) get 401 with WWW-Authenticate.
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		redirectTo := "/login"
		if r.URL.Path != "" && r.URL.Path != "/" {
			redirectTo += "?next=" + url.QueryEscape(r.URL.Path)
		}
		http.Redirect(w, r, redirectTo, http.StatusFound)
		return
	}
	w.Header().Set("WWW-Authenticate", `Cookie realm="rag-svc"`)
	http.Error(w, `{"error":"unauthenticated"}`, http.StatusUnauthorized)
}

func randToken() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

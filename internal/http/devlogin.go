package http

import (
	"fmt"
	"net/http"
	"strings"
)

// handleDevLogin sets the stub session cookie. Only mounted when
// `OIDC_ISSUER` is empty; step 8 will remove this when real OIDC lands.
//
// Usage:
//
//	curl -i -c /tmp/jar 'http://localhost:8080/dev/login?email=you@example.com'
//	curl -b /tmp/jar 'http://localhost:8080/search?q=...'
//
// or, in a browser, visit /dev/login?email=... once and subsequent
// /search requests ride the cookie automatically.
func (s *Server) handleDevLogin(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimSpace(r.URL.Query().Get("email"))
	if email == "" || !strings.Contains(email, "@") {
		http.Error(w, `{"error":"email query param required"}`, http.StatusBadRequest)
		return
	}

	cookie := &http.Cookie{
		Name:     s.cfg.Auth.OIDCSessionCookie,
		Value:    "stub:" + email,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// Not Secure: dev-only helper, runs on plain http://localhost.
		MaxAge: 60 * 60 * 24 * 7, // 7 days
	}
	http.SetCookie(w, cookie)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "stub session set for %s; cookie %q valid for 7d\n", email, cookie.Name)
}

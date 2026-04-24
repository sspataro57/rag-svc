package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseStubCookie(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr error
	}{
		{"stub:foo@bar.com", "foo@bar.com", nil},
		{"stub:a@b", "a@b", nil},
		{"", "", ErrNoSession},
		{"nope", "", ErrInvalidSession},
		{"stub:", "", ErrInvalidSession},
		{"stub:no-at-sign", "", ErrInvalidSession},
		{"stub:has spaces@x.com", "", ErrInvalidSession},
		{"real-oidc-token-here", "", ErrInvalidSession},
	}
	for _, c := range cases {
		got, err := ParseStubCookie(c.in)
		if got != c.want || err != c.wantErr {
			t.Errorf("ParseStubCookie(%q) = (%q, %v); want (%q, %v)", c.in, got, err, c.want, c.wantErr)
		}
	}
}

func TestMiddleware_StubSuccess(t *testing.T) {
	var gotUser User
	h := Middleware(Options{StubMode: true})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := UserFromContext(r.Context())
		if !ok {
			t.Fatal("no user in context")
		}
		gotUser = u
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/search?q=x", nil)
	req.AddCookie(&http.Cookie{Name: "rag_svc_session", Value: "stub:alice@treetopllc.com"})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("code: got %d want 200", w.Code)
	}
	if gotUser.Email != "alice@treetopllc.com" {
		t.Errorf("user: got %+v", gotUser)
	}
}

func TestMiddleware_MissingCookieReturns401WithWWWAuthenticate(t *testing.T) {
	h := Middleware(Options{StubMode: true})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("handler must not be called on unauthenticated request")
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/search?q=x", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code: got %d want 401", w.Code)
	}
	if got := w.Header().Get("WWW-Authenticate"); got != `Cookie realm="rag-svc"` {
		t.Errorf("WWW-Authenticate: got %q", got)
	}
}

func TestMiddleware_InvalidCookie401(t *testing.T) {
	h := Middleware(Options{StubMode: true})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler must not run on invalid cookie")
	}))

	req := httptest.NewRequest("GET", "/search?q=x", nil)
	req.AddCookie(&http.Cookie{Name: "rag_svc_session", Value: "not-stub-format"})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("got %d want 401", w.Code)
	}
}

func TestMiddleware_NonStubMode_RejectsCookieWithoutOIDC(t *testing.T) {
	// StubMode=false means real OIDC validation required; no implementation
	// yet (step 8), so every request must 401. This guards against
	// accidentally deploying stub parsing in production.
	h := Middleware(Options{StubMode: false})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("handler must not run in non-stub mode until OIDC is wired")
	}))
	req := httptest.NewRequest("GET", "/search?q=x", nil)
	req.AddCookie(&http.Cookie{Name: "rag_svc_session", Value: "stub:foo@bar.com"})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("got %d want 401", w.Code)
	}
}

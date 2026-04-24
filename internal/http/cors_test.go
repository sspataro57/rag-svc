package http

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCORS_AllowedOriginGetsHeaders(t *testing.T) {
	mw := CORSMiddleware(CORSOptions{AllowedOrigins: []string{"https://rag.treetopllc.com"}})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/search?q=x", nil)
	req.Header.Set("Origin", "https://rag.treetopllc.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://rag.treetopllc.com" {
		t.Errorf("Allow-Origin: got %q", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("Allow-Credentials: got %q", got)
	}
	if got := w.Header().Get("Vary"); got != "Origin" {
		t.Errorf("Vary: got %q", got)
	}
}

func TestCORS_PreflightFromAllowedOrigin(t *testing.T) {
	mw := CORSMiddleware(CORSOptions{
		AllowedOrigins: []string{"chrome-extension://abcd"},
	})
	h := mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("preflight should not reach the inner handler")
	}))

	req := httptest.NewRequest("OPTIONS", "/search", nil)
	req.Header.Set("Origin", "chrome-extension://abcd")
	req.Header.Set("Access-Control-Request-Method", "GET")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("code: got %d want 204", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Errorf("Allow-Methods missing")
	}
	if got := w.Header().Get("Access-Control-Max-Age"); got != "3600" {
		t.Errorf("Max-Age: got %q", got)
	}
}

func TestCORS_DisallowedOriginNoHeaders(t *testing.T) {
	mw := CORSMiddleware(CORSOptions{AllowedOrigins: []string{"https://rag.treetopllc.com"}})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/search?q=x", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin should be empty for disallowed origin, got %q", got)
	}
	// The request itself still reaches the handler; the browser is the gate.
	if w.Code != http.StatusOK {
		t.Errorf("expected inner handler to run (got code %d)", w.Code)
	}
}

func TestCORS_PreflightFromDisallowedOriginForbidden(t *testing.T) {
	mw := CORSMiddleware(CORSOptions{AllowedOrigins: []string{"https://rag.treetopllc.com"}})
	h := mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("disallowed preflight should not reach handler")
	}))

	req := httptest.NewRequest("OPTIONS", "/search", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("code: got %d want 403", w.Code)
	}
}

func TestBuildCORSOrigins(t *testing.T) {
	cases := []struct {
		ext  string
		web  string
		want []string
	}{
		{"", "", nil},
		{"CHANGEME", "", nil},
		{"abc123", "", []string{"chrome-extension://abc123"}},
		{"", "https://rag.treetopllc.com", []string{"https://rag.treetopllc.com"}},
		{"abc123", "https://rag.treetopllc.com", []string{"chrome-extension://abc123", "https://rag.treetopllc.com"}},
	}
	for _, c := range cases {
		got := BuildCORSOrigins(c.ext, c.web)
		if len(got) != len(c.want) {
			t.Errorf("BuildCORSOrigins(%q,%q): got %v want %v", c.ext, c.web, got, c.want)
			continue
		}
		for i := range c.want {
			if got[i] != c.want[i] {
				t.Errorf("BuildCORSOrigins(%q,%q)[%d]: got %q want %q", c.ext, c.web, i, got[i], c.want[i])
			}
		}
	}
}

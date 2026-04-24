package http

import (
	"net/http"
	"strings"
)

// CORSOptions configures the CORS middleware. Allowed origins must be fully
// qualified (scheme + host [+ port]) — e.g., "https://rag.treetopllc.com" or
// "chrome-extension://abcdef...". No wildcards (rag-svc treats CORS as an
// allowlist, not a discovery mechanism).
type CORSOptions struct {
	AllowedOrigins []string
	AllowedMethods []string
	AllowedHeaders []string
	MaxAgeSeconds  int
}

func (o CORSOptions) withDefaults() CORSOptions {
	if len(o.AllowedMethods) == 0 {
		o.AllowedMethods = []string{"GET", "POST", "OPTIONS"}
	}
	if len(o.AllowedHeaders) == 0 {
		o.AllowedHeaders = []string{"Content-Type", "Authorization"}
	}
	if o.MaxAgeSeconds == 0 {
		o.MaxAgeSeconds = 3600
	}
	return o
}

// CORSMiddleware returns a middleware that enforces the allowlist.
// Preflight OPTIONS requests from allowed origins short-circuit with 204.
// Requests from disallowed origins proceed without CORS headers — the
// browser will then block them by default, which is what we want.
func CORSMiddleware(opts CORSOptions) func(http.Handler) http.Handler {
	opts = opts.withDefaults()
	allow := make(map[string]struct{}, len(opts.AllowedOrigins))
	for _, o := range opts.AllowedOrigins {
		if o != "" {
			allow[o] = struct{}{}
		}
	}
	methodStr := strings.Join(opts.AllowedMethods, ", ")
	headerStr := strings.Join(opts.AllowedHeaders, ", ")

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			_, allowed := allow[origin]
			if allowed {
				h := w.Header()
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Access-Control-Allow-Credentials", "true")
				h.Add("Vary", "Origin")
				if r.Method == http.MethodOptions {
					h.Set("Access-Control-Allow-Methods", methodStr)
					h.Set("Access-Control-Allow-Headers", headerStr)
					h.Set("Access-Control-Max-Age", itoa(opts.MaxAgeSeconds))
					w.WriteHeader(http.StatusNoContent)
					return
				}
			} else if r.Method == http.MethodOptions {
				// Preflight from a disallowed origin — reject cheaply.
				w.WriteHeader(http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// BuildCORSOrigins derives the allowlist from config: the extension origin
// (when ExtensionID is set) and the web UI origin (when WebUIOrigin is set).
// Separated from CORSMiddleware so the server can log which origins were
// actually allowlisted at startup.
func BuildCORSOrigins(extensionID, webUIOrigin string) []string {
	var out []string
	if extensionID != "" && extensionID != "CHANGEME" {
		out = append(out, "chrome-extension://"+extensionID)
	}
	if webUIOrigin != "" {
		out = append(out, webUIOrigin)
	}
	return out
}

// itoa avoids pulling in strconv for one call site; keeps the file tight.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

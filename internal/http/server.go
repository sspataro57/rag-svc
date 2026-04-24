package http

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/treetop/rag-svc/internal/auth"
	"github.com/treetop/rag-svc/internal/blob"
	"github.com/treetop/rag-svc/internal/config"
	"github.com/treetop/rag-svc/internal/retrieve"
	"github.com/treetop/rag-svc/internal/store"
)

type Server struct {
	cfg       *config.Config
	store     *store.Store
	redis     redis.UniversalClient
	logger    *slog.Logger
	retrieval *retrieve.Deps
	blob      blob.Client
	web       WebMounter
	oidc      *auth.OIDC
	mcp       http.Handler
}

// WebMounter lets the top-level server register the web package's routes
// (chat, documents, login) without the http package importing web — a
// cycle that would otherwise pull in answer + templates.
type WebMounter interface {
	Mount(mux *http.ServeMux, authMW func(http.Handler) http.Handler)
}

func NewServer(cfg *config.Config, s *store.Store, rdb redis.UniversalClient, logger *slog.Logger) *Server {
	return &Server{cfg: cfg, store: s, redis: rdb, logger: logger}
}

// WithRetrieval installs the retrieval deps so /search can run. Optional —
// when unset, /search returns 503. This keeps the server boot-able even in
// contexts where an embedder isn't configured (pure healthz probes).
func (s *Server) WithRetrieval(d *retrieve.Deps) *Server {
	s.retrieval = d
	return s
}

// WithBlob installs the object-store client used by /upload. Optional —
// /upload returns 503 when unset.
func (s *Server) WithBlob(c blob.Client) *Server {
	s.blob = c
	return s
}

// WithWeb installs the web package's handler set (login, chat,
// documents, /static). Optional — the pure API (search, upload)
// still works without it.
func (s *Server) WithWeb(m WebMounter) *Server {
	s.web = m
	return s
}

// WithOIDC installs a real OIDC validator. When set, authenticated
// endpoints use OIDC session validation; otherwise they fall back to
// stub parsing.
func (s *Server) WithOIDC(o *auth.OIDC) *Server {
	s.oidc = o
	return s
}

// WithMCP mounts the MCP JSON-RPC handler behind bearer auth. The
// supplied handler is the raw mcp.Server; the server wraps it with
// auth.BearerMiddleware here.
func (s *Server) WithMCP(h http.Handler) *Server {
	s.mcp = auth.BearerMiddleware(s.store, s.logger)(h)
	return s
}

// Handler returns the HTTP mux for the server. Kept separate from Run so
// tests can exercise handlers directly.
//
// Route tree:
//
//	/healthz, /readyz           bare — no auth, no CORS (K8s probes).
//	/dev/login                  unauthenticated; only mounted in stub mode.
//	/search, /projects, /spaces authenticated endpoints — CORS + auth +
//	                            rate-limit middleware stack.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	stubMode := s.oidc == nil
	if stubMode {
		mux.HandleFunc("GET /dev/login", s.handleDevLogin)
	}

	cors := CORSMiddleware(CORSOptions{
		AllowedOrigins: BuildCORSOrigins(s.cfg.Auth.ExtensionID, s.cfg.Auth.WebUIOrigin),
	})
	var authMW func(http.Handler) http.Handler
	if s.oidc != nil {
		authMW = auth.MiddlewareWithOIDC(s.oidc, s.cfg.Auth.OIDCSessionCookie)
	} else {
		authMW = auth.Middleware(auth.Options{
			CookieName: s.cfg.Auth.OIDCSessionCookie,
			StubMode:   true,
		})
	}
	rl := RateLimitMiddleware(RateLimitOptions{
		PerUserLimit: s.cfg.Search.RateLimitPerUser,
		KeyPrefix:    "rl:search",
		Redis:        s.redis,
		Logger:       s.logger,
	})

	// Middleware composes outermost to innermost: CORS writes its headers
	// first (and short-circuits preflight OPTIONS so they skip auth/RL),
	// then auth extracts the user, then rate-limit enforces the per-user
	// window, then the inner mux dispatches by method.
	authedMux := http.NewServeMux()
	authedMux.HandleFunc("GET /search", s.handleSearch)
	authedMux.HandleFunc("GET /projects", s.handleProjects)
	authedMux.HandleFunc("GET /spaces", s.handleSpaces)
	authedMux.HandleFunc("POST /upload", s.handleUpload)

	stack := cors(authMW(rl(authedMux)))
	mux.Handle("/search", stack)
	mux.Handle("/projects", stack)
	mux.Handle("/spaces", stack)
	mux.Handle("/upload", stack)

	// Mount web package routes (chat UI, documents page, login, etc.)
	// with the same auth middleware but NO CORS/rate-limit — those are
	// API concerns.
	if s.web != nil {
		s.web.Mount(mux, authMW)
	}

	// MCP: static bearer tokens, no OIDC, no CORS. Already wrapped by
	// BearerMiddleware in WithMCP.
	if s.mcp != nil {
		mux.Handle("POST /mcp", s.mcp)
	}

	return mux
}

// Run starts the HTTP server and blocks until ctx is cancelled, then
// gracefully shuts it down.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.Core.HTTPAddr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("http listening", "addr", s.cfg.Core.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	checks := map[string]string{}
	status := http.StatusOK

	if err := s.store.Ping(ctx); err != nil {
		checks["postgres"] = "error: " + err.Error()
		status = http.StatusServiceUnavailable
	} else {
		checks["postgres"] = "ok"
	}

	if err := s.redis.Ping(ctx).Err(); err != nil {
		checks["redis"] = "error: " + err.Error()
		status = http.StatusServiceUnavailable
	} else {
		checks["redis"] = "ok"
	}

	overall := "ok"
	if status != http.StatusOK {
		overall = "degraded"
	}
	writeJSON(w, status, map[string]any{"status": overall, "checks": checks})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

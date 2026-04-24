package http

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/treetop/rag-svc/internal/auth"
	"github.com/treetop/rag-svc/internal/retrieve"
)

const (
	maxQueryLen      = 512
	maxSearchLimit   = 50
	defaultSrchLimit = 10
)

// searchResponse is the wire shape for GET /search. We keep it small and
// explicit so the Chrome extension (and future OpenAPI generation) sees a
// stable schema. Slice fields are never nil — clients can rely on
// `response.hits` being an array.
type searchResponse struct {
	Query string      `json:"query"`
	Hits  []searchHit `json:"hits"`
	Meta  searchMeta  `json:"meta"`
}

type searchHit struct {
	ID             string         `json:"id"`
	Source         string         `json:"source"`
	Title          string         `json:"title"`
	Snippet        string         `json:"snippet"`
	URL            string         `json:"url"`
	ProjectOrSpace string         `json:"project_or_space,omitempty"`
	UpdatedAt      time.Time      `json:"updated_at"`
	Score          float64        `json:"score"`
	Extra          map[string]any `json:"extra,omitempty"`
}

type searchMeta struct {
	Limit       int    `json:"limit"`
	ElapsedMS   int64  `json:"elapsed_ms"`
	TicketShort string `json:"ticket_shortcut,omitempty"` // key if the ticket-key shortcut fired
}

type errorResponse struct {
	Error string `json:"error"`
}

// handleSearch implements GET /search. Query param contract matches CLAUDE.md:
//
//	q             required, 1–512 chars
//	limit         optional, 1–50 (default 10)
//	source        repeatable (jira|confluence|document)
//	project       repeatable
//	space         repeatable
//	updated_after RFC 3339 timestamp
//
// OIDC auth, CORS, and per-user rate limiting are intentionally not wired
// here — step 4 layers those on. For now /search is callable without auth.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if s.retrieval == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "search not configured"})
		return
	}

	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "q is required"})
		return
	}
	if len(q) > maxQueryLen {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "q exceeds 512 chars"})
		return
	}

	limit := defaultSrchLimit
	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > maxSearchLimit {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "limit must be 1..50"})
			return
		}
		limit = n
	}

	filters := retrieve.Filters{
		Sources:  r.URL.Query()["source"],
		Projects: r.URL.Query()["project"],
		Spaces:   r.URL.Query()["space"],
	}
	if ua := r.URL.Query().Get("updated_after"); ua != "" {
		t, err := time.Parse(time.RFC3339, ua)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "updated_after must be RFC 3339"})
			return
		}
		filters.UpdatedAfter = t
	}

	user, _ := auth.UserFromContext(r.Context())

	start := time.Now()
	hits, err := retrieve.SearchHits(r.Context(), *s.retrieval, retrieve.Query{
		Text:        q,
		Filters:     filters,
		Limit:       limit,
		UserContext: &retrieve.UserCtx{Email: user.Email},
	})
	if err != nil {
		s.logger.Error("search failed", "err", err, "q", q, "user", user.Email)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "search failed"})
		return
	}
	elapsed := time.Since(start)
	s.logger.Info("search",
		"user", user.Email,
		"q", truncate(q, 200),
		"hits", len(hits),
		"elapsed_ms", elapsed.Milliseconds(),
	)

	resp := searchResponse{
		Query: q,
		Hits:  make([]searchHit, 0, len(hits)),
		Meta: searchMeta{
			Limit:       limit,
			ElapsedMS:   elapsed.Milliseconds(),
			TicketShort: retrieve.ParseTicketKey(q),
		},
	}
	for _, h := range hits {
		resp.Hits = append(resp.Hits, searchHit{
			ID:             h.ID,
			Source:         h.Source,
			Title:          h.Title,
			Snippet:        h.Snippet,
			URL:            h.URL,
			ProjectOrSpace: h.ProjectOrSpace,
			UpdatedAt:      h.UpdatedAt,
			Score:          h.Score,
			Extra:          h.Extra,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

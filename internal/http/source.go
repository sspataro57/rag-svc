package http

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// sourceResponse is the wire shape for GET /sources. Mirrors the persisted
// row layout, with the same colon-prefixed `id` form that /search emits so
// callers can pipe a search hit's id straight back without reparsing.
type sourceResponse struct {
	ID             string         `json:"id"`
	SourceType     string         `json:"source_type"`
	SourceKey      string         `json:"source_key"`
	ProjectOrSpace string         `json:"project_or_space,omitempty"`
	Title          string         `json:"title"`
	URL            string         `json:"url"`
	BodyMarkdown   string         `json:"body_markdown"`
	Extra          map[string]any `json:"extra,omitempty"`
	UpdatedAt      time.Time      `json:"updated_at"`
	IndexedAt      time.Time      `json:"indexed_at"`
}

// handleGetSource implements GET /sources?id=<source_type>:<source_key>.
//
// The id form matches the `id` field returned by /search hits, so a caller
// holding a hit can fetch its full body without reparsing. Returns 400 on a
// malformed id, 404 when the source isn't indexed, and 200 with the row
// otherwise.
func (s *Server) handleGetSource(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "id is required"})
		return
	}
	srcType, srcKey, ok := parseSourceID(id)
	if !ok {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "id must be <source_type>:<source_key>"})
		return
	}

	row, err := s.store.GetSourceByKey(r.Context(), srcType, srcKey)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "source not found"})
			return
		}
		s.logger.Error("get source failed", "err", err, "id", id)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "lookup failed"})
		return
	}

	writeJSON(w, http.StatusOK, sourceResponse{
		ID:             row.SourceType + ":" + row.SourceKey,
		SourceType:     row.SourceType,
		SourceKey:      row.SourceKey,
		ProjectOrSpace: row.ProjectOrSpace,
		Title:          row.Title,
		URL:            row.URL,
		BodyMarkdown:   row.BodyMarkdown,
		Extra:          row.Extra,
		UpdatedAt:      row.UpdatedAt,
		IndexedAt:      row.IndexedAt,
	})
}

// parseSourceID splits a colon-prefixed id like "jira:API-2302" or
// "document:scheduling-portfolio-2026" into (source_type, source_key). Only
// the first colon is the separator — document keys may legitimately contain
// further colons.
func parseSourceID(id string) (srcType, srcKey string, ok bool) {
	i := strings.IndexByte(id, ':')
	if i <= 0 || i == len(id)-1 {
		return "", "", false
	}
	srcType = id[:i]
	srcKey = id[i+1:]
	switch srcType {
	case "jira", "confluence", "document":
		return srcType, srcKey, true
	}
	return "", "", false
}

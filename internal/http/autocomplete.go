package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"
)

const autocompleteCacheTTL = time.Hour

type autocompleteResponse struct {
	Values []string `json:"values"`
}

// handleProjects returns the distinct project_or_space values for indexed
// Jira sources. Cached in Redis for 1h because ingestion cadence is slow
// enough that stale-by-an-hour is fine.
func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	s.handleAutocomplete(w, r, "jira", "autocomplete:projects")
}

// handleSpaces mirrors handleProjects for Confluence. Returns an empty list
// today because Confluence ingestion lands in step 6 — keeping the handler
// live now means the Chrome extension can wire both endpoints from day one.
func (s *Server) handleSpaces(w http.ResponseWriter, r *http.Request) {
	s.handleAutocomplete(w, r, "confluence", "autocomplete:spaces")
}

func (s *Server) handleAutocomplete(w http.ResponseWriter, r *http.Request, sourceType, cacheKey string) {
	ctx := r.Context()

	if s.redis != nil {
		if cached, err := s.redis.Get(ctx, cacheKey).Bytes(); err == nil {
			var resp autocompleteResponse
			if json.Unmarshal(cached, &resp) == nil {
				writeJSON(w, http.StatusOK, resp)
				return
			}
		} else if !errors.Is(err, redis.Nil) {
			s.logger.Warn("autocomplete: cache get failed", "err", err, "key", cacheKey)
		}
	}

	values, err := s.listDistinctProjectOrSpace(ctx, sourceType)
	if err != nil {
		s.logger.Error("autocomplete: query failed", "err", err, "source", sourceType)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "autocomplete failed"})
		return
	}
	resp := autocompleteResponse{Values: values}

	if s.redis != nil {
		if data, err := json.Marshal(resp); err == nil {
			if err := s.redis.Set(ctx, cacheKey, data, autocompleteCacheTTL).Err(); err != nil {
				s.logger.Warn("autocomplete: cache set failed", "err", err)
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) listDistinctProjectOrSpace(ctx context.Context, sourceType string) ([]string, error) {
	const q = `
SELECT DISTINCT project_or_space
FROM sources
WHERE source_type = $1 AND project_or_space IS NOT NULL
ORDER BY project_or_space`
	rows, err := s.store.Pool().Query(ctx, q, sourceType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0, 32)
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

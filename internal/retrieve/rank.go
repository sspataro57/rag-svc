package retrieve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ticketKeyRE matches a Jira-style issue key: PROJECT-NUMBER where PROJECT
// starts with a letter and may contain letters/digits/underscores. The
// pattern matches the entire input — callers should TrimSpace first.
var ticketKeyRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]+-\d+$`)

// ParseTicketKey returns the normalized ticket key if text is exactly a Jira
// key, else "".
func ParseTicketKey(text string) string {
	key := strings.TrimSpace(text)
	if !ticketKeyRE.MatchString(key) {
		return ""
	}
	return key
}

// fetchJiraByKey runs the ticket-key shortcut described in CLAUDE.md: direct
// source lookup by (source_type, source_key) when the query text is a bare
// ticket key. Returns (hit, true) when found, (_, false) when not.
func fetchJiraByKey(ctx context.Context, q queryer, key string) (Hit, bool, error) {
	const sql = `
SELECT source_type, source_key, project_or_space, title, url, extra, updated_at,
       ts_headline('english',
                   COALESCE(NULLIF(body_markdown, ''), title),
                   plainto_tsquery('english', $2),
                   'MaxWords=40, MinWords=20, StartSel=<mark>, StopSel=</mark>') AS snippet
FROM sources
WHERE source_type = 'jira' AND source_key = $1
LIMIT 1`
	row := q.QueryRow(ctx, sql, key, key)
	var (
		sourceType, sourceKey, title, url, snippet string
		projectOrSpace                             *string
		extra                                      []byte
	)
	var h Hit
	err := row.Scan(&sourceType, &sourceKey, &projectOrSpace, &title, &url, &extra, &h.UpdatedAt, &snippet)
	if errors.Is(err, pgx.ErrNoRows) {
		return Hit{}, false, nil
	}
	if err != nil {
		return Hit{}, false, fmt.Errorf("ticket-key shortcut: %w", err)
	}
	h.ID = sourceType + ":" + sourceKey
	h.Source = sourceType
	h.Title = title
	h.URL = url
	h.Snippet = snippet
	if projectOrSpace != nil {
		h.ProjectOrSpace = *projectOrSpace
	}
	h.Score = 1.0
	if len(extra) > 0 {
		_ = json.Unmarshal(extra, &h.Extra)
	}
	return h, true, nil
}

// queryer is the subset of pgx pool/tx methods we use — stated as an
// interface so tests can swap in a mock when appropriate.
type queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

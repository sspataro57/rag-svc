package retrieve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"

	"github.com/jackc/pgx/v5"
)

// ticketKeyRE matches a Jira-style issue key anywhere in the text:
// PROJECT-NUMBER where PROJECT starts with a letter and may contain
// letters/digits/underscores. Keys are uppercase only, so ordinary words
// like "step-2" don't trigger lookups; something like "UTF-8" does, and
// simply finds no row.
var ticketKeyRE = regexp.MustCompile(`\b[A-Z][A-Z0-9_]+-\d+\b`)

// maxTicketKeys caps how many keys from one query get a direct lookup.
const maxTicketKeys = 5

// ExtractTicketKeys returns the distinct Jira keys named in text, in order
// of first appearance, capped at maxTicketKeys. A bare key ("PLAT-482") and
// a key inside a sentence ("what is PLAT-482 about?") both count.
func ExtractTicketKeys(text string) []string {
	var keys []string
	for _, k := range ticketKeyRE.FindAllString(text, -1) {
		if slices.Contains(keys, k) {
			continue
		}
		keys = append(keys, k)
		if len(keys) == maxTicketKeys {
			break
		}
	}
	return keys
}

// fetchJiraByKey runs the ticket-key shortcut described in CLAUDE.md: direct
// source lookup by (source_type, source_key) for a key named in the query.
// queryText drives the snippet, so a question about the issue highlights
// the relevant part of its body. Returns (hit, true) when found, (_, false)
// when not.
func fetchJiraByKey(ctx context.Context, q queryer, key, queryText string) (Hit, bool, error) {
	const sql = `
SELECT source_type, source_key, project_or_space, title, url, extra, updated_at,
       ts_headline('english',
                   COALESCE(NULLIF(body_markdown, ''), title),
                   plainto_tsquery('english', $2),
                   'MaxWords=40, MinWords=20, StartSel=<mark>, StopSel=</mark>') AS snippet
FROM sources
WHERE source_type = 'jira' AND source_key = $1
LIMIT 1`
	row := q.QueryRow(ctx, sql, key, queryText)
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

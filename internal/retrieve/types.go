// Package retrieve implements the hybrid search pipeline shared by /search,
// /chat, the MCP server, and the Slack app. One public entry point —
// SearchHits — combines the ticket-key shortcut, vector similarity, full-text
// rank, recency boost, and per-source dedup as described in CLAUDE.md.
package retrieve

import "time"

// Query is the single input to SearchHits. All fields are optional except
// Text; callers that need pagination should stitch multiple queries with
// adjusted Limit + post-filter (no offset in v1).
type Query struct {
	Text        string
	Filters     Filters
	Limit       int
	UserContext *UserCtx
}

// Filters narrows results. Any empty slice / zero time is treated as
// "no filter for this dimension" (bound as NULL to the SQL).
type Filters struct {
	Sources      []string // "jira" | "confluence" | "document"
	Projects     []string // Jira project keys
	Spaces       []string // Confluence space keys
	UpdatedAfter time.Time
}

// UserCtx carries caller identity. Step 3 doesn't enforce auth — this is
// plumbing for future telemetry and per-user rate limiting.
type UserCtx struct {
	Email string
	// Future: Scopes, TenantID, etc.
}

// Hit is a ranked source that matches the query. Snippet contains HTML <mark>
// markers around the matched terms (ts_headline output).
type Hit struct {
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

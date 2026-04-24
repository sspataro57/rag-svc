package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/treetop/rag-svc/internal/answer"
	"github.com/treetop/rag-svc/internal/retrieve"
	"github.com/treetop/rag-svc/internal/store"
)

// ToolDeps bundles every collaborator the six built-in tools need.
// The MCP server gets one at startup; every tool call reuses it.
type ToolDeps struct {
	Retrieval retrieve.Deps
	Answerer  *answer.Answerer
	Store     *store.Store

	// DefaultLimit caps search/ask hits when the caller doesn't specify.
	DefaultLimit int
}

// BuiltinTools returns the six tools CLAUDE.md specifies, with JSON
// schemas and handlers wired to deps.
func BuiltinTools(deps ToolDeps) []Tool {
	if deps.DefaultLimit <= 0 {
		deps.DefaultLimit = 10
	}
	return []Tool{
		{
			Name:        "search",
			Description: "Hybrid (vector + BM25) search across Treetop's indexed Jira issues, Confluence pages, and uploaded documents. Returns ranked hits with snippets.",
			InputSchema: mustJSON(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":         map[string]any{"type": "string", "description": "Natural-language query or a Jira issue key for a direct lookup."},
					"limit":         map[string]any{"type": "integer", "minimum": 1, "maximum": 50, "description": "Max hits (default 10)."},
					"source":        map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": []string{"jira", "confluence", "document"}}, "description": "Restrict to one or more source kinds."},
					"project":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Restrict to specific Jira projects."},
					"space":         map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Restrict to specific Confluence spaces."},
					"updated_after": map[string]any{"type": "string", "format": "date-time", "description": "RFC 3339 timestamp. Only return content updated after this."},
				},
				"required": []string{"query"},
			}),
			Handler: deps.searchHandler,
		},
		{
			Name:        "ask",
			Description: "End-to-end RAG: retrieves relevant context and has the answer model produce a cited response. Prefer this over raw `search` when the user wants a synthesized answer.",
			InputSchema: mustJSON(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":   map[string]any{"type": "string", "description": "The question to answer."},
					"source":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"project": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"space":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
				"required": []string{"query"},
			}),
			Handler: deps.askHandler,
		},
		{
			Name:        "list_projects",
			Description: "List Jira project keys that have been indexed into rag-svc.",
			InputSchema: mustJSON(map[string]any{"type": "object", "properties": map[string]any{}}),
			Handler:     deps.listProjectsHandler,
		},
		{
			Name:        "list_spaces",
			Description: "List Confluence space keys that have been indexed into rag-svc.",
			InputSchema: mustJSON(map[string]any{"type": "object", "properties": map[string]any{}}),
			Handler:     deps.listSpacesHandler,
		},
		{
			Name:        "get_issue",
			Description: "Fetch the stored Treetop Jira issue by key (e.g., PLAT-482). Returns title, URL, normalized body (description + comments), status, and issue_type.",
			InputSchema: mustJSON(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"key": map[string]any{"type": "string", "description": "Jira issue key, e.g. PLAT-482"},
				},
				"required": []string{"key"},
			}),
			Handler: deps.getIssueHandler,
		},
		{
			Name:        "get_page",
			Description: "Fetch the stored Confluence page by numeric id OR by its Confluence URL. Returns title, URL, normalized markdown body, and breadcrumb.",
			InputSchema: mustJSON(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":  map[string]any{"type": "string", "description": "Confluence page numeric ID (string-typed)."},
					"url": map[string]any{"type": "string", "description": "Confluence page URL; the page ID is extracted from /pages/{id}."},
				},
			}),
			Handler: deps.getPageHandler,
		},
	}
}

// ---- handlers ----

type searchArgs struct {
	Query        string   `json:"query"`
	Limit        int      `json:"limit,omitempty"`
	Source       []string `json:"source,omitempty"`
	Project      []string `json:"project,omitempty"`
	Space        []string `json:"space,omitempty"`
	UpdatedAfter string   `json:"updated_after,omitempty"`
}

func (d ToolDeps) searchHandler(ctx context.Context, raw json.RawMessage) (ToolResult, error) {
	var args searchArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return ToolResult{}, fmt.Errorf("parse args: %w", err)
	}
	if strings.TrimSpace(args.Query) == "" {
		return ToolResult{}, errors.New("query is required")
	}
	limit := args.Limit
	if limit <= 0 {
		limit = d.DefaultLimit
	}
	filters, err := buildFilters(args.Source, args.Project, args.Space, args.UpdatedAfter)
	if err != nil {
		return ToolResult{}, err
	}
	hits, err := retrieve.SearchHits(ctx, d.Retrieval, retrieve.Query{
		Text:    args.Query,
		Filters: filters,
		Limit:   limit,
	})
	if err != nil {
		return ToolResult{}, fmt.Errorf("search: %w", err)
	}
	return TextResult(map[string]any{
		"query": args.Query,
		"hits":  hits,
		"count": len(hits),
	})
}

type askArgs struct {
	Query   string   `json:"query"`
	Source  []string `json:"source,omitempty"`
	Project []string `json:"project,omitempty"`
	Space   []string `json:"space,omitempty"`
}

func (d ToolDeps) askHandler(ctx context.Context, raw json.RawMessage) (ToolResult, error) {
	if d.Answerer == nil {
		return ToolResult{}, errors.New("ask: answer model not configured")
	}
	var args askArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return ToolResult{}, fmt.Errorf("parse args: %w", err)
	}
	if strings.TrimSpace(args.Query) == "" {
		return ToolResult{}, errors.New("query is required")
	}
	filters, err := buildFilters(args.Source, args.Project, args.Space, "")
	if err != nil {
		return ToolResult{}, err
	}
	finalText, hits, err := d.Answerer.Stream(ctx, answer.Request{
		Query:   args.Query,
		Filters: filters,
	}, func(answer.Event) {
		// MCP tool calls return a single response; we drop intermediate
		// events and deliver the final text + sources in one payload.
	})
	if err != nil {
		return ToolResult{}, err
	}
	return TextResult(map[string]any{
		"answer":  finalText,
		"sources": hits,
	})
}

func (d ToolDeps) listProjectsHandler(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
	values, err := d.distinctProjectOrSpace(ctx, "jira")
	if err != nil {
		return ToolResult{}, err
	}
	return TextResult(map[string]any{"projects": values})
}

func (d ToolDeps) listSpacesHandler(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
	values, err := d.distinctProjectOrSpace(ctx, "confluence")
	if err != nil {
		return ToolResult{}, err
	}
	return TextResult(map[string]any{"spaces": values})
}

type getIssueArgs struct {
	Key string `json:"key"`
}

func (d ToolDeps) getIssueHandler(ctx context.Context, raw json.RawMessage) (ToolResult, error) {
	var args getIssueArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return ToolResult{}, fmt.Errorf("parse args: %w", err)
	}
	key := strings.TrimSpace(args.Key)
	if key == "" {
		return ToolResult{}, errors.New("key is required")
	}
	return d.fetchSource(ctx, "jira", key)
}

type getPageArgs struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

var pageIDFromURL = regexp.MustCompile(`/pages/(\d+)`)

func (d ToolDeps) getPageHandler(ctx context.Context, raw json.RawMessage) (ToolResult, error) {
	var args getPageArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return ToolResult{}, fmt.Errorf("parse args: %w", err)
	}
	pageID := strings.TrimSpace(args.ID)
	if pageID == "" && args.URL != "" {
		u, err := url.Parse(args.URL)
		if err == nil {
			if m := pageIDFromURL.FindStringSubmatch(u.Path); m != nil {
				pageID = m[1]
			}
		}
	}
	if pageID == "" {
		return ToolResult{}, errors.New("id or url with /pages/{id} is required")
	}
	return d.fetchSource(ctx, "confluence", pageID)
}

// ---- helpers ----

type storedSource struct {
	ID             int64          `json:"id"`
	SourceType     string         `json:"source_type"`
	SourceKey      string         `json:"source_key"`
	Title          string         `json:"title"`
	URL            string         `json:"url"`
	ProjectOrSpace string         `json:"project_or_space,omitempty"`
	BodyMarkdown   string         `json:"body_markdown"`
	Extra          map[string]any `json:"extra,omitempty"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

func (d ToolDeps) fetchSource(ctx context.Context, sourceType, sourceKey string) (ToolResult, error) {
	const q = `
SELECT id, source_type, source_key, title, url, project_or_space, body_markdown, extra, updated_at
FROM sources
WHERE source_type = $1 AND source_key = $2`
	row := d.Store.Pool().QueryRow(ctx, q, sourceType, sourceKey)
	var s storedSource
	var pos *string
	var extraRaw []byte
	err := row.Scan(&s.ID, &s.SourceType, &s.SourceKey, &s.Title, &s.URL, &pos, &s.BodyMarkdown, &extraRaw, &s.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ToolResult{}, fmt.Errorf("%s %s not indexed", sourceType, sourceKey)
	}
	if err != nil {
		return ToolResult{}, err
	}
	if pos != nil {
		s.ProjectOrSpace = *pos
	}
	if len(extraRaw) > 0 {
		_ = json.Unmarshal(extraRaw, &s.Extra)
	}
	return TextResult(s)
}

func (d ToolDeps) distinctProjectOrSpace(ctx context.Context, sourceType string) ([]string, error) {
	const q = `
SELECT DISTINCT project_or_space
FROM sources
WHERE source_type = $1 AND project_or_space IS NOT NULL
ORDER BY project_or_space`
	rows, err := d.Store.Pool().Query(ctx, q, sourceType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func buildFilters(sources, projects, spaces []string, updatedAfter string) (retrieve.Filters, error) {
	f := retrieve.Filters{
		Sources:  sources,
		Projects: projects,
		Spaces:   spaces,
	}
	if updatedAfter != "" {
		t, err := time.Parse(time.RFC3339, updatedAfter)
		if err != nil {
			return retrieve.Filters{}, fmt.Errorf("updated_after: %w", err)
		}
		f.UpdatedAfter = t
	}
	return f, nil
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // schemas are static; panic at init beats shipping bad JSON
	}
	return b
}

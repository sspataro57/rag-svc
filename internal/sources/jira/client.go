package jira

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Client talks to Jira Cloud's v3 REST API. Basic auth via
// `email:api_token`, rate-limit-aware (Retry-After + exponential backoff).
type Client struct {
	baseURL    string
	authHeader string
	httpClient *http.Client
	maxRetries int
}

type Options struct {
	BaseURL    string
	Email      string
	Token      string
	HTTPClient *http.Client
	MaxRetries int
}

func New(opts Options) *Client {
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if opts.MaxRetries == 0 {
		opts.MaxRetries = 5
	}
	creds := opts.Email + ":" + opts.Token
	return &Client{
		baseURL:    opts.BaseURL,
		authHeader: "Basic " + base64.StdEncoding.EncodeToString([]byte(creds)),
		httpClient: opts.HTTPClient,
		maxRetries: opts.MaxRetries,
	}
}

// ---- Search (POST /rest/api/3/search/jql) ----

// SearchRequest is the POST body for the new search/jql endpoint. The legacy
// GET /search endpoint was removed by Atlassian in August 2025.
type SearchRequest struct {
	JQL           string   `json:"jql"`
	Fields        []string `json:"fields,omitempty"`
	Expand        []string `json:"expand,omitempty"`
	MaxResults    int      `json:"maxResults,omitempty"`
	NextPageToken string   `json:"nextPageToken,omitempty"`
}

// SearchResponse is the trimmed response. search/jql does not return a `total`
// — pagination is purely token-driven.
type SearchResponse struct {
	Issues        []Issue `json:"issues"`
	NextPageToken string  `json:"nextPageToken,omitempty"`
	IsLast        bool    `json:"isLast,omitempty"`
}

func (c *Client) SearchJQL(ctx context.Context, req SearchRequest) (*SearchResponse, error) {
	var out SearchResponse
	if err := c.doJSON(ctx, http.MethodPost, "/rest/api/3/search/jql", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---- Issue fetch (GET /rest/api/3/issue/{key}) ----

// Issue is the subset of fields we actually normalize. Everything else is kept
// generic so the ADF converter can walk `Description.Content` or
// `Comment.Body` without a fixed schema.
type Issue struct {
	ID     string      `json:"id"`
	Key    string      `json:"key"`
	Self   string      `json:"self"`
	Fields IssueFields `json:"fields"`
}

type IssueFields struct {
	Summary     string         `json:"summary"`
	Description *ADFDocument   `json:"description"`
	Status      *IssueStatus   `json:"status"`
	IssueType   *IssueTypeInfo `json:"issuetype"`
	Project     *IssueProject  `json:"project"`
	Updated     string         `json:"updated"`
	Comment     *CommentBlock  `json:"comment"`
	Labels      []string       `json:"labels,omitempty"`
	Creator     *UserRef       `json:"creator,omitempty"`
	Reporter    *UserRef       `json:"reporter,omitempty"`
	Assignee    *UserRef       `json:"assignee,omitempty"`

	// Relationship fields used by source_links graph expansion. Atlassian
	// Cloud unified epic-link with parent in 2023, so an Epic shows up as a
	// regular parent whose IssueType is "Epic".
	Parent     *ParentRef  `json:"parent,omitempty"`
	Subtasks   []IssueRef  `json:"subtasks,omitempty"`
	IssueLinks []IssueLink `json:"issuelinks,omitempty"`
}

// IssueRef is the minimal embedded-issue shape Jira uses in relationship
// fields (parent, subtasks, issuelinks). The full issue is fetched separately
// when needed.
type IssueRef struct {
	ID     string             `json:"id,omitempty"`
	Key    string             `json:"key"`
	Fields *IssueRefSubFields `json:"fields,omitempty"`
}

// IssueRefSubFields is the trimmed `fields` block embedded inside a parent /
// subtask reference. Only IssueType is needed today — used to tag an Epic
// parent with kind="epic" rather than the generic "parent".
type IssueRefSubFields struct {
	IssueType *IssueTypeInfo `json:"issuetype,omitempty"`
}

// ParentRef is the shape Jira uses for `fields.parent`. Same as IssueRef but
// kept distinct in case Atlassian adds parent-specific fields later.
type ParentRef = IssueRef

// IssueLink represents one entry in `fields.issuelinks`. Each entry carries
// either an outwardIssue or an inwardIssue (never both); the API uses two
// separate entries when both directions are relevant.
type IssueLink struct {
	ID           string         `json:"id,omitempty"`
	Type         *IssueLinkType `json:"type,omitempty"`
	OutwardIssue *IssueRef      `json:"outwardIssue,omitempty"`
	InwardIssue  *IssueRef      `json:"inwardIssue,omitempty"`
}

// IssueLinkType carries the human-readable verb phrases for both directions
// of a link kind. The Treetop instance ships the default Atlassian types:
// Blocks (outward="blocks", inward="is blocked by"), Relates ("relates to"
// both ways), Duplicate ("duplicates" / "is duplicated by"), Cloners
// ("clones" / "is cloned by"). Admins can add custom kinds — we canonicalize
// whatever verb the API returns.
type IssueLinkType struct {
	ID      string `json:"id,omitempty"`
	Name    string `json:"name,omitempty"`
	Inward  string `json:"inward,omitempty"`
	Outward string `json:"outward,omitempty"`
}

type IssueStatus struct {
	Name string `json:"name"`
}
type IssueTypeInfo struct {
	Name string `json:"name"`
}
type IssueProject struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}
type UserRef struct {
	AccountID   string `json:"accountId"`
	DisplayName string `json:"displayName"`
}

type CommentBlock struct {
	Comments   []Comment `json:"comments"`
	Total      int       `json:"total"`
	MaxResults int       `json:"maxResults"`
	StartAt    int       `json:"startAt"`
}

type Comment struct {
	ID      string       `json:"id"`
	Author  *UserRef     `json:"author"`
	Body    *ADFDocument `json:"body"`
	Created string       `json:"created"`
	Updated string       `json:"updated"`
}

// GetIssue fetches one issue with its comments expanded. Used when a search
// result's embedded comment block is truncated (Jira truncates at ~50
// comments even when the field is requested).
func (c *Client) GetIssue(ctx context.Context, key string) (*Issue, error) {
	path := "/rest/api/3/issue/" + url.PathEscape(key) + "?expand=renderedFields,comments"
	var out Issue
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---- HTTP plumbing ----

type apiError struct {
	Status     int
	Body       string
	Retryable  bool
	RetryAfter time.Duration
}

func (e *apiError) Error() string {
	return fmt.Sprintf("jira: http %d: %s", e.Status, e.Body)
}

// IsNotFound reports whether err represents a 404 from the Jira API —
// e.g. an issue we have indexed has since been deleted in Jira, or the
// service account lost access to its project. Callers (notably the
// rebuild-links backfill) treat this as a skippable per-issue condition
// rather than a fatal run-level error.
func IsNotFound(err error) bool {
	var ae *apiError
	if !errors.As(err, &ae) {
		return false
	}
	return ae.Status == http.StatusNotFound
}

// IsTokenError reports whether err is a 4xx pagination-token rejection —
// per CLAUDE.md, the orchestrator should restart from the last watermark
// instead of retrying with the same token.
func IsTokenError(err error) bool {
	var ae *apiError
	if !errors.As(err, &ae) {
		return false
	}
	if ae.Status < 400 || ae.Status >= 500 {
		return false
	}
	// Atlassian returns 400 with a body mentioning "token" when the
	// nextPageToken goes stale. Conservative substring match.
	return bytesContainsFold([]byte(ae.Body), []byte("token"))
}

func (c *Client) doJSON(ctx context.Context, method, path string, reqBody, out any) error {
	var body []byte
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("jira: marshal: %w", err)
		}
		body = b
	}

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			wait := backoff(attempt)
			if ae, ok := asAPIError(lastErr); ok && ae.RetryAfter > 0 {
				wait = ae.RetryAfter
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}

		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", c.authHeader)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		err = handleResponse(resp, out)
		if err == nil {
			return nil
		}
		lastErr = err
		if ae, ok := asAPIError(err); ok && !ae.Retryable {
			return err
		}
	}
	return fmt.Errorf("jira: exhausted retries: %w", lastErr)
}

func handleResponse(resp *http.Response, out any) error {
	defer resp.Body.Close()
	if resp.StatusCode == 429 || resp.StatusCode >= 500 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		ra := parseRetryAfter(resp.Header.Get("Retry-After"))
		return &apiError{Status: resp.StatusCode, Body: string(body), Retryable: true, RetryAfter: ra}
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return &apiError{Status: resp.StatusCode, Body: string(body)}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func asAPIError(err error) (*apiError, bool) {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}

func parseRetryAfter(h string) time.Duration {
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// backoff: base 1s, max 60s, jitter 0.3 — matches the CLAUDE.md spec.
func backoff(attempt int) time.Duration {
	base := time.Second
	d := base << (attempt - 1)
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	jitter := time.Duration(rand.Float64() * float64(d) * 0.3)
	return d + jitter
}

func bytesContainsFold(s, sub []byte) bool {
	if len(sub) == 0 {
		return true
	}
	// Simple case-insensitive ASCII contains without pulling in bytes.EqualFold
	// on substrings.
	for i := 0; i+len(sub) <= len(s); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			a := s[i+j]
			b := sub[j]
			if 'A' <= a && a <= 'Z' {
				a += 'a' - 'A'
			}
			if 'A' <= b && b <= 'Z' {
				b += 'a' - 'A'
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

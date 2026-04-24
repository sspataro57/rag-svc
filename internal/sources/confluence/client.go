// Package confluence contains the v2 API client, storage-format → markdown
// converter, and normalization logic for Confluence Cloud pages.
//
// Endpoints used:
//
//	GET /api/v2/spaces?keys=...     — resolve space keys to numeric IDs.
//	GET /api/v2/spaces/{id}/pages   — cursor-paginated page list.
//	GET /api/v2/pages/{id}          — fetch one page with storage-format body.
//	GET /api/v2/pages/{id}/ancestors — ancestor chain (IDs only).
//	GET /api/v2/pages?id=A&id=B     — bulk title resolution (for breadcrumbs).
//
// Basic auth reuses the same email+API-token pair as the Jira service
// account (CLAUDE.md: "Same token works for both.").
package confluence

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
	"strings"
	"time"
)

type Client struct {
	baseURL    string // includes /wiki (e.g., https://treetopllc.jira.com/wiki)
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
		baseURL:    strings.TrimRight(opts.BaseURL, "/"),
		authHeader: "Basic " + base64.StdEncoding.EncodeToString([]byte(creds)),
		httpClient: opts.HTTPClient,
		maxRetries: opts.MaxRetries,
	}
}

// ---- Spaces ----

type Space struct {
	ID     string `json:"id"`
	Key    string `json:"key"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Status string `json:"status"`
}

type spaceSearchResp struct {
	Results []Space `json:"results"`
}

// ResolveSpaces returns a map of space key → space info for the given keys.
// Keys not found are omitted silently so the caller can decide how loud to
// be about that.
func (c *Client) ResolveSpaces(ctx context.Context, keys []string) (map[string]Space, error) {
	if len(keys) == 0 {
		return map[string]Space{}, nil
	}
	// The v2 API supports ?keys=A,B,C — passing all at once is cheaper than
	// iterating.
	q := url.Values{}
	q.Set("limit", "100")
	for _, k := range keys {
		q.Add("keys", k)
	}
	var out spaceSearchResp
	if err := c.doJSON(ctx, http.MethodGet, "/api/v2/spaces?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	m := make(map[string]Space, len(out.Results))
	for _, s := range out.Results {
		m[s.Key] = s
	}
	return m, nil
}

// ---- Page list (per space, cursor-paginated, sort -modified-date) ----

type PageListItem struct {
	ID       string      `json:"id"`
	Title    string      `json:"title"`
	ParentID string      `json:"parentId,omitempty"`
	SpaceID  string      `json:"spaceId,omitempty"`
	Status   string      `json:"status,omitempty"`
	Version  PageVersion `json:"version"`
}

type PageVersion struct {
	Number    int       `json:"number"`
	CreatedAt time.Time `json:"createdAt"`
}

type pageListResp struct {
	Results []PageListItem `json:"results"`
	Links   struct {
		Next string `json:"next,omitempty"`
	} `json:"_links"`
}

// ListPagesInSpace yields pages in -modified-date order; the caller stops
// early when a page's Version.CreatedAt precedes the last watermark
// (CLAUDE.md: "Filter client-side by modified-date > last_sync_watermark").
// nextPath is the _links.next URL path from the previous response, or ""
// to start from the first page.
func (c *Client) ListPagesInSpace(ctx context.Context, spaceID, nextPath string) (*pageListResp, error) {
	path := nextPath
	if path == "" {
		path = fmt.Sprintf("/api/v2/spaces/%s/pages?limit=100&sort=-modified-date", url.PathEscape(spaceID))
	}
	var resp pageListResp
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ---- Single-page fetch (storage-format body) ----

type PageBody struct {
	Storage struct {
		Value          string `json:"value"`
		Representation string `json:"representation"`
	} `json:"storage"`
}

type Page struct {
	ID       string      `json:"id"`
	Title    string      `json:"title"`
	ParentID string      `json:"parentId,omitempty"`
	SpaceID  string      `json:"spaceId,omitempty"`
	Status   string      `json:"status,omitempty"`
	AuthorID string      `json:"authorId,omitempty"`
	Body     PageBody    `json:"body"`
	Version  PageVersion `json:"version"`
}

// GetPage fetches one page with body-format=storage.
func (c *Client) GetPage(ctx context.Context, pageID string) (*Page, error) {
	path := fmt.Sprintf("/api/v2/pages/%s?body-format=storage&include-version=true", url.PathEscape(pageID))
	var p Page
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// ---- Ancestors (IDs only) + bulk title resolution ----

type ancestorItem struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}
type ancestorsResp struct {
	Results []ancestorItem `json:"results"`
}

// GetAncestors returns the ancestor page IDs in root-first order. The v2
// API doesn't include titles in this response, so GetPagesByIDs is used to
// enrich.
func (c *Client) GetAncestors(ctx context.Context, pageID string) ([]string, error) {
	path := fmt.Sprintf("/api/v2/pages/%s/ancestors", url.PathEscape(pageID))
	var r ancestorsResp
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &r); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(r.Results))
	for _, a := range r.Results {
		if a.Type == "page" {
			out = append(out, a.ID)
		}
	}
	return out, nil
}

type pagesBulkResp struct {
	Results []Page `json:"results"`
}

// GetPagesByIDs bulk-fetches page metadata (no body) for title resolution.
// The v2 API supports up to 250 IDs per call; we batch internally.
func (c *Client) GetPagesByIDs(ctx context.Context, ids []string) ([]Page, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	const batchSize = 250
	out := make([]Page, 0, len(ids))
	for start := 0; start < len(ids); start += batchSize {
		end := start + batchSize
		if end > len(ids) {
			end = len(ids)
		}
		q := url.Values{}
		q.Set("limit", strconv.Itoa(batchSize))
		q.Set("body-format", "none")
		for _, id := range ids[start:end] {
			q.Add("id", id)
		}
		var r pagesBulkResp
		if err := c.doJSON(ctx, http.MethodGet, "/api/v2/pages?"+q.Encode(), nil, &r); err != nil {
			return nil, err
		}
		out = append(out, r.Results...)
	}
	return out, nil
}

// ---- HTTP plumbing (mirrors the Jira client) ----

type apiError struct {
	Status     int
	Body       string
	Retryable  bool
	RetryAfter time.Duration
}

func (e *apiError) Error() string {
	return fmt.Sprintf("confluence: http %d: %s", e.Status, e.Body)
}

func (c *Client) doJSON(ctx context.Context, method, path string, reqBody, out any) error {
	var body []byte
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("confluence: marshal: %w", err)
		}
		body = b
	}

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			wait := backoff(attempt)
			var ae *apiError
			if errors.As(lastErr, &ae) && ae.RetryAfter > 0 {
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
		var ae *apiError
		if errors.As(err, &ae) && !ae.Retryable {
			return err
		}
	}
	return fmt.Errorf("confluence: exhausted retries: %w", lastErr)
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

// backoff matches the Jira client's policy: base 1s, max 60s, 0.3 jitter.
func backoff(attempt int) time.Duration {
	base := time.Second
	d := base << (attempt - 1)
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	jitter := time.Duration(rand.Float64() * float64(d) * 0.3)
	return d + jitter
}

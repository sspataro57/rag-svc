package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/treetop/rag-svc/internal/ingest"
	"github.com/treetop/rag-svc/internal/sources/jira"
)

// Cap a single ingest request so a runaway producer can't OOM the server
// or blow our embeddings budget in one shot. Feeders should batch.
const maxIngestBatchSize = 200

// ingestJiraRequest is the wire shape for POST /ingest/jira. It's a thin
// JSON projection of jira.NormalizedIssue so external feeders (e.g. the
// reconstructed-from-email script) can produce it without depending on
// the Atlassian API client. Fields the live-Jira normalizer would derive
// from the API response (Project from Key prefix, URL from BaseURL) are
// the producer's responsibility here.
type ingestJiraRequest struct {
	Issues []ingestJiraIssue `json:"issues"`
}

type ingestJiraIssue struct {
	Key         string              `json:"key"`
	Project     string              `json:"project"`
	Title       string              `json:"title"`
	Description string              `json:"description,omitempty"`
	Status      string              `json:"status,omitempty"`
	IssueType   string              `json:"issue_type,omitempty"`
	URL         string              `json:"url,omitempty"`
	UpdatedAt   time.Time           `json:"updated_at"`
	Comments    []ingestJiraComment `json:"comments,omitempty"`
	Extra       map[string]any      `json:"extra,omitempty"`
}

type ingestJiraComment struct {
	Author    string    `json:"author,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	Body      string    `json:"body"`
}

type ingestJiraResponse struct {
	Upserted int               `json:"upserted"`
	Chunks   int               `json:"chunks"`
	Failed   int               `json:"failed"`
	Errors   []ingestJiraError `json:"errors,omitempty"`
}

type ingestJiraError struct {
	Key   string `json:"key"`
	Error string `json:"error"`
}

// handleIngestJira accepts a batch of normalized Jira-shaped issues and
// runs them through the same chunk → embed → upsert pipeline as the live
// sync. Auth: bearer token (mounted under BearerMiddleware in server.go).
//
// Failure model: each issue is independent — one bad row shouldn't fail
// the whole batch. Per-issue errors are collected and returned in the
// response body alongside the success counts. The HTTP status is 200 if
// any issue succeeded, 400 if the request itself was malformed, and 502
// only when all issues failed (the caller can treat it as a hard error).
func (s *Server) handleIngestJira(w http.ResponseWriter, r *http.Request) {
	if s.ingest == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "ingest not configured"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20)) // 32MiB cap; well above any realistic batch
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "read body: " + err.Error()})
		return
	}
	var req ingestJiraRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid JSON: " + err.Error()})
		return
	}
	if len(req.Issues) == 0 {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "issues is empty"})
		return
	}
	if len(req.Issues) > maxIngestBatchSize {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: fmt.Sprintf("batch too large: %d > %d", len(req.Issues), maxIngestBatchSize)})
		return
	}

	resp := ingestJiraResponse{}
	for _, in := range req.Issues {
		norm, err := buildNormalizedIssue(in)
		if err != nil {
			resp.Failed++
			resp.Errors = append(resp.Errors, ingestJiraError{Key: in.Key, Error: err.Error()})
			continue
		}
		stats, err := ingest.IngestJiraNormalized(r.Context(), *s.ingest, s.ingestOpts, norm)
		if err != nil {
			resp.Failed++
			resp.Errors = append(resp.Errors, ingestJiraError{Key: norm.Key, Error: err.Error()})
			continue
		}
		resp.Upserted++
		resp.Chunks += stats.Chunks
	}

	status := http.StatusOK
	if resp.Upserted == 0 {
		status = http.StatusBadGateway
	}
	writeJSON(w, status, resp)
}

// buildNormalizedIssue converts a wire-shape ingestJiraIssue into the
// jira.NormalizedIssue the chunker expects. Validation is deliberately
// minimal — the producer is trusted (bearer-authed), and surface-level
// shape checks are enough to keep bad rows out of the store.
func buildNormalizedIssue(in ingestJiraIssue) (*jira.NormalizedIssue, error) {
	key := strings.TrimSpace(in.Key)
	if key == "" {
		return nil, errors.New("key is required")
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return nil, errors.New("title is required")
	}
	if in.UpdatedAt.IsZero() {
		return nil, errors.New("updated_at is required")
	}
	project := strings.TrimSpace(in.Project)
	if project == "" {
		// Fall back to the prefix of the issue key — Jira convention is
		// PROJECT-NUMBER, mirrored by the live normalizer in
		// internal/sources/jira/normalize.go.
		if i := strings.LastIndex(key, "-"); i > 0 {
			project = key[:i]
		}
	}

	comments := make([]jira.NormalizedComment, 0, len(in.Comments))
	for _, c := range in.Comments {
		comments = append(comments, jira.NormalizedComment{
			Author:    c.Author,
			CreatedAt: c.CreatedAt,
			Body:      c.Body,
		})
	}

	extra := in.Extra
	if extra == nil {
		extra = map[string]any{}
	}
	// Mirror live normalizer's audit fields so /search rendering and
	// retrieval filters work uniformly across ingest paths.
	if _, ok := extra["status"]; !ok && in.Status != "" {
		extra["status"] = in.Status
	}
	if _, ok := extra["issue_type"]; !ok && in.IssueType != "" {
		extra["issue_type"] = in.IssueType
	}
	if _, ok := extra["comment_count"]; !ok {
		extra["comment_count"] = len(comments)
	}

	return &jira.NormalizedIssue{
		Key:         key,
		Title:       title,
		Description: in.Description,
		Comments:    comments,
		Status:      in.Status,
		IssueType:   in.IssueType,
		Project:     project,
		URL:         in.URL,
		UpdatedAt:   in.UpdatedAt,
		Extra:       extra,
	}, nil
}

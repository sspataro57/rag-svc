package jira

import (
	"fmt"
	"strings"
	"time"
)

// NormalizedIssue is the storage-shaped view of a Jira issue, produced by
// Normalize. Fields map directly onto the sources table plus the separate
// Comments slice the chunker can ingest alongside the body.
type NormalizedIssue struct {
	Key         string
	Title       string
	Description string // ADF rendered to markdown
	Comments    []NormalizedComment
	Status      string
	IssueType   string
	Project     string
	URL         string
	UpdatedAt   time.Time
	Extra       map[string]any
}

type NormalizedComment struct {
	ID        string
	Author    string
	CreatedAt time.Time
	Body      string // ADF rendered to markdown
}

// BodyMarkdown returns the fully-assembled markdown stored in sources.body_markdown:
// description first, then each comment under a `## Comment by {author} on {date}`
// header. This is the text the chunker splits; keeping it in one field means
// `rag-svc reindex` can rebuild chunks without re-fetching Jira.
func (n *NormalizedIssue) BodyMarkdown() string {
	var b strings.Builder
	if n.Description != "" {
		b.WriteString(strings.TrimRight(n.Description, "\n"))
		b.WriteString("\n")
	}
	for _, c := range n.Comments {
		fmt.Fprintf(&b, "\n\n## Comment by %s on %s\n\n", c.Author, c.CreatedAt.UTC().Format("2006-01-02"))
		b.WriteString(strings.TrimRight(c.Body, "\n"))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// Normalize converts a raw Issue from the Jira API into a NormalizedIssue.
// issuesBaseURL should be the Jira base URL (e.g., https://treetopllc.jira.com)
// — the issue browse URL is constructed as `${baseURL}/browse/{Key}`.
func Normalize(issue *Issue, issuesBaseURL string) (*NormalizedIssue, error) {
	if issue == nil {
		return nil, fmt.Errorf("normalize: nil issue")
	}
	updated, err := parseJiraTime(issue.Fields.Updated)
	if err != nil {
		return nil, fmt.Errorf("normalize %s: updated: %w", issue.Key, err)
	}

	description := ADFDocumentToMarkdown(issue.Fields.Description)
	description = strings.TrimRight(description, "\n")

	var comments []NormalizedComment
	if issue.Fields.Comment != nil {
		for _, c := range issue.Fields.Comment.Comments {
			created, err := parseJiraTime(c.Created)
			if err != nil {
				// Skip malformed timestamp rather than failing the whole
				// issue — comments with bad metadata are still more
				// useful than not having them.
				created = time.Time{}
			}
			author := "unknown"
			if c.Author != nil && c.Author.DisplayName != "" {
				author = c.Author.DisplayName
			}
			body := strings.TrimRight(ADFDocumentToMarkdown(c.Body), "\n")
			comments = append(comments, NormalizedComment{
				ID:        c.ID,
				Author:    author,
				CreatedAt: created,
				Body:      body,
			})
		}
	}

	status := ""
	if issue.Fields.Status != nil {
		status = issue.Fields.Status.Name
	}
	itype := ""
	if issue.Fields.IssueType != nil {
		itype = issue.Fields.IssueType.Name
	}
	project := ""
	if issue.Fields.Project != nil {
		project = issue.Fields.Project.Key
	}
	if project == "" {
		// Fall back to the prefix of the issue key — Jira always uses
		// PROJECTKEY-NUMBER so this is safe.
		if i := strings.LastIndex(issue.Key, "-"); i > 0 {
			project = issue.Key[:i]
		}
	}

	base := strings.TrimRight(issuesBaseURL, "/")
	url := base + "/browse/" + issue.Key

	extra := map[string]any{
		"status":        status,
		"issue_type":    itype,
		"comment_count": len(comments),
	}
	if len(issue.Fields.Labels) > 0 {
		extra["labels"] = issue.Fields.Labels
	}
	if issue.Fields.Assignee != nil {
		extra["assignee"] = issue.Fields.Assignee.DisplayName
	}

	return &NormalizedIssue{
		Key:         issue.Key,
		Title:       issue.Fields.Summary,
		Description: description,
		Comments:    comments,
		Status:      status,
		IssueType:   itype,
		Project:     project,
		URL:         url,
		UpdatedAt:   updated,
		Extra:       extra,
	}, nil
}

// Jira returns timestamps in ISO 8601 with a numeric offset, e.g.
// "2026-01-01T09:15:22.123+0000". time.RFC3339 expects a colon in the offset
// so we try both forms.
func parseJiraTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf("empty timestamp")
	}
	layouts := []string{
		"2006-01-02T15:04:05.000-0700",
		"2006-01-02T15:04:05-0700",
		time.RFC3339Nano,
		time.RFC3339,
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized timestamp %q", s)
}

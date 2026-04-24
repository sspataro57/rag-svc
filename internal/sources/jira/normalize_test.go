package jira

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestNormalize_BasicIssue(t *testing.T) {
	iss := &Issue{
		Key: "PLAT-42",
		Fields: IssueFields{
			Summary:   "Credential rotation runbook",
			Updated:   "2026-01-15T09:30:00.000+0000",
			Status:    &IssueStatus{Name: "Done"},
			IssueType: &IssueTypeInfo{Name: "Task"},
			Project:   &IssueProject{Key: "PLAT", Name: "Platform"},
			Labels:    []string{"ops", "runbook"},
			Description: mustADF(t, `{
				"type":"doc","version":1,
				"content":[{"type":"paragraph","content":[{"type":"text","text":"Body here."}]}]
			}`),
			Comment: &CommentBlock{Comments: []Comment{
				{
					ID:      "c1",
					Author:  &UserRef{DisplayName: "Alice"},
					Created: "2026-01-16T10:00:00.000+0000",
					Body:    mustADF(t, `{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"First comment."}]}]}`),
				},
				{
					ID:      "c2",
					Author:  &UserRef{DisplayName: "Bob"},
					Created: "2026-01-17T11:00:00.000+0000",
					Body:    mustADF(t, `{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"Second comment."}]}]}`),
				},
			}, Total: 2},
		},
	}

	n, err := Normalize(iss, "https://treetopllc.jira.com")
	if err != nil {
		t.Fatal(err)
	}
	if n.Key != "PLAT-42" || n.Title != "Credential rotation runbook" {
		t.Errorf("unexpected key/title: %+v", n)
	}
	if n.URL != "https://treetopllc.jira.com/browse/PLAT-42" {
		t.Errorf("url: %s", n.URL)
	}
	if n.Project != "PLAT" {
		t.Errorf("project: %s", n.Project)
	}
	want := time.Date(2026, 1, 15, 9, 30, 0, 0, time.UTC)
	if !n.UpdatedAt.Equal(want) {
		t.Errorf("updated: got %s want %s", n.UpdatedAt, want)
	}
	if got := n.Extra["comment_count"]; got != 2 {
		t.Errorf("comment_count: %v", got)
	}

	body := n.BodyMarkdown()
	if !strings.Contains(body, "Body here.") {
		t.Error("body missing description")
	}
	if !strings.Contains(body, "## Comment by Alice on 2026-01-16") {
		t.Errorf("missing Alice header in:\n%s", body)
	}
	if !strings.Contains(body, "First comment.") {
		t.Error("body missing first comment text")
	}
	if !strings.Contains(body, "## Comment by Bob on 2026-01-17") {
		t.Error("body missing Bob header")
	}
}

func TestNormalize_MissingProjectFallsBackToKeyPrefix(t *testing.T) {
	iss := &Issue{
		Key:    "ABC-9",
		Fields: IssueFields{Summary: "x", Updated: "2026-01-01T00:00:00.000+0000"},
	}
	n, err := Normalize(iss, "https://x")
	if err != nil {
		t.Fatal(err)
	}
	if n.Project != "ABC" {
		t.Errorf("expected fallback ABC, got %q", n.Project)
	}
}

func TestNormalize_EmptyDescriptionOK(t *testing.T) {
	iss := &Issue{
		Key: "X-1",
		Fields: IssueFields{
			Summary: "No body",
			Project: &IssueProject{Key: "X"},
			Updated: "2026-01-01T00:00:00.000+0000",
		},
	}
	n, err := Normalize(iss, "https://x")
	if err != nil {
		t.Fatal(err)
	}
	if n.Description != "" {
		t.Errorf("want empty description, got %q", n.Description)
	}
	if n.BodyMarkdown() != "\n" {
		t.Errorf("empty body should render to just a newline, got %q", n.BodyMarkdown())
	}
}

func mustADF(t *testing.T, s string) *ADFDocument {
	t.Helper()
	var d ADFDocument
	if err := json.Unmarshal([]byte(s), &d); err != nil {
		t.Fatal(err)
	}
	return &d
}

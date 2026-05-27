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

func TestNormalize_ExtractsLinks(t *testing.T) {
	iss := &Issue{
		Key: "API-4266",
		Fields: IssueFields{
			Summary: "SmartFill: accept PDF uploads",
			Updated: "2026-05-22T20:53:42.000+0000",
			Parent: &ParentRef{
				Key:    "WEB-10275",
				Fields: &IssueRefSubFields{IssueType: &IssueTypeInfo{Name: "Epic"}},
			},
			Subtasks: []IssueRef{
				{Key: "API-4266a"},
				{Key: "API-4266b"},
			},
			IssueLinks: []IssueLink{
				{
					Type:         &IssueLinkType{Name: "Blocks", Outward: "blocks", Inward: "is blocked by"},
					OutwardIssue: &IssueRef{Key: "API-4270"},
				},
				{
					Type:        &IssueLinkType{Name: "Blocks", Outward: "blocks", Inward: "is blocked by"},
					InwardIssue: &IssueRef{Key: "API-4268"},
				},
				{
					Type:         &IssueLinkType{Name: "Relates", Outward: "relates to", Inward: "relates to"},
					OutwardIssue: &IssueRef{Key: "WEB-10300"},
				},
				// Duplicate edge — should be de-duped:
				{
					Type:         &IssueLinkType{Name: "Blocks", Outward: "blocks", Inward: "is blocked by"},
					OutwardIssue: &IssueRef{Key: "API-4270"},
				},
				// Self-loop — should be dropped:
				{
					Type:         &IssueLinkType{Name: "Relates", Outward: "relates to", Inward: "relates to"},
					OutwardIssue: &IssueRef{Key: "API-4266"},
				},
				// Empty-keyed link — should be dropped:
				{
					Type:         &IssueLinkType{Name: "Relates", Outward: "relates to", Inward: "relates to"},
					OutwardIssue: &IssueRef{Key: ""},
				},
				// Typeless link — should be dropped:
				{
					OutwardIssue: &IssueRef{Key: "API-4271"},
				},
			},
		},
	}

	n, err := Normalize(iss, "https://treetopllc.jira.com")
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"WEB-10275": "epic",
		"API-4266a": "subtask",
		"API-4266b": "subtask",
		"API-4270":  "blocks",
		"API-4268":  "is_blocked_by",
		"WEB-10300": "relates_to",
	}
	got := map[string]string{}
	for _, l := range n.Links {
		if l.TargetType != "jira" {
			t.Errorf("expected target_type=jira, got %q for %s", l.TargetType, l.TargetKey)
		}
		got[l.TargetKey] = l.Kind
	}
	if len(got) != len(want) {
		t.Errorf("link count: got %d want %d (got=%v)", len(got), len(want), got)
	}
	for k, v := range want {
		if gv := got[k]; gv != v {
			t.Errorf("link for %s: got %q want %q", k, gv, v)
		}
	}
}

func TestNormalize_NoLinks(t *testing.T) {
	iss := &Issue{
		Key:    "X-1",
		Fields: IssueFields{Summary: "x", Updated: "2026-01-01T00:00:00.000+0000"},
	}
	n, err := Normalize(iss, "https://x")
	if err != nil {
		t.Fatal(err)
	}
	if len(n.Links) != 0 {
		t.Errorf("expected no links, got %d: %+v", len(n.Links), n.Links)
	}
}

func TestNormalize_ParentNonEpicTaggedAsParent(t *testing.T) {
	iss := &Issue{
		Key: "X-2",
		Fields: IssueFields{
			Summary: "x",
			Updated: "2026-01-01T00:00:00.000+0000",
			Parent: &ParentRef{
				Key:    "X-1",
				Fields: &IssueRefSubFields{IssueType: &IssueTypeInfo{Name: "Story"}},
			},
		},
	}
	n, err := Normalize(iss, "https://x")
	if err != nil {
		t.Fatal(err)
	}
	if len(n.Links) != 1 || n.Links[0].Kind != "parent" {
		t.Errorf("expected one 'parent' link, got %+v", n.Links)
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

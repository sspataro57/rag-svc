package http

import (
	"strings"
	"testing"
	"time"
)

func TestBuildNormalizedIssue_MissingKeyRejected(t *testing.T) {
	_, err := buildNormalizedIssue(ingestJiraIssue{
		Title:     "x",
		UpdatedAt: time.Now(),
	})
	if err == nil || !strings.Contains(err.Error(), "key") {
		t.Fatalf("want key error, got %v", err)
	}
}

func TestBuildNormalizedIssue_MissingTitleRejected(t *testing.T) {
	_, err := buildNormalizedIssue(ingestJiraIssue{
		Key:       "TES-1",
		UpdatedAt: time.Now(),
	})
	if err == nil || !strings.Contains(err.Error(), "title") {
		t.Fatalf("want title error, got %v", err)
	}
}

func TestBuildNormalizedIssue_MissingUpdatedAtRejected(t *testing.T) {
	_, err := buildNormalizedIssue(ingestJiraIssue{
		Key:   "TES-1",
		Title: "x",
	})
	if err == nil || !strings.Contains(err.Error(), "updated_at") {
		t.Fatalf("want updated_at error, got %v", err)
	}
}

func TestBuildNormalizedIssue_ProjectDerivedFromKeyPrefix(t *testing.T) {
	got, err := buildNormalizedIssue(ingestJiraIssue{
		Key:       "TES-482",
		Title:     "Discount support",
		UpdatedAt: time.Date(2019, 8, 17, 7, 24, 12, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Project != "TES" {
		t.Errorf("project: got %q want TES", got.Project)
	}
}

func TestBuildNormalizedIssue_ExplicitProjectWins(t *testing.T) {
	got, err := buildNormalizedIssue(ingestJiraIssue{
		Key:       "TES-1",
		Project:   "OVERRIDE",
		Title:     "x",
		UpdatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Project != "OVERRIDE" {
		t.Errorf("project override ignored: got %q", got.Project)
	}
}

func TestBuildNormalizedIssue_AutoExtra(t *testing.T) {
	got, err := buildNormalizedIssue(ingestJiraIssue{
		Key:       "TES-1",
		Title:     "x",
		Status:    "Done",
		IssueType: "Task",
		UpdatedAt: time.Now(),
		Comments: []ingestJiraComment{
			{Body: "a"}, {Body: "b"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Extra["status"] != "Done" {
		t.Errorf("extra.status: got %v", got.Extra["status"])
	}
	if got.Extra["issue_type"] != "Task" {
		t.Errorf("extra.issue_type: got %v", got.Extra["issue_type"])
	}
	if got.Extra["comment_count"] != 2 {
		t.Errorf("extra.comment_count: got %v", got.Extra["comment_count"])
	}
}

func TestBuildNormalizedIssue_CallerExtraPreserved(t *testing.T) {
	got, err := buildNormalizedIssue(ingestJiraIssue{
		Key:       "TES-1",
		Title:     "x",
		Status:    "Done",
		UpdatedAt: time.Now(),
		Extra:     map[string]any{"reconstructed": true, "email_count": 8},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Extra["reconstructed"] != true {
		t.Errorf("caller extra.reconstructed dropped: %v", got.Extra)
	}
	if got.Extra["email_count"] != 8 {
		t.Errorf("caller extra.email_count dropped: %v", got.Extra)
	}
	// Auto fields layered on top of caller-provided extra.
	if got.Extra["status"] != "Done" {
		t.Errorf("auto status missing: %v", got.Extra)
	}
}

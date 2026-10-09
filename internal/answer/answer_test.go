package answer

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/treetop/rag-svc/internal/retrieve"
)

func TestBuildPrompt_FormatsCitableContext(t *testing.T) {
	hits := []retrieve.Hit{
		{ID: "jira:PLAT-1", Title: "Rotate creds", URL: "https://x/browse/PLAT-1", Snippet: "<mark>rotate</mark> the token"},
		{ID: "confluence:123", Title: "Runbook", URL: "https://x/wiki/pages/123", Snippet: "Step one"},
	}
	msgs := BuildPrompt("how do I rotate credentials?", hits, 0)
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %d", len(msgs))
	}
	if msgs[0].Role != "system" {
		t.Errorf("first message role: got %q want system", msgs[0].Role)
	}
	if !strings.Contains(msgs[0].Content, "[^N]") {
		t.Errorf("system prompt missing citation instruction")
	}
	if msgs[1].Role != "user" {
		t.Errorf("second role: got %q want user", msgs[1].Role)
	}
	// Context is numbered starting at 1 and mark tags are stripped.
	if !strings.Contains(msgs[1].Content, "[1] Rotate creds — https://x/browse/PLAT-1") {
		t.Errorf("missing numbered citation #1:\n%s", msgs[1].Content)
	}
	if !strings.Contains(msgs[1].Content, "[2] Runbook") {
		t.Errorf("missing numbered citation #2")
	}
	if strings.Contains(msgs[1].Content, "<mark>") {
		t.Errorf("<mark> tags leaked into prompt")
	}
	if !strings.Contains(msgs[1].Content, "Question: how do I rotate credentials?") {
		t.Errorf("question missing")
	}
}

func TestBuildPrompt_RespectsCharBudget(t *testing.T) {
	big := strings.Repeat("x", 5000)
	hits := []retrieve.Hit{
		{ID: "a", Title: "A", URL: "u1", Snippet: big},
		{ID: "b", Title: "B", URL: "u2", Snippet: big},
		{ID: "c", Title: "C", URL: "u3", Snippet: big},
	}
	msgs := BuildPrompt("q", hits, 5500)
	// Only the first hit should have fit.
	if !strings.Contains(msgs[1].Content, "[1] A") {
		t.Errorf("expected hit 1 in context")
	}
	if strings.Contains(msgs[1].Content, "[2] B") {
		t.Errorf("hit 2 unexpectedly included (over budget)")
	}
}

func TestBuildPrompt_IncludesJiraMetadata(t *testing.T) {
	hits := []retrieve.Hit{
		{
			ID: "jira:WEB-10500", Title: "Step does not save", URL: "https://x/browse/WEB-10500",
			Snippet:   "returns 400",
			UpdatedAt: time.Date(2026, 10, 9, 15, 9, 28, 0, time.UTC),
			Extra:     map[string]any{"status": "In Progress", "issue_type": "Bug", "assignee": "Dana Okafor", "comment_count": float64(3)},
		},
		{ID: "confluence:123", Title: "Runbook", URL: "https://x/wiki/pages/123", Snippet: "Step one"},
	}
	got := BuildPrompt("what is the status of WEB-10500?", hits, 0)[1].Content
	want := "[1] Step does not save — https://x/browse/WEB-10500\n" +
		"Status: In Progress | Type: Bug | Assignee: Dana Okafor | Updated: 2026-10-09\n" +
		"returns 400\n\n" +
		"[2] Runbook — https://x/wiki/pages/123\nStep one\n\n"
	if !strings.Contains(got, want) {
		t.Errorf("context mismatch:\n%s", got)
	}
}

func TestBuildPrompt_NamedIssueUsesBody(t *testing.T) {
	body := "Full description of the bug.\n\n## Comment by Dana on 2026-10-08\n\nReproduced on staging."
	hits := []retrieve.Hit{
		{ID: "jira:WEB-10500", Title: "Step does not save", URL: "u1", Snippet: "short snippet", Body: body},
		{ID: "jira:WEB-928", Title: "Other", URL: "u2", Snippet: "other snippet"},
	}
	got := BuildPrompt("q", hits, 0)[1].Content
	if !strings.Contains(got, body) {
		t.Errorf("body missing from context:\n%s", got)
	}
	if strings.Contains(got, "short snippet") {
		t.Errorf("snippet should be replaced by the body")
	}
	if !strings.Contains(got, "other snippet") {
		t.Errorf("hit without a body should keep its snippet")
	}
}

func TestBuildPrompt_BodyIsCappedAndFallsBackToSnippet(t *testing.T) {
	long := strings.Repeat("é", 5000) // 10,000 bytes, cut must land on a rune boundary
	hits := []retrieve.Hit{
		{ID: "a", Title: "A", URL: "u1", Snippet: "snip-a", Body: long},
		{ID: "b", Title: "B", URL: "u2", Snippet: "snip-b", Body: long},
	}
	got := BuildPrompt("q", hits, 4500)[1].Content
	if !utf8.ValidString(got) {
		t.Fatal("truncation split a rune")
	}
	if n := strings.Count(got, "é"); n != maxBodyChars/2 {
		t.Errorf("want %d body runes from the first hit only, got %d", maxBodyChars/2, n)
	}
	if !strings.Contains(got, "[truncated]") {
		t.Errorf("cut body should be marked")
	}
	// The second body no longer fits; its snippet does.
	if !strings.Contains(got, "[2] B — u2\nsnip-b") {
		t.Errorf("second hit should fall back to its snippet:\n%s", got)
	}
}

func TestBuildPrompt_EmptyHits(t *testing.T) {
	msgs := BuildPrompt("orphan question", nil, 0)
	if !strings.Contains(msgs[1].Content, "Question: orphan question") {
		t.Errorf("question missing: %q", msgs[1].Content)
	}
}

func TestHitsToCitations(t *testing.T) {
	h := []retrieve.Hit{
		{ID: "jira:X", Source: "jira", Title: "T", URL: "U", Score: 0.9, Snippet: "s"},
	}
	c := HitsToCitations(h)
	if len(c) != 1 || c[0].ID != "jira:X" || c[0].Score != 0.9 {
		t.Errorf("unexpected conversion: %+v", c)
	}
}

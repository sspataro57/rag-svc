package answer

import (
	"strings"
	"testing"

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

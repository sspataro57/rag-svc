package chunk

import (
	"strings"
	"testing"
	"time"

	"github.com/treetop/rag-svc/internal/sources/jira"
)

func TestJira_SmallIssueSingleChunk(t *testing.T) {
	iss := &jira.NormalizedIssue{
		Key:         "PLAT-1",
		Title:       "Short",
		Description: "A tiny body.",
	}
	chunks, err := Jira(iss, JiraOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 {
		t.Fatalf("want 1 chunk, got %d", len(chunks))
	}
	c := chunks[0]
	if c.Kind != KindBody {
		t.Errorf("kind: got %s want body", c.Kind)
	}
	if !strings.Contains(c.Content, "PLAT-1") || !strings.Contains(c.Content, "A tiny body.") {
		t.Errorf("content missing expected text:\n%s", c.Content)
	}
	if c.TokenCount == 0 {
		t.Error("token count should be > 0")
	}
	if c.Index != 0 {
		t.Errorf("index: got %d want 0", c.Index)
	}
}

func TestJira_OverflowCommentsProduceExtraChunks(t *testing.T) {
	// Body is small enough to fit under the budget; comments overflow so
	// they produce additional chunks with chunk_kind = "comment".
	iss := &jira.NormalizedIssue{
		Key:         "PLAT-2",
		Title:       "Hello",
		Description: "A short body.",
		Comments: []jira.NormalizedComment{
			{Author: "Alice", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Body: strings.Repeat("alice-comment ", 30)},
			{Author: "Bob", CreatedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), Body: strings.Repeat("bob-comment ", 30)},
		},
	}
	chunks, err := Jira(iss, JiraOptions{MaxTokens: 60, OverlapTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 2 {
		t.Fatalf("expected overflow chunks, got %d", len(chunks))
	}
	if chunks[0].Kind != KindBody {
		t.Errorf("first chunk kind: got %s want body", chunks[0].Kind)
	}
	for i, c := range chunks[1:] {
		if c.Kind != KindComment {
			t.Errorf("chunk %d kind: got %s want comment", i+1, c.Kind)
		}
	}
	for _, c := range chunks {
		if c.TokenCount > 60 {
			t.Errorf("chunk exceeded budget: tokens=%d content=%q", c.TokenCount, c.Content)
		}
	}
	for i, c := range chunks {
		if c.Index != i {
			t.Errorf("index %d: got %d", i, c.Index)
		}
	}
}

func TestJira_BodyOverflowStaysBody(t *testing.T) {
	// A body that exceeds the per-chunk budget should split into multiple
	// chunks, all tagged body.
	iss := &jira.NormalizedIssue{
		Key:         "PLAT-3",
		Title:       "Big one",
		Description: strings.Repeat("filler sentence with words and content. ", 40),
	}
	chunks, err := Jira(iss, JiraOptions{MaxTokens: 80, OverlapTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 2 {
		t.Fatalf("expected body to split across multiple chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if c.Kind != KindBody {
			t.Errorf("chunk %d: kind=%s want body (body overflow must keep body kind)", i, c.Kind)
		}
	}
}

func TestJira_VeryLongCommentIsSplit(t *testing.T) {
	iss := &jira.NormalizedIssue{
		Key:         "PLAT-3",
		Title:       "Huge thread",
		Description: "short",
		Comments: []jira.NormalizedComment{
			{
				Author:    "Carol",
				CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
				// Far exceeds the 50-token budget below.
				Body: strings.Repeat("word ", 500),
			},
		},
	}
	chunks, err := Jira(iss, JiraOptions{MaxTokens: 50, OverlapTokens: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 3 {
		t.Fatalf("expected many chunks from splitting a huge comment, got %d", len(chunks))
	}
	for _, c := range chunks {
		if c.TokenCount > 50 {
			t.Errorf("chunk exceeded budget: %d tokens", c.TokenCount)
		}
	}
}

func TestJira_CommentHeaderVariants(t *testing.T) {
	// Reconstructed tickets carry partial metadata. Verify each header
	// branch — never emit "by " or "on 0001-01-01" filler.
	cases := []struct {
		name    string
		comment jira.NormalizedComment
		wantSub []string
		denySub []string
	}{
		{
			name:    "both",
			comment: jira.NormalizedComment{Author: "Alice", CreatedAt: time.Date(2026, 1, 16, 0, 0, 0, 0, time.UTC), Body: "x"},
			wantSub: []string{"## Comment by Alice on 2026-01-16"},
			denySub: []string{"0001-01-01"},
		},
		{
			name:    "author-only",
			comment: jira.NormalizedComment{Author: "Salvador Spataro", Body: "x"},
			wantSub: []string{"## Comment by Salvador Spataro\n"},
			denySub: []string{"0001-01-01", " on "},
		},
		{
			name:    "date-only",
			comment: jira.NormalizedComment{CreatedAt: time.Date(2019, 8, 17, 0, 0, 0, 0, time.UTC), Body: "x"},
			wantSub: []string{"## Comment on 2019-08-17"},
			denySub: []string{"by "},
		},
		{
			name:    "neither",
			comment: jira.NormalizedComment{Body: "x"},
			wantSub: []string{"## Comment\n"},
			denySub: []string{"by ", " on ", "0001-01-01"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chunks, err := Jira(&jira.NormalizedIssue{
				Key:      "TES-1",
				Title:    "T",
				Comments: []jira.NormalizedComment{tc.comment},
			}, JiraOptions{})
			if err != nil {
				t.Fatal(err)
			}
			body := chunks[0].Content
			for _, w := range tc.wantSub {
				if !strings.Contains(body, w) {
					t.Errorf("missing %q in:\n%s", w, body)
				}
			}
			for _, d := range tc.denySub {
				if strings.Contains(body, d) {
					t.Errorf("unexpected %q in:\n%s", d, body)
				}
			}
		})
	}
}

func TestTokenCount(t *testing.T) {
	n, err := TokenCount("hello world")
	if err != nil {
		t.Fatal(err)
	}
	if n < 2 {
		t.Errorf("unexpectedly low token count: %d", n)
	}
}

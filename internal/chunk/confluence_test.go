package chunk

import (
	"strings"
	"testing"
	"time"

	"github.com/treetop/rag-svc/internal/sources/confluence"
)

func TestConfluence_SmallSinglePageSingleChunk(t *testing.T) {
	p := &confluence.NormalizedPage{
		ID:        "1",
		Title:     "Runbook",
		SpaceKey:  "OPS",
		UpdatedAt: time.Now(),
		Body:      "A short description.",
	}
	chunks, err := Confluence(p, p.BodyMarkdown(), ConfluenceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 {
		t.Fatalf("want 1 chunk, got %d", len(chunks))
	}
	if chunks[0].Kind != KindSection {
		t.Errorf("kind: got %s want section", chunks[0].Kind)
	}
	if !strings.Contains(chunks[0].Content, "Runbook") {
		t.Errorf("missing title: %q", chunks[0].Content)
	}
}

func TestConfluence_SplitsOnHeadings(t *testing.T) {
	p := &confluence.NormalizedPage{
		Title: "Big doc",
		Body: `Intro paragraph.

## Section A

Content for A.

## Section B

Content for B.

### Subsection B1

More B1 stuff.`,
	}
	chunks, err := Confluence(p, p.BodyMarkdown(), ConfluenceOptions{MaxTokens: 4000})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 3 {
		t.Fatalf("expected at least 3 chunks (intro, A, B/B1), got %d", len(chunks))
	}
	for _, c := range chunks {
		if c.Kind != KindSection {
			t.Errorf("kind: got %s want section", c.Kind)
		}
	}
	// First chunk should carry the title heading.
	if !strings.Contains(chunks[0].Content, "# Big doc") {
		t.Errorf("first chunk missing title heading")
	}
}

func TestConfluence_OverflowSectionSplitsWithOverlap(t *testing.T) {
	longText := strings.Repeat("sentence about things. ", 120)
	p := &confluence.NormalizedPage{
		Title: "Long",
		Body:  "## Section\n\n" + longText,
	}
	chunks, err := Confluence(p, p.BodyMarkdown(), ConfluenceOptions{MaxTokens: 80, OverlapTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 3 {
		t.Fatalf("expected multiple overflow chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if c.TokenCount > 80 {
			t.Errorf("chunk %d exceeded budget: %d tokens", i, c.TokenCount)
		}
		if c.Kind != KindSection {
			t.Errorf("chunk %d: kind=%s want section", i, c.Kind)
		}
	}
}

func TestConfluence_NoHeadingsStillChunks(t *testing.T) {
	p := &confluence.NormalizedPage{
		Title: "No headings",
		Body:  "Just some content without any markdown heading levels.",
	}
	chunks, err := Confluence(p, p.BodyMarkdown(), ConfluenceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) == 0 {
		t.Fatal("expected at least one chunk")
	}
}

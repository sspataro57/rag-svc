package chunk

import (
	"strings"
	"testing"

	"github.com/treetop/rag-svc/internal/sources/document"
)

func TestDocument_ShortSingleChunk(t *testing.T) {
	d := &document.NormalizedDocument{
		Title: "Notes",
		Body:  "A short note.",
	}
	chunks, err := Document(d, DocumentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 {
		t.Fatalf("want 1 chunk, got %d", len(chunks))
	}
	if chunks[0].Kind != KindSection {
		t.Errorf("kind: got %s want section", chunks[0].Kind)
	}
	if !strings.Contains(chunks[0].Content, "# Notes") {
		t.Errorf("title heading missing: %q", chunks[0].Content)
	}
}

func TestDocument_LongSplitsRespectBudget(t *testing.T) {
	d := &document.NormalizedDocument{
		Title: "Long",
		Body:  strings.Repeat("paragraph sentence about topics and things.\n\n", 60),
	}
	chunks, err := Document(d, DocumentOptions{MaxTokens: 80, OverlapTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 3 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		if c.TokenCount > 80 {
			t.Errorf("chunk %d exceeded budget: %d tokens", i, c.TokenCount)
		}
	}
}

func TestDocument_EmptyBodyNoChunks(t *testing.T) {
	chunks, err := Document(&document.NormalizedDocument{Title: "x", Body: ""}, DocumentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 0 {
		t.Errorf("expected 0 chunks, got %d", len(chunks))
	}
}

// Package chunk turns normalized source content into chunks suitable for
// embedding. Per CLAUDE.md each source type has its own strategy; step 2
// only implements Jira.
package chunk

import (
	"fmt"
	"strings"
	"time"

	"github.com/pkoukk/tiktoken-go"

	"github.com/treetop/rag-svc/internal/sources/jira"
)

// Kind matches the chunks.chunk_kind values accepted by 0002_chunk_kind.
type Kind string

const (
	KindBody    Kind = "body"
	KindComment Kind = "comment"
	KindSection Kind = "section" // reserved for Confluence/doc strategies (step 6/7)
)

// Chunk is a unit of text ready to be embedded and stored.
type Chunk struct {
	Index      int
	Content    string
	TokenCount int
	Kind       Kind
}

// JiraOptions controls the Jira chunking strategy.
type JiraOptions struct {
	// MaxTokens per chunk (default 4000 per CLAUDE.md).
	MaxTokens int
	// OverlapTokens between overflow chunks (default 200).
	OverlapTokens int
}

func (o JiraOptions) withDefaults() JiraOptions {
	if o.MaxTokens == 0 {
		o.MaxTokens = 4000
	}
	if o.OverlapTokens == 0 {
		o.OverlapTokens = 200
	}
	return o
}

// Jira chunks a NormalizedIssue per the spec:
//   - First chunk: title + description + as many comments as fit under MaxTokens.
//     chunk_kind = "body".
//   - Overflow comments go into subsequent chunks with OverlapTokens of carry-over
//     from the previous chunk so context is preserved across the boundary.
//     chunk_kind = "comment".
//
// The assembled text matches NormalizedIssue.BodyMarkdown so `rag-svc reindex`
// can reconstruct chunks from sources.body_markdown alone.
func Jira(issue *jira.NormalizedIssue, opts JiraOptions) ([]Chunk, error) {
	opts = opts.withDefaults()
	enc, err := encoder()
	if err != nil {
		return nil, err
	}

	type segment struct {
		text string
		kind Kind
	}
	var segments []segment
	// Lead with the title so embeddings see it; keeps short-query matches
	// against issue summaries strong.
	head := fmt.Sprintf("# %s (%s)\n\n", issue.Title, issue.Key)
	if issue.Description != "" {
		head += strings.TrimRight(issue.Description, "\n") + "\n"
	}
	segments = append(segments, segment{text: head, kind: KindBody})

	for _, c := range issue.Comments {
		header := commentHeader(c.Author, c.CreatedAt)
		body := strings.TrimRight(c.Body, "\n")
		segments = append(segments, segment{text: header + body, kind: KindComment})
	}

	var out []Chunk
	var cur strings.Builder
	var curTokens []int // encoded token ids, used for overlap carry-over
	curKind := KindBody

	flush := func() {
		if cur.Len() == 0 {
			return
		}
		out = append(out, Chunk{
			Index:      len(out),
			Content:    strings.TrimLeft(cur.String(), "\n"),
			TokenCount: len(curTokens),
			Kind:       curKind,
		})
		cur.Reset()
		curTokens = nil
	}

	// addOverlap pulls the last OverlapTokens from the most recently flushed
	// chunk into the current buffer so context crosses the boundary.
	addOverlap := func() {
		if opts.OverlapTokens <= 0 || len(out) == 0 {
			return
		}
		prev := out[len(out)-1]
		prevTokens := enc.Encode(prev.Content, nil, nil)
		start := len(prevTokens) - opts.OverlapTokens
		if start < 0 {
			start = 0
		}
		overlapIDs := prevTokens[start:]
		cur.WriteString(enc.Decode(overlapIDs))
		curTokens = append(curTokens, overlapIDs...)
	}

	for _, seg := range segments {
		tokens := enc.Encode(seg.text, nil, nil)
		// Fast path: the whole segment fits in the current chunk.
		if len(curTokens)+len(tokens) <= opts.MaxTokens {
			cur.WriteString(seg.text)
			curTokens = append(curTokens, tokens...)
			continue
		}
		// The segment doesn't fit. Close out the current chunk and start
		// a new one whose kind matches this segment's kind (body overflow
		// stays "body"; comment overflow becomes "comment").
		flush()
		curKind = seg.kind
		addOverlap()

		// Split across as many chunks as needed to drain the segment.
		remaining := tokens
		for len(remaining) > 0 {
			budget := opts.MaxTokens - len(curTokens)
			if budget <= 0 {
				flush()
				curKind = seg.kind
				addOverlap()
				budget = opts.MaxTokens - len(curTokens)
			}
			take := budget
			if take > len(remaining) {
				take = len(remaining)
			}
			piece := enc.Decode(remaining[:take])
			cur.WriteString(piece)
			curTokens = append(curTokens, remaining[:take]...)
			remaining = remaining[take:]
		}
	}
	flush()

	return out, nil
}

// commentHeader builds the chunk-level comment separator. Reconstructed
// tickets often carry partial metadata: the author may be known (the
// feeder attributes empty-author bodies to the configured default) while
// the timestamp is missing, or both fields may be blank. Drop the missing
// half rather than emitting "by  on 0001-01-01" garbage into the
// embedding.
func commentHeader(author string, createdAt time.Time) string {
	hasAuthor := strings.TrimSpace(author) != ""
	hasDate := !createdAt.IsZero()
	switch {
	case hasAuthor && hasDate:
		return fmt.Sprintf("\n\n## Comment by %s on %s\n\n", author, createdAt.UTC().Format("2006-01-02"))
	case hasAuthor:
		return fmt.Sprintf("\n\n## Comment by %s\n\n", author)
	case hasDate:
		return fmt.Sprintf("\n\n## Comment on %s\n\n", createdAt.UTC().Format("2006-01-02"))
	default:
		return "\n\n## Comment\n\n"
	}
}

// encoder returns the shared cl100k_base tokenizer. tiktoken-go caches the
// encoding internally.
func encoder() (*tiktoken.Tiktoken, error) {
	enc, err := tiktoken.GetEncoding("cl100k_base")
	if err != nil {
		return nil, fmt.Errorf("chunk: load cl100k_base: %w", err)
	}
	return enc, nil
}

// TokenCount reports the cl100k_base token count for s — exposed for callers
// (e.g., embedder batch sizing) that need to stay under model context limits.
func TokenCount(s string) (int, error) {
	enc, err := encoder()
	if err != nil {
		return 0, err
	}
	return len(enc.Encode(s, nil, nil)), nil
}

package chunk

import (
	"strings"

	"github.com/treetop/rag-svc/internal/sources/document"
)

// DocumentOptions controls the document chunking strategy.
type DocumentOptions struct {
	MaxTokens     int
	OverlapTokens int
}

func (o DocumentOptions) withDefaults() DocumentOptions {
	if o.MaxTokens == 0 {
		o.MaxTokens = 1000
	}
	if o.OverlapTokens == 0 {
		o.OverlapTokens = 150
	}
	return o
}

// Document chunks a NormalizedDocument with a pure paragraph-then-sentence
// recursive split — no heading awareness, because uploaded documents
// (PDFs especially) carry unreliable heading structure. Every chunk is
// tagged chunk_kind=section for consistency with Confluence's strategy.
func Document(d *document.NormalizedDocument, opts DocumentOptions) ([]Chunk, error) {
	opts = opts.withDefaults()
	enc, err := encoder()
	if err != nil {
		return nil, err
	}
	body := strings.TrimSpace(d.Body)
	if body == "" {
		return nil, nil
	}

	// Prepend a synthetic H1 so the first chunk carries the title — mirrors
	// the Jira/Confluence chunkers so search snippets always self-identify.
	body = "# " + d.Title + "\n\n" + body
	toks := enc.Encode(body, nil, nil)
	if len(toks) <= opts.MaxTokens {
		return []Chunk{{
			Index:      0,
			Content:    body,
			TokenCount: len(toks),
			Kind:       KindSection,
		}}, nil
	}

	pieces := splitOversizeSection(body, ConfluenceOptions{
		MaxTokens:     opts.MaxTokens,
		OverlapTokens: opts.OverlapTokens,
	}, enc)
	out := make([]Chunk, 0, len(pieces))
	for i, p := range pieces {
		ptoks := enc.Encode(p, nil, nil)
		out = append(out, Chunk{
			Index:      i,
			Content:    p,
			TokenCount: len(ptoks),
			Kind:       KindSection,
		})
	}
	return out, nil
}

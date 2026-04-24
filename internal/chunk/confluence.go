package chunk

import (
	"bufio"
	"fmt"
	"regexp"
	"strings"

	"github.com/treetop/rag-svc/internal/sources/confluence"
)

// ConfluenceOptions controls the Confluence chunking strategy.
type ConfluenceOptions struct {
	// MaxTokens per chunk (default 1000 per CLAUDE.md).
	MaxTokens int
	// OverlapTokens between overflow chunks (default 150).
	OverlapTokens int
}

func (o ConfluenceOptions) withDefaults() ConfluenceOptions {
	if o.MaxTokens == 0 {
		o.MaxTokens = 1000
	}
	if o.OverlapTokens == 0 {
		o.OverlapTokens = 150
	}
	return o
}

// headingRE matches markdown headings `#`, `##`, or `###` — the levels we
// split on. Deeper headings stay inside their parent section.
var headingRE = regexp.MustCompile(`(?m)^(#{1,3})\s+(.+)$`)

// Confluence chunks a NormalizedPage using a header-aware split:
//   - Break the page on h1/h2/h3 boundaries so each section travels with its
//     heading.
//   - Each section fits one chunk when under MaxTokens; otherwise it
//     recursively splits on paragraph boundaries (blank lines), and finally
//     sentences if a single paragraph still exceeds the budget.
//   - Adjacent overflow chunks share OverlapTokens from the previous chunk's
//     tail to preserve context across the boundary.
//
// Every chunk is tagged chunk_kind=section. The body text the chunker
// operates on is the normalized body (which already starts with a title-H1
// line); the caller is responsible for resolving any sentinel tokens
// before chunking so the chunk's token_count reflects final text.
func Confluence(page *confluence.NormalizedPage, body string, opts ConfluenceOptions) ([]Chunk, error) {
	opts = opts.withDefaults()
	enc, err := encoder()
	if err != nil {
		return nil, err
	}

	sections := splitByHeadings(body)
	var out []Chunk
	for _, sec := range sections {
		toks := enc.Encode(sec, nil, nil)
		if len(toks) <= opts.MaxTokens {
			out = append(out, Chunk{
				Index:      len(out),
				Content:    sec,
				TokenCount: len(toks),
				Kind:       KindSection,
			})
			continue
		}
		// Section exceeds budget — recursively split.
		pieces := splitOversizeSection(sec, opts, enc)
		for _, p := range pieces {
			ptoks := enc.Encode(p, nil, nil)
			out = append(out, Chunk{
				Index:      len(out),
				Content:    p,
				TokenCount: len(ptoks),
				Kind:       KindSection,
			})
		}
	}

	// Degenerate: page body had no headings AND fit entirely — make sure we
	// emit at least one chunk rather than returning an empty slice.
	if len(out) == 0 && strings.TrimSpace(body) != "" {
		toks := enc.Encode(body, nil, nil)
		out = append(out, Chunk{
			Index:      0,
			Content:    body,
			TokenCount: len(toks),
			Kind:       KindSection,
		})
	}
	_ = page // reserved for future section-aware metadata
	return out, nil
}

// splitByHeadings slices body at each h1/h2/h3 boundary. The heading line
// stays attached to its section. Content before the first heading
// (if any) becomes its own leading section so we don't drop it.
func splitByHeadings(body string) []string {
	matches := headingRE.FindAllStringIndex(body, -1)
	if len(matches) == 0 {
		return []string{body}
	}
	var out []string
	// Content before the first heading.
	if matches[0][0] > 0 {
		head := strings.TrimRight(body[:matches[0][0]], "\n")
		if strings.TrimSpace(head) != "" {
			out = append(out, head)
		}
	}
	for i, m := range matches {
		end := len(body)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		sec := strings.TrimRight(body[m[0]:end], "\n")
		if strings.TrimSpace(sec) != "" {
			out = append(out, sec)
		}
	}
	return out
}

// splitOversizeSection splits a single section that exceeds MaxTokens into
// paragraphs, then greedily packs paragraphs into chunks with overlap. If
// any individual paragraph still exceeds the budget, it falls through to
// sentence splitting, then finally a hard token-boundary split.
func splitOversizeSection(sec string, opts ConfluenceOptions, enc tokenEncoder) []string {
	paras := splitParagraphs(sec)
	var chunks []string
	var cur strings.Builder
	var curTokens []int

	flush := func() {
		if cur.Len() == 0 {
			return
		}
		chunks = append(chunks, strings.TrimRight(cur.String(), "\n"))
		cur.Reset()
		curTokens = nil
	}
	addOverlap := func() {
		if opts.OverlapTokens <= 0 || len(chunks) == 0 {
			return
		}
		prev := chunks[len(chunks)-1]
		prevTokens := enc.Encode(prev, nil, nil)
		start := len(prevTokens) - opts.OverlapTokens
		if start < 0 {
			start = 0
		}
		tail := prevTokens[start:]
		cur.WriteString(enc.Decode(tail))
		cur.WriteString("\n\n")
		curTokens = append(curTokens, tail...)
	}
	appendText := func(text string, toks []int) {
		if cur.Len() > 0 {
			cur.WriteString("\n\n")
		}
		cur.WriteString(text)
		curTokens = append(curTokens, toks...)
	}

	for _, p := range paras {
		ptoks := enc.Encode(p, nil, nil)
		if len(ptoks) > opts.MaxTokens {
			// Flush what we have, then split this paragraph further.
			flush()
			pieces := splitBySentences(p, opts, enc)
			for _, piece := range pieces {
				ptks := enc.Encode(piece, nil, nil)
				if len(curTokens)+len(ptks)+2 > opts.MaxTokens {
					flush()
					addOverlap()
				}
				appendText(piece, ptks)
			}
			continue
		}
		if len(curTokens)+len(ptoks)+2 > opts.MaxTokens {
			flush()
			addOverlap()
		}
		appendText(p, ptoks)
	}
	flush()
	return chunks
}

func splitParagraphs(s string) []string {
	parts := regexp.MustCompile(`\n\s*\n`).Split(s, -1)
	out := parts[:0]
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, strings.TrimSpace(p))
		}
	}
	return out
}

// splitBySentences uses a naive period/newline heuristic — good enough for
// the header-aware chunker's fallback path. Any sentence that itself
// exceeds budget falls back to a hard token split.
func splitBySentences(p string, opts ConfluenceOptions, enc tokenEncoder) []string {
	scanner := bufio.NewScanner(strings.NewReader(p))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	scanner.Split(splitSentences)

	var out []string
	for scanner.Scan() {
		s := strings.TrimSpace(scanner.Text())
		if s == "" {
			continue
		}
		toks := enc.Encode(s, nil, nil)
		if len(toks) > opts.MaxTokens {
			// Hard token split.
			for start := 0; start < len(toks); start += opts.MaxTokens {
				end := start + opts.MaxTokens
				if end > len(toks) {
					end = len(toks)
				}
				out = append(out, enc.Decode(toks[start:end]))
			}
			continue
		}
		out = append(out, s)
	}
	return out
}

// splitSentences is a conservative sentence splitter suitable for
// bufio.Scanner: cuts on `.?!` followed by whitespace or end-of-input.
func splitSentences(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	for i := 0; i < len(data); i++ {
		c := data[i]
		if c == '.' || c == '?' || c == '!' {
			j := i + 1
			// Skip following punctuation/quotes.
			for j < len(data) && (data[j] == ')' || data[j] == ']' || data[j] == '"' || data[j] == '\'') {
				j++
			}
			if j >= len(data) {
				if atEOF {
					return len(data), data, nil
				}
				return 0, nil, nil
			}
			if data[j] == ' ' || data[j] == '\n' || data[j] == '\t' {
				return j + 1, data[:j], nil
			}
		}
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// tokenEncoder is the subset of tiktoken-go.Tiktoken we use; declaring it as
// an interface lets tests swap in a cheap fake if needed.
type tokenEncoder interface {
	Encode(text string, allowedSpecial, disallowedSpecial []string) []int
	Decode(ids []int) string
}

// Keep fmt.Sprintf reachable for future heading-aware diagnostics.
var _ = fmt.Sprintf

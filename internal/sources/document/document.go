// Package document handles user-uploaded files: sniffs the content type,
// extracts text (markdown/text pass-through, HTML strip via goquery, PDF
// via ledongthuc/pdf), derives a title, and produces a NormalizedDocument
// ready for the chunker.
//
// The PDF library choice diverges from CLAUDE.md's literal "pdfcpu" hint
// because pdfcpu's public text-extraction API writes raw PDF content
// streams (operator/glyph codes), not plain text. ledongthuc/pdf is the
// practical MIT-licensed alternative for our retrieval use case.
package document

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/ledongthuc/pdf"
)

// Kind identifies how a document was extracted. Exposed so the upload
// handler can put it in `extra.extraction_method` for audit.
type Kind string

const (
	KindMarkdown Kind = "markdown"
	KindText     Kind = "text"
	KindHTML     Kind = "html"
	KindPDF      Kind = "pdf"
)

// NormalizedDocument is the storage-shaped view of an uploaded document.
type NormalizedDocument struct {
	Title      string
	Body       string // markdown
	Extraction Kind
	Pages      int // populated for PDFs; zero otherwise
	Extra      map[string]any
	UpdatedAt  time.Time
}

// ErrScannedPDF is returned when a PDF yields near-zero text; OCR is a
// documented v1 non-goal and the caller should surface a 422 instead of
// storing garbage.
var ErrScannedPDF = errors.New("document: scanned PDF (no embedded text)")

// ErrUnsupportedType means we couldn't match the input to any extraction
// path. The upload handler maps this to HTTP 415.
var ErrUnsupportedType = errors.New("document: unsupported content type")

// DetectKind picks an extraction strategy from the declared content type
// and the filename. Ambiguous octet-stream uploads fall back to the
// extension. Unknown → ErrUnsupportedType.
func DetectKind(contentType, filename string) (Kind, error) {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if idx := strings.Index(ct, ";"); idx >= 0 {
		ct = strings.TrimSpace(ct[:idx])
	}
	switch ct {
	case "text/markdown", "text/x-markdown":
		return KindMarkdown, nil
	case "text/plain":
		return KindText, nil
	case "text/html", "application/xhtml+xml":
		return KindHTML, nil
	case "application/pdf":
		return KindPDF, nil
	}
	// Fallback: extension-based when the browser sent octet-stream.
	if ct == "" || ct == "application/octet-stream" {
		switch strings.ToLower(filepath.Ext(filename)) {
		case ".md", ".markdown":
			return KindMarkdown, nil
		case ".txt":
			return KindText, nil
		case ".html", ".htm":
			return KindHTML, nil
		case ".pdf":
			return KindPDF, nil
		}
	}
	return "", fmt.Errorf("%w: %q (file=%q)", ErrUnsupportedType, contentType, filename)
}

// Extract dispatches to the right extractor and derives a title from the
// text plus filename fallback. The resulting markdown body is what the
// chunker operates on.
func Extract(data []byte, kind Kind, filename string) (*NormalizedDocument, error) {
	var doc NormalizedDocument
	doc.Extraction = kind
	doc.UpdatedAt = time.Now().UTC()

	switch kind {
	case KindMarkdown:
		doc.Body = string(data)
	case KindText:
		doc.Body = string(data)
	case KindHTML:
		body, err := extractHTML(data)
		if err != nil {
			return nil, err
		}
		doc.Body = body
	case KindPDF:
		body, pages, err := extractPDF(data)
		if err != nil {
			return nil, err
		}
		if isLikelyScanned(body, pages) {
			return nil, ErrScannedPDF
		}
		doc.Body = body
		doc.Pages = pages
	default:
		return nil, fmt.Errorf("%w: kind=%s", ErrUnsupportedType, kind)
	}

	doc.Title = deriveTitle(doc.Body, filename, kind)
	return &doc, nil
}

// ---- HTML ----

func extractHTML(data []byte) (string, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("document: parse html: %w", err)
	}
	// Drop non-content nodes before rendering.
	doc.Find("script, style, noscript, template").Remove()

	var b strings.Builder
	// Title first so it anchors search.
	if t := strings.TrimSpace(doc.Find("title").First().Text()); t != "" {
		fmt.Fprintf(&b, "# %s\n\n", t)
	}
	// h1–h6 and p in document order, plus li for lists. goquery doesn't
	// expose document-order traversal trivially, so we rely on .Each over
	// the merged selector, which preserves insertion order.
	doc.Find("body").Each(func(_ int, body *goquery.Selection) {
		renderBlock(body, &b, 0)
	})
	return strings.TrimSpace(b.String()) + "\n", nil
}

// renderBlock walks selection children emitting markdown-flavored text.
// Kept intentionally light: HTML uploads aren't the common path.
func renderBlock(s *goquery.Selection, b *strings.Builder, depth int) {
	s.Contents().Each(func(_ int, c *goquery.Selection) {
		if c.Length() == 0 {
			return
		}
		node := c.Get(0)
		if node.Type == 1 { // TextNode
			txt := strings.TrimSpace(c.Text())
			if txt != "" {
				b.WriteString(txt)
				b.WriteByte(' ')
			}
			return
		}
		tag := strings.ToLower(node.Data)
		switch tag {
		case "h1", "h2", "h3", "h4", "h5", "h6":
			level := int(tag[1] - '0')
			fmt.Fprintf(b, "\n\n%s %s\n\n", strings.Repeat("#", level), strings.TrimSpace(c.Text()))
		case "p":
			fmt.Fprintf(b, "\n\n%s\n\n", strings.TrimSpace(c.Text()))
		case "li":
			fmt.Fprintf(b, "- %s\n", strings.TrimSpace(c.Text()))
		case "br":
			b.WriteString("  \n")
		case "pre", "code":
			b.WriteString("\n```\n")
			b.WriteString(c.Text())
			b.WriteString("\n```\n\n")
		default:
			renderBlock(c, b, depth+1)
		}
	})
}

// ---- PDF ----

func extractPDF(data []byte) (string, int, error) {
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", 0, fmt.Errorf("document: pdf open: %w", err)
	}
	pages := r.NumPage()
	txtReader, err := r.GetPlainText()
	if err != nil {
		return "", 0, fmt.Errorf("document: pdf plain text: %w", err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, txtReader); err != nil {
		return "", 0, fmt.Errorf("document: pdf read: %w", err)
	}
	// Light cleanup: collapse runs of whitespace but preserve paragraph
	// breaks (double newlines become paragraph separators naturally
	// because pdf.GetPlainText separates pages with newlines).
	text := strings.TrimSpace(buf.String())
	text = collapseSpaces.ReplaceAllString(text, " ")
	text = strings.ReplaceAll(text, "\r", "\n")
	return text, pages, nil
}

var collapseSpaces = regexp.MustCompile(`[ \t]{2,}`)

// isLikelyScanned: zero pages is a real error; otherwise require at least
// ~10 characters of text per page on average. Many real PDFs have whole
// sections with minimal text (cover pages, mostly-figures), so the
// threshold is kept low.
func isLikelyScanned(text string, pages int) bool {
	if pages <= 0 {
		return true
	}
	letters := 0
	for _, r := range text {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			letters++
		}
	}
	return letters < pages*10
}

// ---- Title ----

var (
	markdownH1RE = regexp.MustCompile(`(?m)^#\s+(.+?)\s*$`)
)

func deriveTitle(body, filename string, kind Kind) string {
	switch kind {
	case KindMarkdown:
		if m := markdownH1RE.FindStringSubmatch(body); m != nil {
			return strings.TrimSpace(m[1])
		}
	case KindHTML:
		// extractHTML already prepends `# Title\n\n` when a <title>
		// exists; fall through to the markdown H1 detector.
		if m := markdownH1RE.FindStringSubmatch(body); m != nil {
			return strings.TrimSpace(m[1])
		}
	}
	// Generic: first non-empty line, else filename without extension.
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > 200 {
			line = line[:200]
		}
		return line
	}
	base := filepath.Base(filename)
	ext := filepath.Ext(base)
	if ext != "" {
		base = base[:len(base)-len(ext)]
	}
	if base == "" {
		return "Untitled document"
	}
	return base
}

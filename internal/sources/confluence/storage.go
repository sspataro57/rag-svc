package confluence

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"
)

// StorageToMarkdown converts a Confluence storage-format XHTML fragment to
// markdown. Unknown macros recurse their inner text content (CLAUDE.md:
// "Unknown macro — emit inner text content") so nothing is silently dropped.
//
// Page-to-page links emit sentinel tokens the orchestrator rewrites on a
// second pass once the (space, title)→URL map is complete:
//
//	⟦pg-id:{id}|{fallback text}⟧
//	⟦pg-title:{space-key}|{title}⟧
//
// In the common case both sentinels carry a fallback so even unresolved
// links still contribute text to retrieval.
func StorageToMarkdown(xhtml string) (string, error) {
	if strings.TrimSpace(xhtml) == "" {
		return "", nil
	}
	// golang.org/x/net/html is HTML5-flavored: it silently drops CDATA
	// sections and doesn't honor XHTML self-closing syntax for unknown
	// custom elements. preprocess() rewrites both so the parser sees
	// well-formed HTML while preserving every byte that carries text.
	fixed := preprocess(xhtml)
	doc, err := html.Parse(strings.NewReader("<body>" + fixed + "</body>"))
	if err != nil {
		return "", fmt.Errorf("confluence: parse storage: %w", err)
	}
	root := goquery.NewDocumentFromNode(doc).Find("body").First()
	var r stRenderer
	r.renderChildren(root.Nodes[0])
	return strings.TrimRight(r.buf.String(), "\n") + "\n", nil
}

var (
	// Matches <ac:foo …/> or <ri:foo …/> and rewrites to <ac:foo …></ac:foo>.
	// XML attribute values can't contain '>' in well-formed XHTML, so the
	// `[^>]*?` body is safe.
	selfClosingRE = regexp.MustCompile(`<((?:ac|ri):[a-zA-Z0-9-]+)((?:\s[^>]*?)?)\s*/>`)

	// CDATA sections. We replace the wrapper with the HTML-escaped inner
	// text so html.Parse surfaces it as a TextNode. Dot-matches-newline
	// flag because code blocks often span lines.
	cdataRE = regexp.MustCompile(`(?s)<!\[CDATA\[(.*?)\]\]>`)
)

func preprocess(s string) string {
	s = cdataRE.ReplaceAllStringFunc(s, func(match string) string {
		inner := match[9 : len(match)-3] // strip "<![CDATA[" and "]]>"
		return htmlEscape(inner)
	})
	s = selfClosingRE.ReplaceAllString(s, "<$1$2></$1>")
	return s
}

func htmlEscape(s string) string {
	return strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	).Replace(s)
}

type stRenderer struct {
	buf        strings.Builder
	quoteDepth int
	listStack  []listFrame
	linkHref   string
}

type listFrame struct {
	ordered bool
	index   int
}

func (r *stRenderer) renderChildren(n *html.Node) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		r.renderNode(c)
	}
}

func (r *stRenderer) renderNode(n *html.Node) {
	switch n.Type {
	case html.TextNode:
		r.buf.WriteString(escapeText(n.Data))
	case html.ElementNode:
		r.renderElement(n)
	}
}

func (r *stRenderer) renderElement(n *html.Node) {
	name := strings.ToLower(n.Data)

	// ac: and ri: namespaces show up as prefixed element names on the
	// golang.org/x/net/html parser.
	switch {
	case strings.HasPrefix(name, "ac:structured-macro"):
		r.renderMacro(n)
		return
	case name == "ac:link":
		r.renderLink(n)
		return
	case name == "ac:image":
		// Attachments out of scope per CLAUDE.md.
		return
	case name == "ac:plain-text-body", name == "ac:rich-text-body":
		// Only reached when the enclosing macro renderer explicitly
		// recurses; otherwise the macro handler consumes these. If we
		// somehow hit one at the top level, treat as transparent.
		r.renderChildren(n)
		return
	case name == "ac:parameter":
		// Parameters are consumed by the macro handler; at the top level
		// they don't contribute text.
		return
	}

	switch name {
	case "h1", "h2", "h3", "h4", "h5", "h6":
		level := int(name[1] - '0')
		r.writeBlockPrefix()
		r.buf.WriteString(strings.Repeat("#", level))
		r.buf.WriteByte(' ')
		r.renderInline(n)
		r.buf.WriteString("\n\n")

	case "p":
		// Skip paragraphs that are just &nbsp; placeholders.
		txt := strings.TrimSpace(collapseNBSP(textOf(n)))
		if txt == "" {
			return
		}
		r.writeBlockPrefix()
		r.renderInline(n)
		r.buf.WriteString("\n\n")

	case "ul":
		r.listStack = append(r.listStack, listFrame{ordered: false})
		r.renderChildren(n)
		r.listStack = r.listStack[:len(r.listStack)-1]
		if len(r.listStack) == 0 {
			r.buf.WriteByte('\n')
		}
	case "ol":
		r.listStack = append(r.listStack, listFrame{ordered: true, index: 1})
		r.renderChildren(n)
		r.listStack = r.listStack[:len(r.listStack)-1]
		if len(r.listStack) == 0 {
			r.buf.WriteByte('\n')
		}
	case "li":
		if len(r.listStack) == 0 {
			r.renderInline(n)
			return
		}
		frameIdx := len(r.listStack) - 1
		r.writeBlockPrefix()
		r.buf.WriteString(strings.Repeat("  ", frameIdx))
		if r.listStack[frameIdx].ordered {
			fmt.Fprintf(&r.buf, "%d. ", r.listStack[frameIdx].index)
			r.listStack[frameIdx].index++
		} else {
			r.buf.WriteString("- ")
		}
		r.renderInline(n)
		r.buf.WriteByte('\n')

	case "pre":
		r.writeBlockPrefix()
		r.buf.WriteString("```\n")
		r.buf.WriteString(textOf(n))
		r.buf.WriteString("\n```\n\n")

	case "blockquote":
		r.quoteDepth++
		r.renderChildren(n)
		r.quoteDepth--

	case "hr":
		r.writeBlockPrefix()
		r.buf.WriteString("---\n\n")

	case "br":
		r.buf.WriteString("  \n")

	case "a":
		href := getAttr(n, "href")
		if href == "" {
			r.renderInline(n)
			return
		}
		r.buf.WriteByte('[')
		r.renderInline(n)
		r.buf.WriteString("](")
		r.buf.WriteString(href)
		r.buf.WriteByte(')')

	case "strong", "b":
		r.buf.WriteString("**")
		r.renderInline(n)
		r.buf.WriteString("**")
	case "em", "i":
		r.buf.WriteByte('*')
		r.renderInline(n)
		r.buf.WriteByte('*')
	case "code":
		r.buf.WriteByte('`')
		r.buf.WriteString(textOf(n))
		r.buf.WriteByte('`')

	case "table":
		r.renderTable(n)

	case "div", "span":
		// Transparent wrappers — render children.
		r.renderChildren(n)

	default:
		// Unknown element: render children so text content survives.
		r.renderChildren(n)
	}
}

func (r *stRenderer) renderInline(n *html.Node) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.TextNode {
			r.buf.WriteString(escapeText(collapseNBSP(c.Data)))
			continue
		}
		if c.Type == html.ElementNode {
			r.renderElement(c)
		}
	}
}

// writeBlockPrefix emits the leading `> ` for any open blockquote nesting.
func (r *stRenderer) writeBlockPrefix() {
	for i := 0; i < r.quoteDepth; i++ {
		r.buf.WriteString("> ")
	}
}

// ---- Macros ----

func (r *stRenderer) renderMacro(n *html.Node) {
	name := getAttr(n, "ac:name")
	switch name {
	case "code":
		r.renderCodeMacro(n)
	case "info", "note", "warning", "tip":
		r.renderPanelMacro(n, name)
	case "expand":
		// Render children unwrapped.
		r.renderChildren(stripToBody(n))
	case "toc", "children", "recently-updated", "contributors", "view-file", "lucidchart", "attachments":
		// Intentional drops per CLAUDE.md's macro table (toc) and
		// attachments-adjacent macros that don't carry durable text.
		return
	case "jira":
		// Render the Jira issue key as a plain token so it's queryable;
		// the server param carries it.
		key := getMacroParam(n, "key")
		if key != "" {
			r.buf.WriteString(key)
		}
	default:
		// Unknown macro: emit inner text content. We render rich-text-body
		// children as markdown if present; otherwise emit plain text.
		if rich := findChild(n, "ac:rich-text-body"); rich != nil {
			r.renderChildren(rich)
		} else if plain := findChild(n, "ac:plain-text-body"); plain != nil {
			r.buf.WriteString(textOf(plain))
		} else {
			// Fall back to stripped text of everything inside.
			r.buf.WriteString(textOf(n))
		}
	}
}

func (r *stRenderer) renderCodeMacro(n *html.Node) {
	lang := getMacroParam(n, "language")
	plain := findChild(n, "ac:plain-text-body")
	var body string
	if plain != nil {
		body = textOf(plain)
	}
	r.writeBlockPrefix()
	r.buf.WriteString("```")
	r.buf.WriteString(lang)
	r.buf.WriteByte('\n')
	r.buf.WriteString(strings.TrimRight(body, "\n"))
	r.buf.WriteString("\n```\n\n")
}

func (r *stRenderer) renderPanelMacro(n *html.Node, kind string) {
	label := strings.ToUpper(kind[:1]) + kind[1:]
	rich := findChild(n, "ac:rich-text-body")
	r.writeBlockPrefix()
	r.buf.WriteString("> **")
	r.buf.WriteString(label)
	r.buf.WriteString(":** ")
	if rich != nil {
		// Render rich body inline after the label; trim trailing blank
		// lines so the quote stays cohesive.
		var sub stRenderer
		sub.quoteDepth = 0
		sub.renderInline(rich)
		r.buf.WriteString(strings.TrimSpace(sub.buf.String()))
	}
	r.buf.WriteString("\n\n")
}

// ---- Links ----
//
// <ac:link><ri:page ri:content-title="Foo" [ri:space-key="ENG"]/></ac:link>
// <ac:link><ri:page ri:content-id="12345"/></ac:link>
// <ac:link><ri:user ri:account-id="..."/></ac:link>
//
// For page links we emit a sentinel that the two-pass resolver rewrites
// once the URL map is available. User links render inline as "@user".
func (r *stRenderer) renderLink(n *html.Node) {
	// Optional display text lives in <ac:plain-text-link-body> or as a
	// nested <ac:link-body>. Extract it if present; otherwise we fall back
	// to the target's natural label.
	var displayText string
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode {
			nm := strings.ToLower(c.Data)
			if nm == "ac:plain-text-link-body" || nm == "ac:link-body" {
				displayText = strings.TrimSpace(textOf(c))
			}
		}
	}

	if page := findChild(n, "ri:page"); page != nil {
		if id := getAttr(page, "ri:content-id"); id != "" {
			label := displayText
			if label == "" {
				label = "page " + id
			}
			fmt.Fprintf(&r.buf, "⟦pg-id:%s|%s⟧", id, label)
			return
		}
		if title := getAttr(page, "ri:content-title"); title != "" {
			space := getAttr(page, "ri:space-key") // may be empty = current space
			label := displayText
			if label == "" {
				label = title
			}
			fmt.Fprintf(&r.buf, "⟦pg-title:%s|%s|%s⟧", space, title, label)
			return
		}
	}
	if user := findChild(n, "ri:user"); user != nil {
		label := displayText
		if label == "" {
			label = "user"
		}
		r.buf.WriteByte('@')
		r.buf.WriteString(strings.TrimPrefix(label, "@"))
		_ = user
		return
	}
	// Unknown ri:* target — emit any display text we found.
	if displayText != "" {
		r.buf.WriteString(displayText)
	}
}

// ---- Tables ----

func (r *stRenderer) renderTable(n *html.Node) {
	var rows [][]string
	for row := firstChildElem(n); row != nil; row = nextSiblingElem(row) {
		if row.Data == "tbody" || row.Data == "thead" {
			// Recurse into grouping elements.
			for sub := firstChildElem(row); sub != nil; sub = nextSiblingElem(sub) {
				if sub.Data == "tr" {
					rows = append(rows, collectRow(sub))
				}
			}
			continue
		}
		if row.Data == "tr" {
			rows = append(rows, collectRow(row))
		}
	}
	if len(rows) == 0 {
		return
	}
	r.writeBlockPrefix()
	writeRow := func(cells []string) {
		r.buf.WriteString("| ")
		r.buf.WriteString(strings.Join(cells, " | "))
		r.buf.WriteString(" |\n")
	}
	writeRow(rows[0])
	r.writeBlockPrefix()
	sep := make([]string, len(rows[0]))
	for i := range sep {
		sep[i] = "---"
	}
	writeRow(sep)
	for i := 1; i < len(rows); i++ {
		row := rows[i]
		for len(row) < len(rows[0]) {
			row = append(row, "")
		}
		r.writeBlockPrefix()
		writeRow(row)
	}
	r.buf.WriteByte('\n')
}

func collectRow(tr *html.Node) []string {
	var cells []string
	for c := firstChildElem(tr); c != nil; c = nextSiblingElem(c) {
		if c.Data != "th" && c.Data != "td" {
			continue
		}
		cells = append(cells, strings.ReplaceAll(strings.TrimSpace(collapseNBSP(textOf(c))), "|", "\\|"))
	}
	return cells
}

// ---- node helpers ----

func getAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		// html parser lowercases unnamespaced attributes but leaves
		// namespaced ones alone; handle either.
		if a.Key == key || a.Namespace+":"+a.Key == key {
			return a.Val
		}
	}
	return ""
}

func getMacroParam(macro *html.Node, name string) string {
	for c := firstChildElem(macro); c != nil; c = nextSiblingElem(c) {
		if strings.ToLower(c.Data) != "ac:parameter" {
			continue
		}
		if getAttr(c, "ac:name") == name {
			return strings.TrimSpace(textOf(c))
		}
	}
	return ""
}

func findChild(parent *html.Node, localName string) *html.Node {
	for c := firstChildElem(parent); c != nil; c = nextSiblingElem(c) {
		if strings.EqualFold(c.Data, localName) {
			return c
		}
	}
	return nil
}

func firstChildElem(n *html.Node) *html.Node {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode {
			return c
		}
	}
	return nil
}

func nextSiblingElem(n *html.Node) *html.Node {
	for c := n.NextSibling; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode {
			return c
		}
	}
	return nil
}

func textOf(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(x *html.Node) {
		if x.Type == html.TextNode {
			b.WriteString(x.Data)
			return
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

// stripToBody finds the rich-text-body child if present so expand/panel
// macros don't bleed <ac:parameter> tags into output. Returns n when no
// such child exists.
func stripToBody(n *html.Node) *html.Node {
	if rich := findChild(n, "ac:rich-text-body"); rich != nil {
		return rich
	}
	return n
}

// collapseNBSP replaces U+00A0 (non-breaking space) with a regular space so
// empty-paragraph detection works and chunking sees normal whitespace.
func collapseNBSP(s string) string {
	return strings.ReplaceAll(s, " ", " ")
}

// escapeText escapes markdown-significant characters only where they'd
// change block structure. Keep it conservative — aggressive escaping
// fights the ts_headline snippet output.
func escapeText(s string) string {
	return s
}

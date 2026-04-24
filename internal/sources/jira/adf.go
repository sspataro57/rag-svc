// Package jira contains the v3 API client, ADF → markdown converter, and
// normalization logic for Jira Cloud issues.
//
// The ADF (Atlassian Document Format) converter walks the node tree the API
// returns for `description` and `comment.body` fields and emits markdown.
// The spec's node table in CLAUDE.md is the source of truth for which nodes
// are handled; unknown nodes recurse their children and emit any text content
// so no data is silently dropped.
package jira

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ADFDocument is the top-level ADF envelope (`type: "doc"`).
type ADFDocument struct {
	Type    string    `json:"type"`
	Version int       `json:"version"`
	Content []ADFNode `json:"content"`
}

// ADFNode is a single node in the ADF tree. Attrs and marks are left as
// json.RawMessage / generic maps because their shape varies per node type;
// the renderer unmarshals them lazily only for nodes it needs.
type ADFNode struct {
	Type    string          `json:"type"`
	Text    string          `json:"text,omitempty"`
	Attrs   json.RawMessage `json:"attrs,omitempty"`
	Marks   []ADFMark       `json:"marks,omitempty"`
	Content []ADFNode       `json:"content,omitempty"`
}

type ADFMark struct {
	Type  string          `json:"type"`
	Attrs json.RawMessage `json:"attrs,omitempty"`
}

// ADFToMarkdown renders an ADF document as markdown. Empty input returns an
// empty string without error.
func ADFToMarkdown(raw []byte) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var doc ADFDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", fmt.Errorf("adf: unmarshal: %w", err)
	}
	return ADFDocumentToMarkdown(&doc), nil
}

// ADFDocumentToMarkdown renders a parsed document. Exported so the
// normalizer can feed already-decoded ADF directly.
func ADFDocumentToMarkdown(doc *ADFDocument) string {
	if doc == nil {
		return ""
	}
	var r renderer
	r.writeNodes(doc.Content, blockCtx{})
	return strings.TrimRight(r.buf.String(), "\n") + "\n"
}

type renderer struct {
	buf strings.Builder
}

// blockCtx carries state that flows through nested block-level rendering,
// like the current list marker sequence for nested lists or the blockquote
// prefix to apply on every line.
type blockCtx struct {
	listStack     []listFrame
	quotePrefixes []string
}

type listFrame struct {
	ordered bool
	index   int // 1-indexed for ordered lists
}

func (r *renderer) writeNodes(nodes []ADFNode, ctx blockCtx) {
	for i, n := range nodes {
		r.writeNode(n, ctx)
		// Block-level separator between sibling blocks: most block nodes
		// emit their own trailing "\n\n" as part of render; the flush at
		// the end trims trailing newlines, so we only need to take care
		// that nodes which DO want tight packing (list items, table
		// cells, inline) don't double-emit. writeNode handles that.
		_ = i
	}
}

func (r *renderer) writeNode(n ADFNode, ctx blockCtx) {
	switch n.Type {
	case "doc":
		r.writeNodes(n.Content, ctx)

	case "paragraph":
		r.writeBlockPrefix(ctx)
		r.writeInline(n.Content, ctx)
		r.buf.WriteString("\n\n")

	case "heading":
		level := intAttr(n.Attrs, "level", 1)
		if level < 1 {
			level = 1
		}
		if level > 6 {
			level = 6
		}
		r.writeBlockPrefix(ctx)
		r.buf.WriteString(strings.Repeat("#", level))
		r.buf.WriteByte(' ')
		r.writeInline(n.Content, ctx)
		r.buf.WriteString("\n\n")

	case "bulletList":
		childCtx := ctx
		childCtx.listStack = append(childCtx.listStack, listFrame{ordered: false})
		r.writeListItems(n.Content, childCtx)
		// After a top-level list, ensure a trailing blank line.
		if len(ctx.listStack) == 0 {
			r.buf.WriteByte('\n')
		}

	case "orderedList":
		childCtx := ctx
		childCtx.listStack = append(childCtx.listStack, listFrame{ordered: true, index: 1})
		r.writeListItems(n.Content, childCtx)
		if len(ctx.listStack) == 0 {
			r.buf.WriteByte('\n')
		}

	case "codeBlock":
		lang := stringAttr(n.Attrs, "language", "")
		r.writeBlockPrefix(ctx)
		r.buf.WriteString("```")
		r.buf.WriteString(lang)
		r.buf.WriteByte('\n')
		// Code blocks contain only text nodes in ADF; concatenate as-is
		// without applying marks.
		for _, c := range n.Content {
			if c.Type == "text" {
				r.buf.WriteString(c.Text)
			}
		}
		r.buf.WriteString("\n```\n\n")

	case "blockquote":
		childCtx := ctx
		childCtx.quotePrefixes = append(childCtx.quotePrefixes, "> ")
		r.writeNodes(n.Content, childCtx)

	case "rule":
		r.writeBlockPrefix(ctx)
		r.buf.WriteString("---\n\n")

	case "panel":
		panelType := stringAttr(n.Attrs, "panelType", "info")
		label := strings.ToUpper(panelType[:1]) + panelType[1:]
		childCtx := ctx
		childCtx.quotePrefixes = append(childCtx.quotePrefixes, "> ")
		// Emit the header as the first quoted line, then render children
		// underneath with the same quote prefix so multi-paragraph panels
		// stay visually grouped.
		r.writeBlockPrefix(childCtx)
		r.buf.WriteString("**")
		r.buf.WriteString(label)
		r.buf.WriteString(":** ")
		// If the first child is a paragraph, inline its content on the
		// same line so the label reads naturally.
		children := n.Content
		if len(children) > 0 && children[0].Type == "paragraph" {
			r.writeInline(children[0].Content, childCtx)
			r.buf.WriteString("\n\n")
			children = children[1:]
		} else {
			r.buf.WriteString("\n\n")
		}
		r.writeNodes(children, childCtx)

	case "table":
		r.writeTable(n, ctx)

	case "mediaSingle", "mediaGroup", "media":
		// Attachments are explicitly out of scope for v1.

	default:
		// Unknown block node: recurse children so text content still
		// ends up in the output.
		r.writeNodes(n.Content, ctx)
	}
}

// writeBlockPrefix emits the blockquote prefix (if any) for the current line.
// Callers invoke it at the start of each block-level emission.
func (r *renderer) writeBlockPrefix(ctx blockCtx) {
	for _, p := range ctx.quotePrefixes {
		r.buf.WriteString(p)
	}
}

func (r *renderer) writeListItems(items []ADFNode, ctx blockCtx) {
	frameIdx := len(ctx.listStack) - 1
	for _, item := range items {
		if item.Type != "listItem" {
			continue
		}
		r.writeBlockPrefix(ctx)
		// Indent for nested lists: 2 spaces per outer level.
		r.buf.WriteString(strings.Repeat("  ", frameIdx))
		if ctx.listStack[frameIdx].ordered {
			fmt.Fprintf(&r.buf, "%d. ", ctx.listStack[frameIdx].index)
			ctx.listStack[frameIdx].index++
		} else {
			r.buf.WriteString("- ")
		}
		// listItem content is a sequence of blocks. We render the first
		// paragraph inline on the marker line, then subsequent blocks on
		// following lines with continuation indent.
		r.writeListItemContent(item.Content, ctx)
	}
}

func (r *renderer) writeListItemContent(nodes []ADFNode, ctx blockCtx) {
	for i, n := range nodes {
		switch {
		case i == 0 && n.Type == "paragraph":
			r.writeInline(n.Content, ctx)
			r.buf.WriteByte('\n')
		default:
			// Subsequent blocks (nested lists, code blocks, more paragraphs)
			// render normally. Nested lists already handle their own prefix.
			r.writeNode(n, ctx)
		}
	}
}

func (r *renderer) writeInline(nodes []ADFNode, _ blockCtx) {
	for _, n := range nodes {
		switch n.Type {
		case "text":
			r.buf.WriteString(applyMarks(n.Text, n.Marks))
		case "hardBreak":
			r.buf.WriteString("  \n")
		case "mention":
			name := stringAttr(n.Attrs, "text", "")
			if name == "" {
				name = stringAttr(n.Attrs, "displayName", "user")
			}
			r.buf.WriteByte('@')
			r.buf.WriteString(strings.TrimPrefix(name, "@"))
		case "emoji":
			short := stringAttr(n.Attrs, "shortName", "")
			text := stringAttr(n.Attrs, "text", "")
			if text != "" {
				r.buf.WriteString(text)
			} else if short != "" {
				r.buf.WriteString(short)
			}
		case "inlineCard", "blockCard":
			url := stringAttr(n.Attrs, "url", "")
			if url != "" {
				fmt.Fprintf(&r.buf, "[%s](%s)", url, url)
			}
		default:
			// Unknown inline node: recurse into its children so text survives.
			r.writeInline(n.Content, blockCtx{})
		}
	}
}

func applyMarks(text string, marks []ADFMark) string {
	if len(marks) == 0 {
		return text
	}
	// Marks nest: link wraps code wraps em wraps strong is a common order.
	// We apply in a canonical order (link → code → strong → em) so output is
	// stable regardless of the order the API returns.
	var (
		hasStrong, hasEm, hasCode bool
		linkHref                  string
	)
	for _, m := range marks {
		switch m.Type {
		case "strong":
			hasStrong = true
		case "em":
			hasEm = true
		case "code":
			hasCode = true
		case "link":
			linkHref = stringAttr(m.Attrs, "href", "")
		}
	}
	out := text
	if hasCode {
		out = "`" + out + "`"
	}
	if hasStrong {
		out = "**" + out + "**"
	}
	if hasEm {
		out = "*" + out + "*"
	}
	if linkHref != "" {
		out = "[" + out + "](" + linkHref + ")"
	}
	return out
}

// writeTable renders an ADF table as a GitHub-flavored markdown pipe table.
// Tables without a header row still get a synthetic separator so the markdown
// parses.
func (r *renderer) writeTable(t ADFNode, ctx blockCtx) {
	var rows [][]string
	var headerRow int = -1
	for ri, row := range t.Content {
		if row.Type != "tableRow" {
			continue
		}
		var cells []string
		isHeader := false
		for _, cell := range row.Content {
			if cell.Type != "tableCell" && cell.Type != "tableHeader" {
				continue
			}
			if cell.Type == "tableHeader" {
				isHeader = true
			}
			var sub renderer
			sub.writeInline(flattenCellContent(cell.Content), ctx)
			cells = append(cells, strings.ReplaceAll(strings.TrimSpace(sub.buf.String()), "|", "\\|"))
		}
		if isHeader && headerRow == -1 {
			headerRow = ri
		}
		rows = append(rows, cells)
	}
	if len(rows) == 0 {
		return
	}
	// If no explicit header was marked, treat the first row as the header.
	if headerRow == -1 {
		headerRow = 0
	}
	r.writeBlockPrefix(ctx)
	writeRow := func(cells []string) {
		r.buf.WriteString("| ")
		r.buf.WriteString(strings.Join(cells, " | "))
		r.buf.WriteString(" |\n")
	}
	writeRow(rows[headerRow])
	r.writeBlockPrefix(ctx)
	sep := make([]string, len(rows[headerRow]))
	for i := range sep {
		sep[i] = "---"
	}
	writeRow(sep)
	for i, row := range rows {
		if i == headerRow {
			continue
		}
		// Pad short rows so the table stays rectangular.
		for len(row) < len(rows[headerRow]) {
			row = append(row, "")
		}
		r.writeBlockPrefix(ctx)
		writeRow(row)
	}
	r.buf.WriteByte('\n')
}

// flattenCellContent extracts the inline nodes from a table cell's block
// children (ADF wraps cell text in paragraph nodes).
func flattenCellContent(cellContent []ADFNode) []ADFNode {
	var out []ADFNode
	for _, c := range cellContent {
		if c.Type == "paragraph" {
			out = append(out, c.Content...)
		} else {
			// Nested block in a cell — flatten by recursing one level.
			out = append(out, flattenCellContent(c.Content)...)
		}
	}
	return out
}

// ---- attribute helpers ----

func stringAttr(attrs json.RawMessage, key, fallback string) string {
	if len(attrs) == 0 {
		return fallback
	}
	var m map[string]any
	if err := json.Unmarshal(attrs, &m); err != nil {
		return fallback
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return fallback
}

func intAttr(attrs json.RawMessage, key string, fallback int) int {
	if len(attrs) == 0 {
		return fallback
	}
	var m map[string]any
	if err := json.Unmarshal(attrs, &m); err != nil {
		return fallback
	}
	if v, ok := m[key].(float64); ok {
		return int(v)
	}
	return fallback
}

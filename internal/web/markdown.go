package web

import (
	"fmt"
	"html"
	"regexp"
	"strings"
)

// lightMarkdown is a minimal safe renderer: paragraphs, inline code,
// bold/italic, triple-backtick code fences, and [^N] footnote refs
// converted to linked superscripts. Any HTML in the input is escaped
// before any rewrites so an LLM that decides to emit raw `<script>` can't.
func lightMarkdown(src string, citationCount int) string {
	escaped := html.EscapeString(src)
	escaped = renderCodeFences(escaped)
	escaped = footnoteRE.ReplaceAllStringFunc(escaped, func(m string) string {
		sub := footnoteRE.FindStringSubmatch(m)
		if sub == nil {
			return m
		}
		n := sub[1]
		// Clamp to actual citation count so a bad LLM can't introduce
		// dangling refs that look like real sources.
		if citationCount > 0 {
			// Parse N; if out of bounds, render as text rather than link.
			if idx := atoi(n); idx < 1 || idx > citationCount {
				return m
			}
		}
		return fmt.Sprintf("<sup><a href=\"#cite-%s\">[%s]</a></sup>", n, n)
	})
	escaped = inlineCodeRE.ReplaceAllString(escaped, "<code>$1</code>")
	escaped = boldRE.ReplaceAllString(escaped, "<strong>$1</strong>")
	escaped = italicRE.ReplaceAllString(escaped, "<em>$1</em>")

	// Split on blank lines into paragraphs; preserve already-rendered
	// <pre>…</pre> blocks by not touching them.
	var out strings.Builder
	for _, block := range strings.Split(escaped, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		if strings.HasPrefix(block, "<pre>") {
			out.WriteString(block)
			out.WriteByte('\n')
			continue
		}
		out.WriteString("<p>")
		out.WriteString(strings.ReplaceAll(block, "\n", "<br>"))
		out.WriteString("</p>\n")
	}
	return out.String()
}

var (
	// Footnote reference tokens: [^1], [^2], etc. The outer brackets were
	// already HTML-escaped to &#91;^N&#93; — match either form so the
	// replacement works before or after escaping.
	footnoteRE = regexp.MustCompile(`\[\^(\d+)\]`)

	// Triple-backtick code fences with optional language.
	codeFenceRE = regexp.MustCompile("(?s)```([a-zA-Z0-9_-]*)\\n(.+?)\\n?```")

	inlineCodeRE = regexp.MustCompile("`([^`\n]+)`")
	boldRE       = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	italicRE     = regexp.MustCompile(`(?:^|[^*])\*([^*\n]+)\*`)
)

func renderCodeFences(s string) string {
	return codeFenceRE.ReplaceAllStringFunc(s, func(m string) string {
		sub := codeFenceRE.FindStringSubmatch(m)
		if sub == nil {
			return m
		}
		// Code contents are already HTML-escaped (we ran html.EscapeString
		// over the whole input first), so this is safe to wrap in <pre>.
		return "<pre><code>" + sub[2] + "</code></pre>"
	})
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

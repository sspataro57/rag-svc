package confluence

import (
	"regexp"
	"strings"
)

// Sentinel token grammar (must stay in sync with storage.go's renderLink).
//
//	⟦pg-id:{id}|{label}⟧
//	⟦pg-title:{space-key}|{title}|{label}⟧
//
// ResolveLinks walks markdown produced by StorageToMarkdown and rewrites
// sentinels to markdown links using the provided maps. Unresolved sentinels
// degrade to their {label} text so retrieval still sees meaningful content.
var (
	pgIDRE    = regexp.MustCompile(`⟦pg-id:([^|⟧]+)\|([^⟧]*)⟧`)
	pgTitleRE = regexp.MustCompile(`⟦pg-title:([^|⟧]*)\|([^|⟧]+)\|([^⟧]*)⟧`)
)

// URLResolver looks up page URLs by numeric id or by (space, title). Both
// lookups return ("", false) when the target isn't indexed in this run.
type URLResolver interface {
	URLByID(id string) (string, bool)
	URLByTitle(spaceKey, title string) (string, bool)
}

// ResolveLinks rewrites sentinel tokens in markdown using r. Tokens for
// pages we haven't indexed (unknown sibling pages) degrade to their label
// text — the link is lost but the semantic word stays in the chunk.
func ResolveLinks(md string, r URLResolver) string {
	out := pgIDRE.ReplaceAllStringFunc(md, func(tok string) string {
		m := pgIDRE.FindStringSubmatch(tok)
		if m == nil {
			return tok
		}
		id, label := m[1], m[2]
		if url, ok := r.URLByID(id); ok {
			return "[" + label + "](" + url + ")"
		}
		return label
	})
	out = pgTitleRE.ReplaceAllStringFunc(out, func(tok string) string {
		m := pgTitleRE.FindStringSubmatch(tok)
		if m == nil {
			return tok
		}
		space, title, label := m[1], m[2], m[3]
		if url, ok := r.URLByTitle(space, title); ok {
			return "[" + label + "](" + url + ")"
		}
		return label
	})
	return out
}

// MapResolver is an in-memory URLResolver backed by maps built during the
// first pass of a sync (and fallback-augmented from the sources table if
// the caller queries for pages outside the current run).
type MapResolver struct {
	ByID    map[string]string
	ByTitle map[string]string // key: spaceKey + "\x00" + title
}

func NewMapResolver() *MapResolver {
	return &MapResolver{ByID: map[string]string{}, ByTitle: map[string]string{}}
}

func (m *MapResolver) Record(id, spaceKey, title, url string) {
	if id != "" {
		m.ByID[id] = url
	}
	if spaceKey != "" && title != "" {
		m.ByTitle[spaceKey+"\x00"+title] = url
	}
}

func (m *MapResolver) URLByID(id string) (string, bool) {
	url, ok := m.ByID[id]
	return url, ok
}

func (m *MapResolver) URLByTitle(spaceKey, title string) (string, bool) {
	// If the caller omitted a space key on the sentinel, try any space with
	// a matching title — Confluence's own editor emits empty space-key for
	// intra-space links, which is the common case.
	if spaceKey == "" {
		suffix := "\x00" + title
		for k, v := range m.ByTitle {
			if strings.HasSuffix(k, suffix) {
				return v, true
			}
		}
		return "", false
	}
	url, ok := m.ByTitle[spaceKey+"\x00"+title]
	return url, ok
}

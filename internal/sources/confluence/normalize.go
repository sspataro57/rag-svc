package confluence

import (
	"fmt"
	"strings"
	"time"
)

// NormalizedPage is the storage-shaped view of a Confluence page. The
// BodyMarkdown field carries sentinel tokens for page-to-page links that
// the ingest orchestrator rewrites once the full (title → URL) map is
// known.
type NormalizedPage struct {
	ID         string
	Title      string
	SpaceKey   string
	Body       string // markdown with link sentinels
	URL        string
	UpdatedAt  time.Time
	Breadcrumb []string // ancestor titles, root first
	ParentID   string
	Extra      map[string]any
}

// BodyMarkdown returns the body with a leading title heading so retrieval
// chunks always have a self-identifying first line. The body's sentinel
// tokens are resolved elsewhere.
func (n *NormalizedPage) BodyMarkdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", n.Title)
	if len(n.Breadcrumb) > 0 {
		b.WriteString("_")
		b.WriteString(strings.Join(n.Breadcrumb, " / "))
		b.WriteString("_\n\n")
	}
	b.WriteString(strings.TrimRight(n.Body, "\n"))
	b.WriteString("\n")
	return b.String()
}

// Normalize produces a NormalizedPage from a raw API page, ancestor titles,
// and the space's key. baseURL must include `/wiki`.
func Normalize(page *Page, spaceKey string, ancestorTitles []string, baseURL string) (*NormalizedPage, error) {
	if page == nil {
		return nil, fmt.Errorf("confluence: normalize nil page")
	}
	body, err := StorageToMarkdown(page.Body.Storage.Value)
	if err != nil {
		return nil, fmt.Errorf("confluence: normalize %s: %w", page.ID, err)
	}

	base := strings.TrimRight(baseURL, "/")
	url := fmt.Sprintf("%s/spaces/%s/pages/%s", base, spaceKey, page.ID)

	updated := page.Version.CreatedAt
	if updated.IsZero() {
		updated = time.Now().UTC()
	}

	extra := map[string]any{
		"parent_id": page.ParentID,
		"space_id":  page.SpaceID,
		"version":   page.Version.Number,
	}
	if len(ancestorTitles) > 0 {
		extra["breadcrumb"] = ancestorTitles
	}

	return &NormalizedPage{
		ID:         page.ID,
		Title:      page.Title,
		SpaceKey:   spaceKey,
		Body:       body,
		URL:        url,
		UpdatedAt:  updated,
		Breadcrumb: ancestorTitles,
		ParentID:   page.ParentID,
		Extra:      extra,
	}, nil
}

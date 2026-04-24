package retrieve

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/treetop/rag-svc/internal/cache"
	"github.com/treetop/rag-svc/internal/embed"
)

// Deps bundles the collaborators SearchHits depends on. Constructing this
// once per process and reusing it keeps connection pools warm.
type Deps struct {
	Pool       *pgxpool.Pool
	Embedder   embed.Embedder
	EmbedCache *cache.EmbedCache // may be nil — retrieval still works, just without cache
	Logger     *slog.Logger

	// Weights default to the CLAUDE.md spec (0.6 / 0.4) when zero.
	VectorWeight float64
	FTSWeight    float64

	// RecencyDecayDays is the half-life used by the recency boost in days.
	// Zero means no boost is applied.
	RecencyDecayDays int
}

const (
	defaultLimit        = 10
	hybridCandidateSize = 50 // per-branch LIMIT inside the CTE
	// defaultRecencyCoefficient is the amplitude of the exp-decay recency
	// bump per CLAUDE.md (`score *= 1 + 0.05 * exp(...)`). We keep it as a
	// package constant rather than surfacing a knob nobody needs in v1.
	defaultRecencyCoefficient = 0.05
)

// SearchHits runs the full retrieval pipeline for q: ticket-key shortcut,
// hybrid vector+FTS CTE, source-level dedup, recency boost, limit.
func SearchHits(ctx context.Context, d Deps, q Query) ([]Hit, error) {
	if d.Pool == nil || d.Embedder == nil {
		return nil, fmt.Errorf("retrieve: missing Pool or Embedder")
	}
	if q.Limit <= 0 {
		q.Limit = defaultLimit
	}

	out := make([]Hit, 0, q.Limit+1)
	seen := make(map[string]struct{}, q.Limit+1)

	// 1. Ticket-key shortcut. If the entire query text is a Jira key, the
	//    matching issue is prepended with score=1.0, then hybrid results
	//    fill the rest.
	if key := ParseTicketKey(q.Text); key != "" {
		hit, ok, err := fetchJiraByKey(ctx, d.Pool, key)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, hit)
			seen[hit.ID] = struct{}{}
		}
	}

	// 2. Hybrid branch. Skip only if the shortcut already satisfied Limit.
	if len(out) >= q.Limit {
		return out, nil
	}
	hybrid, err := d.runHybrid(ctx, q)
	if err != nil {
		return nil, err
	}
	for _, h := range hybrid {
		if _, dup := seen[h.ID]; dup {
			continue
		}
		out = append(out, h)
		seen[h.ID] = struct{}{}
		if len(out) >= q.Limit {
			break
		}
	}
	return out, nil
}

// runHybrid runs the vector+FTS CTE and applies the recency boost + source
// dedup in one pass.
func (d Deps) runHybrid(ctx context.Context, q Query) ([]Hit, error) {
	vec, err := d.getQueryVector(ctx, q.Text)
	if err != nil {
		return nil, err
	}

	sources := nullableSlice(q.Filters.Sources)
	projects := nullableSlice(q.Filters.Projects)
	spaces := nullableSlice(q.Filters.Spaces)
	var updatedAfter any
	if !q.Filters.UpdatedAfter.IsZero() {
		updatedAfter = q.Filters.UpdatedAfter
	}

	vecWeight := d.VectorWeight
	ftsWeight := d.FTSWeight
	if vecWeight == 0 && ftsWeight == 0 {
		vecWeight = 0.6
		ftsWeight = 0.4
	}

	_ = spaces // currently unused; Confluence space-specific filtering lands with step 6.
	rows, err := d.Pool.Query(ctx, hybridSQL,
		formatVector(vec),
		q.Text,
		vecWeight,
		ftsWeight,
		sources,
		projects,
		updatedAfter,
		// Overfetch to survive source-level dedup; the trailing slice in
		// SearchHits enforces q.Limit.
		hybridCandidateSize,
	)
	if err != nil {
		return nil, fmt.Errorf("retrieve: hybrid query: %w", err)
	}
	defer rows.Close()

	hits, err := scanHits(rows)
	if err != nil {
		return nil, err
	}

	// Recency boost (in Go so we can tune coefficient independently of SQL).
	if d.RecencyDecayDays > 0 {
		now := time.Now().UTC()
		for i := range hits {
			ageDays := now.Sub(hits[i].UpdatedAt).Hours() / 24
			if ageDays < 0 {
				ageDays = 0
			}
			hits[i].Score *= 1 + defaultRecencyCoefficient*math.Exp(-ageDays/float64(d.RecencyDecayDays))
		}
	}

	// Source-level dedup: keep the best-scoring chunk per source. The CTE
	// already returns at most one row per source (DISTINCT ON), but
	// defensively re-sort + dedup here so a future SQL change doesn't
	// silently regress.
	hits = dedupeBySource(hits)
	return hits, nil
}

// getQueryVector embeds q with the configured Embedder, going through
// EmbedCache when available.
func (d Deps) getQueryVector(ctx context.Context, text string) ([]float32, error) {
	model := d.Embedder.Model()
	if d.EmbedCache != nil {
		if v, ok, err := d.EmbedCache.Get(ctx, model, text); err == nil && ok {
			return v, nil
		} else if err != nil && d.Logger != nil {
			d.Logger.Warn("retrieve: embed cache get failed", "err", err)
		}
	}
	vecs, err := d.Embedder.Embed(ctx, []string{text})
	if err != nil {
		return nil, fmt.Errorf("retrieve: embed query: %w", err)
	}
	if len(vecs) != 1 {
		return nil, fmt.Errorf("retrieve: expected 1 embedding, got %d", len(vecs))
	}
	if d.EmbedCache != nil {
		if err := d.EmbedCache.Set(ctx, model, text, vecs[0]); err != nil && d.Logger != nil {
			d.Logger.Warn("retrieve: embed cache set failed", "err", err)
		}
	}
	return vecs[0], nil
}

// hybridSQL is the CTE from CLAUDE.md, with DISTINCT ON for source-level
// dedup so we don't need a second pass in Go to pick the winning chunk per
// source.
const hybridSQL = `
WITH vector_hits AS (
  SELECT id AS chunk_id, 1 - (embedding <=> $1::vector) AS v_score
  FROM chunks
  ORDER BY embedding <=> $1::vector
  LIMIT 50
),
fts_hits AS (
  SELECT id AS chunk_id, ts_rank_cd(tsv, q) AS f_score
  FROM chunks, plainto_tsquery('english', $2) q
  WHERE tsv @@ q
  ORDER BY f_score DESC
  LIMIT 50
),
fused AS (
  SELECT COALESCE(v.chunk_id, f.chunk_id) AS chunk_id,
         COALESCE(v.v_score, 0) * $3
           + COALESCE(f.f_score / NULLIF((SELECT MAX(f_score) FROM fts_hits), 0), 0) * $4
           AS score
  FROM vector_hits v FULL OUTER JOIN fts_hits f USING (chunk_id)
),
ranked AS (
  SELECT DISTINCT ON (s.id)
         s.id, s.source_type, s.source_key, s.title, s.url, s.project_or_space,
         s.updated_at, s.extra,
         ts_headline('english', c.content, plainto_tsquery('english', $2),
                     'MaxWords=40, MinWords=20, StartSel=<mark>, StopSel=</mark>') AS snippet,
         f.score
  FROM fused f
  JOIN chunks c ON c.id = f.chunk_id
  JOIN sources s ON s.id = c.source_id
  WHERE ($5::text[] IS NULL OR s.source_type = ANY($5))
    AND ($6::text[] IS NULL OR s.project_or_space = ANY($6))
    AND ($7::timestamptz IS NULL OR s.updated_at > $7)
  ORDER BY s.id, f.score DESC
)
SELECT source_type, source_key, title, url, project_or_space, updated_at, extra, snippet, score
FROM ranked
ORDER BY score DESC
LIMIT $8`

// $1  query vector (as pgvector text literal)
// $2  query text for FTS and ts_headline
// $3  vector weight
// $4  FTS weight
// $5  source_type filter (text[] or NULL)
// $6  project_or_space filter (text[] or NULL)
// $7  updated_after (timestamptz or NULL)
// $8  LIMIT

func scanHits(rows pgx.Rows) ([]Hit, error) {
	var hits []Hit
	for rows.Next() {
		var (
			sourceType, sourceKey, title, url, snippet string
			projectOrSpace                             *string
			updatedAt                                  time.Time
			extra                                      []byte
			score                                      float64
		)
		if err := rows.Scan(&sourceType, &sourceKey, &title, &url, &projectOrSpace, &updatedAt, &extra, &snippet, &score); err != nil {
			return nil, fmt.Errorf("retrieve: scan: %w", err)
		}
		h := Hit{
			ID:        sourceType + ":" + sourceKey,
			Source:    sourceType,
			Title:     title,
			Snippet:   snippet,
			URL:       url,
			UpdatedAt: updatedAt,
			Score:     score,
		}
		if projectOrSpace != nil {
			h.ProjectOrSpace = *projectOrSpace
		}
		if len(extra) > 0 {
			_ = json.Unmarshal(extra, &h.Extra)
		}
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return hits, nil
}

func dedupeBySource(hits []Hit) []Hit {
	if len(hits) < 2 {
		return hits
	}
	best := make(map[string]int, len(hits))
	keep := make([]bool, len(hits))
	for i, h := range hits {
		if j, ok := best[h.ID]; ok {
			if hits[j].Score < h.Score {
				keep[j] = false
				best[h.ID] = i
				keep[i] = true
			}
			continue
		}
		best[h.ID] = i
		keep[i] = true
	}
	out := hits[:0]
	for i, h := range hits {
		if keep[i] {
			out = append(out, h)
		}
	}
	// Preserve descending score ordering post-dedup.
	sortByScoreDesc(out)
	return out
}

func sortByScoreDesc(hs []Hit) {
	for i := 1; i < len(hs); i++ {
		for j := i; j > 0 && hs[j].Score > hs[j-1].Score; j-- {
			hs[j-1], hs[j] = hs[j], hs[j-1]
		}
	}
}

// nullableSlice returns nil (→ SQL NULL) for an empty slice so the
// `$N::text[] IS NULL` guards in the CTE skip the filter. pgx passes a
// non-nil empty []string as an empty array, which would filter everything
// out.
func nullableSlice(s []string) any {
	if len(s) == 0 {
		return nil
	}
	return s
}

// formatVector renders a []float32 as the pgvector text literal "[x,y,z]".
// Duplicated from internal/store to avoid a circular import; identical
// format.
func formatVector(v []float32) string {
	var b strings.Builder
	b.Grow(len(v)*8 + 2)
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%g", f)
	}
	b.WriteByte(']')
	return b.String()
}

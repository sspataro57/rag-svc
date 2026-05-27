package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// SourceRow mirrors the public.sources row layout used during ingestion.
// The store owns no business logic — it just persists whatever the caller
// provides.
type SourceRow struct {
	SourceType     string
	SourceKey      string
	ProjectOrSpace string
	Title          string
	URL            string
	BodyMarkdown   string
	Extra          map[string]any
	UpdatedAt      time.Time
}

// Source is the read shape for GetSourceByKey: the persisted SourceRow plus
// the row's database id and indexed_at, both populated by the server.
type Source struct {
	ID             int64
	SourceType     string
	SourceKey      string
	ProjectOrSpace string
	Title          string
	URL            string
	BodyMarkdown   string
	Extra          map[string]any
	UpdatedAt      time.Time
	IndexedAt      time.Time
}

// GetSourceByKey loads a single source by (source_type, source_key). Returns
// pgx.ErrNoRows when not found — callers translate that into a 404. Used by
// downstream services (job-agent fact verifier) to fetch the full text behind
// a search hit so they can verify claims against it.
func (s *Store) GetSourceByKey(ctx context.Context, sourceType, sourceKey string) (*Source, error) {
	const q = `
SELECT id, source_type, source_key,
       COALESCE(project_or_space, ''), title, url, body_markdown, extra,
       updated_at, indexed_at
FROM sources
WHERE source_type = $1 AND source_key = $2`
	var (
		out       Source
		extraJSON []byte
	)
	err := s.pool.QueryRow(ctx, q, sourceType, sourceKey).Scan(
		&out.ID,
		&out.SourceType,
		&out.SourceKey,
		&out.ProjectOrSpace,
		&out.Title,
		&out.URL,
		&out.BodyMarkdown,
		&extraJSON,
		&out.UpdatedAt,
		&out.IndexedAt,
	)
	if err != nil {
		return nil, err
	}
	if len(extraJSON) > 0 {
		if err := json.Unmarshal(extraJSON, &out.Extra); err != nil {
			return nil, fmt.Errorf("store: unmarshal extra for %s/%s: %w", sourceType, sourceKey, err)
		}
	}
	return &out, nil
}

// ChunkRow mirrors public.chunks minus the generated tsvector column.
type ChunkRow struct {
	ChunkIndex int
	Content    string
	TokenCount int
	Kind       string // 'body' | 'comment' | 'section'
	Embedding  []float32
}

// UpsertSource writes the source row by (source_type, source_key). On
// conflict it updates everything except indexed_at, which defaults to now().
// Returns the source's id.
func (s *Store) UpsertSource(ctx context.Context, row SourceRow) (int64, error) {
	extraJSON, err := json.Marshal(row.Extra)
	if err != nil {
		return 0, fmt.Errorf("store: marshal extra: %w", err)
	}
	const q = `
INSERT INTO sources (source_type, source_key, project_or_space, title, url, body_markdown, extra, updated_at, indexed_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())
ON CONFLICT (source_type, source_key) DO UPDATE SET
    project_or_space = EXCLUDED.project_or_space,
    title            = EXCLUDED.title,
    url              = EXCLUDED.url,
    body_markdown    = EXCLUDED.body_markdown,
    extra            = EXCLUDED.extra,
    updated_at       = EXCLUDED.updated_at,
    indexed_at       = now()
RETURNING id`
	var id int64
	err = s.pool.QueryRow(ctx, q,
		row.SourceType,
		row.SourceKey,
		nullableString(row.ProjectOrSpace),
		row.Title,
		row.URL,
		row.BodyMarkdown,
		extraJSON,
		row.UpdatedAt,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: upsert source %s/%s: %w", row.SourceType, row.SourceKey, err)
	}
	return id, nil
}

// ReplaceChunks atomically deletes existing chunks for sourceID and inserts
// the new set. Used during re-ingestion of an updated source.
func (s *Store) ReplaceChunks(ctx context.Context, sourceID int64, chunks []ChunkRow) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("store: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `DELETE FROM chunks WHERE source_id = $1`, sourceID); err != nil {
		return fmt.Errorf("store: delete chunks for %d: %w", sourceID, err)
	}

	const q = `
INSERT INTO chunks (source_id, chunk_index, content, token_count, embedding, chunk_kind)
VALUES ($1, $2, $3, $4, $5::vector, $6)`
	for _, c := range chunks {
		kind := c.Kind
		if kind == "" {
			kind = "body"
		}
		_, err := tx.Exec(ctx, q,
			sourceID,
			c.ChunkIndex,
			c.Content,
			c.TokenCount,
			formatVector(c.Embedding),
			kind,
		)
		if err != nil {
			return fmt.Errorf("store: insert chunk %d for source %d: %w", c.ChunkIndex, sourceID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit chunks: %w", err)
	}
	return nil
}

// ---- Ingest state (watermarks) ----

// GetWatermark returns the last-synced watermark for sourceType. When no row
// exists, returns zero time (caller treats that as "never synced").
func (s *Store) GetWatermark(ctx context.Context, sourceType string) (time.Time, string, error) {
	const q = `SELECT last_synced_at, cursor FROM ingest_state WHERE source_type = $1`
	var ts time.Time
	var cursor pgtype.Text
	err := s.pool.QueryRow(ctx, q, sourceType).Scan(&ts, &cursor)
	if err == pgx.ErrNoRows {
		return time.Time{}, "", nil
	}
	if err != nil {
		return time.Time{}, "", fmt.Errorf("store: get watermark: %w", err)
	}
	if cursor.Valid {
		return ts, cursor.String, nil
	}
	return ts, "", nil
}

// SetWatermark upserts ingest_state. Pass cursor == "" to clear it.
func (s *Store) SetWatermark(ctx context.Context, sourceType string, lastSynced time.Time, cursor string) error {
	var cur any
	if cursor != "" {
		cur = cursor
	}
	const q = `
INSERT INTO ingest_state (source_type, last_synced_at, cursor)
VALUES ($1, $2, $3)
ON CONFLICT (source_type) DO UPDATE SET
    last_synced_at = EXCLUDED.last_synced_at,
    cursor         = EXCLUDED.cursor`
	if _, err := s.pool.Exec(ctx, q, sourceType, lastSynced, cur); err != nil {
		return fmt.Errorf("store: set watermark: %w", err)
	}
	return nil
}

// ---- helpers ----

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// formatVector renders a []float32 as pgvector's text literal "[x,y,z]".
// We pass this as a plain string parameter and let Postgres cast it via
// `$N::vector` in the INSERT — avoids pulling in the pgvector-go dependency
// just to encode the same text format.
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

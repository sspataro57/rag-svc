// Package ingest orchestrates fetching from an external source, chunking,
// embedding, and persisting. Step 2 implements Jira only.
package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/treetop/rag-svc/internal/chunk"
	"github.com/treetop/rag-svc/internal/embed"
	"github.com/treetop/rag-svc/internal/sources/jira"
	"github.com/treetop/rag-svc/internal/store"
)

const (
	sourceTypeJira = "jira"

	// maxIssuesPerRun is the safety cap from CLAUDE.md — keeps a runaway
	// pagination loop from hammering Jira.
	maxIssuesPerRun = 10000

	// watermarkSlack accounts for clock drift between our ingest process
	// and Jira — query with updated >= watermark - slack so we don't miss
	// issues updated just before the previous run's end timestamp.
	watermarkSlack = 1 * time.Minute
)

// JiraDeps bundles the external collaborators the Jira ingestor depends on.
// Passing them explicitly keeps the orchestrator testable in isolation.
type JiraDeps struct {
	Client   *jira.Client
	Embedder embed.Embedder
	Store    *store.Store
	Logger   *slog.Logger
}

// JiraOptions controls a single ingest run.
type JiraOptions struct {
	BaseURL   string   // Jira base URL for building issue browse links.
	Projects  []string // Empty = no project filter (all projects visible).
	Workers   int
	BatchSize int // embedding batch size
	PageSize  int // JQL page size (<=100 per Atlassian docs)
	// ChunkOpts overrides the default chunker settings; zero values fall
	// back to chunk.JiraOptions defaults (4k tokens, 200 overlap).
	ChunkOpts chunk.JiraOptions
}

func (o JiraOptions) withDefaults() JiraOptions {
	if o.Workers <= 0 {
		o.Workers = 4
	}
	if o.BatchSize <= 0 {
		o.BatchSize = 96
	}
	if o.PageSize <= 0 || o.PageSize > 100 {
		o.PageSize = 100
	}
	return o
}

// JiraResult summarizes what a single run did. Useful for CLI output.
type JiraResult struct {
	IssuesFetched   int
	SourcesUpserted int
	ChunksWritten   int
	Embeddings      int
	StartedAt       time.Time
	FinishedAt      time.Time
	Watermark       time.Time
}

// RunJira performs one incremental Jira sync.
func RunJira(ctx context.Context, deps JiraDeps, opts JiraOptions) (*JiraResult, error) {
	opts = opts.withDefaults()
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}

	start := time.Now().UTC()

	watermark, _, err := deps.Store.GetWatermark(ctx, sourceTypeJira)
	if err != nil {
		return nil, err
	}
	jql := buildJQL(opts.Projects, watermark)
	log.Info("jira ingest: starting",
		"watermark", watermark.Format(time.RFC3339),
		"jql", jql,
		"workers", opts.Workers,
	)

	// Fetch phase: walk all pages into a slice we can fan out to workers.
	// Page size is small (<=100) and the 10k safety cap keeps memory bounded.
	var issues []jira.Issue
	var nextToken string
	for {
		if len(issues) >= maxIssuesPerRun {
			log.Warn("jira ingest: safety cap hit", "cap", maxIssuesPerRun)
			break
		}
		resp, err := deps.Client.SearchJQL(ctx, jira.SearchRequest{
			JQL:           jql,
			Fields:        []string{"summary", "description", "status", "issuetype", "project", "updated", "comment", "labels", "assignee"},
			MaxResults:    opts.PageSize,
			NextPageToken: nextToken,
		})
		if err != nil {
			if jira.IsTokenError(err) {
				// Per spec: restart from the last successful watermark
				// instead of retrying with the bad token.
				log.Warn("jira ingest: stale pagination token, aborting run; next run will restart from watermark", "err", err)
				break
			}
			return nil, err
		}
		issues = append(issues, resp.Issues...)
		if resp.NextPageToken == "" || resp.IsLast {
			break
		}
		nextToken = resp.NextPageToken
	}

	result := &JiraResult{
		IssuesFetched: len(issues),
		StartedAt:     start,
		Watermark:     watermark,
	}

	if len(issues) == 0 {
		// Do NOT advance the watermark on a zero-result run. Atlassian's
		// /search/jql endpoint occasionally returns empty pages for
		// recent updates; advancing here would silently skip past those
		// updates forever (see incident 2026-05: 19-day gap between
		// last indexed_at and the watermark). Leaving the watermark in
		// place means the next run re-scans the same window, which is
		// idempotent and self-healing once the API returns real data.
		result.FinishedAt = time.Now().UTC()
		if !watermark.IsZero() && start.Sub(watermark) > 6*time.Hour {
			log.Warn("jira ingest: no new issues but watermark is stale",
				"watermark", watermark.Format(time.RFC3339),
				"age", start.Sub(watermark).String(),
			)
		} else {
			log.Info("jira ingest: no new issues", "watermark", watermark.Format(time.RFC3339))
		}
		return result, nil
	}

	// Worker pool: each worker consumes an issue from the channel and runs
	// the full normalize → chunk → embed → store pipeline. Because
	// embeddings dominate latency, this scales well with Workers.
	jobs := make(chan jira.Issue)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	var sources, chunks, embeds int

	advanceMax := watermark
	setErr := func(e error) {
		mu.Lock()
		defer mu.Unlock()
		if firstErr == nil {
			firstErr = e
		}
	}

	for w := 0; w < opts.Workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for iss := range jobs {
				if ctx.Err() != nil {
					return
				}
				n, err := processIssue(ctx, deps, opts, iss)
				if err != nil {
					log.Error("jira ingest: issue failed", "key", iss.Key, "err", err)
					setErr(err)
					continue
				}
				mu.Lock()
				sources++
				chunks += n.chunks
				embeds += n.embeddings
				if n.updatedAt.After(advanceMax) {
					advanceMax = n.updatedAt
				}
				mu.Unlock()
			}
		}(w)
	}

	for _, iss := range issues {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return result, ctx.Err()
		case jobs <- iss:
		}
	}
	close(jobs)
	wg.Wait()

	result.SourcesUpserted = sources
	result.ChunksWritten = chunks
	result.Embeddings = embeds
	result.FinishedAt = time.Now().UTC()

	if firstErr != nil {
		// Leave the watermark untouched so the next run re-tries the
		// failed issues — partial progress is recoverable because
		// UpsertSource + ReplaceChunks are idempotent per issue.
		return result, firstErr
	}

	// Advance the watermark to the run's start timestamp. We already
	// included updated >= watermark - slack in the JQL, so this is safe.
	newWatermark := start
	if advanceMax.After(newWatermark) {
		newWatermark = advanceMax
	}
	if err := deps.Store.SetWatermark(ctx, sourceTypeJira, newWatermark, ""); err != nil {
		return result, err
	}
	result.Watermark = newWatermark

	log.Info("jira ingest: done",
		"issues", result.IssuesFetched,
		"sources", result.SourcesUpserted,
		"chunks", result.ChunksWritten,
		"embeddings", result.Embeddings,
		"elapsed", result.FinishedAt.Sub(result.StartedAt).String(),
		"watermark", result.Watermark.Format(time.RFC3339),
	)
	return result, nil
}

type issueStats struct {
	chunks     int
	embeddings int
	updatedAt  time.Time
}

func processIssue(ctx context.Context, deps JiraDeps, opts JiraOptions, iss jira.Issue) (issueStats, error) {
	// Re-fetch the full issue if comments are truncated (spec says ~50).
	if iss.Fields.Comment != nil && iss.Fields.Comment.Total > len(iss.Fields.Comment.Comments) {
		full, err := deps.Client.GetIssue(ctx, iss.Key)
		if err != nil {
			return issueStats{}, fmt.Errorf("refetch %s: %w", iss.Key, err)
		}
		iss = *full
	}

	norm, err := jira.Normalize(&iss, opts.BaseURL)
	if err != nil {
		return issueStats{}, err
	}

	chunks, err := chunk.Jira(norm, opts.ChunkOpts)
	if err != nil {
		return issueStats{}, err
	}

	rows := make([]store.ChunkRow, 0, len(chunks))
	if len(chunks) > 0 {
		// Batch embed to respect EMBED_BATCH_SIZE.
		texts := make([]string, len(chunks))
		for i, c := range chunks {
			texts[i] = c.Content
		}
		vectors, err := embedInBatches(ctx, deps.Embedder, texts, opts.BatchSize)
		if err != nil {
			return issueStats{}, fmt.Errorf("embed %s: %w", iss.Key, err)
		}
		for i, c := range chunks {
			rows = append(rows, store.ChunkRow{
				ChunkIndex: c.Index,
				Content:    c.Content,
				TokenCount: c.TokenCount,
				Kind:       string(c.Kind),
				Embedding:  vectors[i],
			})
		}
	}

	sourceID, err := deps.Store.UpsertSource(ctx, store.SourceRow{
		SourceType:     sourceTypeJira,
		SourceKey:      norm.Key,
		ProjectOrSpace: norm.Project,
		Title:          norm.Title,
		URL:            norm.URL,
		BodyMarkdown:   norm.BodyMarkdown(),
		Extra:          norm.Extra,
		UpdatedAt:      norm.UpdatedAt,
	})
	if err != nil {
		return issueStats{}, err
	}
	if err := deps.Store.ReplaceChunks(ctx, sourceID, rows); err != nil {
		return issueStats{}, err
	}

	return issueStats{
		chunks:     len(rows),
		embeddings: len(rows),
		updatedAt:  norm.UpdatedAt,
	}, nil
}

func embedInBatches(ctx context.Context, e embed.Embedder, texts []string, batchSize int) ([][]float32, error) {
	if batchSize <= 0 {
		batchSize = 96
	}
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += batchSize {
		end := start + batchSize
		if end > len(texts) {
			end = len(texts)
		}
		vecs, err := e.Embed(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		if len(vecs) != end-start {
			return nil, errors.New("ingest: embedder returned wrong vector count")
		}
		out = append(out, vecs...)
	}
	return out, nil
}

// buildJQL composes the incremental-sync query. Always orders by updated ASC
// so we can checkpoint the watermark safely.
func buildJQL(projects []string, watermark time.Time) string {
	var parts []string
	if len(projects) > 0 {
		quoted := make([]string, len(projects))
		for i, p := range projects {
			quoted[i] = fmt.Sprintf("%q", p)
		}
		parts = append(parts, fmt.Sprintf("project in (%s)", strings.Join(quoted, ",")))
	}
	if !watermark.IsZero() {
		t := watermark.Add(-watermarkSlack).UTC()
		// JQL wants minute precision; Atlassian rejects seconds in this
		// position.
		parts = append(parts, fmt.Sprintf(`updated >= "%s"`, t.Format("2006-01-02 15:04")))
	}
	if len(parts) == 0 {
		return "order by updated ASC"
	}
	return strings.Join(parts, " AND ") + " order by updated ASC"
}

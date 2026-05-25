package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/treetop/rag-svc/internal/chunk"
	"github.com/treetop/rag-svc/internal/embed"
	"github.com/treetop/rag-svc/internal/sources/confluence"
	"github.com/treetop/rag-svc/internal/store"
)

const sourceTypeConfluence = "confluence"

// ConfluenceDeps bundles the Confluence ingestor's collaborators.
type ConfluenceDeps struct {
	Client   *confluence.Client
	Embedder embed.Embedder
	Store    *store.Store
	Logger   *slog.Logger
}

// ConfluenceOptions controls a single ingest run.
type ConfluenceOptions struct {
	BaseURL   string
	Spaces    []string // space keys; empty = all spaces visible to the service account (not v1 recommended)
	Workers   int
	BatchSize int // embedding batch size
	ChunkOpts chunk.ConfluenceOptions
}

func (o ConfluenceOptions) withDefaults() ConfluenceOptions {
	if o.Workers <= 0 {
		o.Workers = 4
	}
	if o.BatchSize <= 0 {
		o.BatchSize = 96
	}
	return o
}

// ConfluenceResult summarizes a single run.
type ConfluenceResult struct {
	PagesFetched    int
	SourcesUpserted int
	ChunksWritten   int
	Embeddings      int
	StartedAt       time.Time
	FinishedAt      time.Time
	Watermark       time.Time
}

// RunConfluence performs one incremental Confluence sync.
//
// Flow:
//  1. Resolve space keys → IDs.
//  2. For each space, iterate pages in -modified-date order; stop a space
//     as soon as we hit a page older than the watermark.
//  3. Fetch each page's full body + ancestor IDs in parallel; enrich
//     ancestors with titles (cache-then-bulk). Normalize into
//     NormalizedPage with sentinel link tokens in the body.
//  4. After all pages are staged, build a MapResolver from the collected
//     (id, space, title, url) tuples plus any sibling pages already in
//     sources (so incremental runs can resolve links to pages ingested in
//     earlier runs).
//  5. For each staged page, resolve sentinels → chunk → embed → persist.
//     Advance the watermark.
func RunConfluence(ctx context.Context, deps ConfluenceDeps, opts ConfluenceOptions) (*ConfluenceResult, error) {
	opts = opts.withDefaults()
	log := deps.Logger
	if log == nil {
		log = slog.Default()
	}
	start := time.Now().UTC()

	watermark, _, err := deps.Store.GetWatermark(ctx, sourceTypeConfluence)
	if err != nil {
		return nil, err
	}

	spaceMap, err := deps.Client.ResolveSpaces(ctx, opts.Spaces)
	if err != nil {
		return nil, err
	}
	if len(opts.Spaces) > 0 && len(spaceMap) == 0 {
		return nil, fmt.Errorf("confluence ingest: none of %v resolved to a space", opts.Spaces)
	}
	log.Info("confluence ingest: starting",
		"watermark", watermark.Format(time.RFC3339),
		"spaces_requested", opts.Spaces,
		"spaces_resolved", keysOf(spaceMap),
		"workers", opts.Workers,
	)

	// --- Fetch phase (serial per space, cap by watermark) ---
	type staged struct {
		page     *confluence.Page
		spaceKey string
	}
	var queued []staged
	for key, space := range spaceMap {
		next := ""
		safety := 0
		stopSpace := false
		for !stopSpace {
			if len(queued) >= maxIssuesPerRun {
				log.Warn("confluence ingest: safety cap hit", "cap", maxIssuesPerRun)
				stopSpace = true
				break
			}
			resp, err := deps.Client.ListPagesInSpace(ctx, space.ID, next)
			if err != nil {
				return nil, err
			}
			for _, p := range resp.Results {
				if !watermark.IsZero() && p.Version.CreatedAt.Before(watermark.Add(-watermarkSlack)) {
					stopSpace = true
					break
				}
				queued = append(queued, staged{
					page:     &confluence.Page{ID: p.ID, Title: p.Title, ParentID: p.ParentID, SpaceID: p.SpaceID, Version: p.Version},
					spaceKey: key,
				})
			}
			if resp.Links.Next == "" {
				break
			}
			next = resp.Links.Next
			safety++
			if safety > 200 {
				log.Warn("confluence ingest: pagination safety trip")
				break
			}
		}
	}

	if len(queued) == 0 {
		// Mirror the jira ingestor: don't advance the watermark on a
		// zero-result run. If the v2 list endpoint ever returns empty
		// for a space that does have recent updates, advancing here
		// would skip them permanently.
		if !watermark.IsZero() && start.Sub(watermark) > 6*time.Hour {
			log.Warn("confluence ingest: no new pages but watermark is stale",
				"watermark", watermark.Format(time.RFC3339),
				"age", start.Sub(watermark).String(),
			)
		} else {
			log.Info("confluence ingest: no new pages", "watermark", watermark.Format(time.RFC3339))
		}
		return &ConfluenceResult{StartedAt: start, FinishedAt: time.Now().UTC(), Watermark: watermark}, nil
	}

	// --- Fetch bodies + ancestors in parallel ---
	type normalized struct {
		norm  *confluence.NormalizedPage
		extra map[string]any
		err   error
	}
	type fetched struct {
		page     *confluence.Page
		spaceKey string
	}

	jobs := make(chan fetched)
	resultsCh := make(chan normalized, len(queued))
	var wg sync.WaitGroup
	titleCache := newPageTitleCache()
	// Pre-seed the title cache with titles we already have from the list
	// response — saves a round-trip when ancestors point at siblings in
	// the same sync.
	for _, q := range queued {
		titleCache.put(q.page.ID, q.page.Title)
	}

	for w := 0; w < opts.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range jobs {
				if ctx.Err() != nil {
					return
				}
				full, err := deps.Client.GetPage(ctx, f.page.ID)
				if err != nil {
					resultsCh <- normalized{err: fmt.Errorf("fetch %s: %w", f.page.ID, err)}
					continue
				}
				titleCache.put(full.ID, full.Title)
				ancestorIDs, err := deps.Client.GetAncestors(ctx, f.page.ID)
				if err != nil {
					resultsCh <- normalized{err: fmt.Errorf("ancestors %s: %w", f.page.ID, err)}
					continue
				}
				titles, err := resolveAncestorTitles(ctx, deps.Client, titleCache, ancestorIDs)
				if err != nil {
					resultsCh <- normalized{err: fmt.Errorf("ancestor titles %s: %w", f.page.ID, err)}
					continue
				}
				norm, err := confluence.Normalize(full, f.spaceKey, titles, opts.BaseURL)
				if err != nil {
					resultsCh <- normalized{err: err}
					continue
				}
				resultsCh <- normalized{norm: norm}
			}
		}()
	}
	go func() {
		for _, q := range queued {
			select {
			case <-ctx.Done():
				close(jobs)
				return
			case jobs <- fetched{page: q.page, spaceKey: q.spaceKey}:
			}
		}
		close(jobs)
	}()
	wg.Wait()
	close(resultsCh)

	var normalizedPages []*confluence.NormalizedPage
	var firstErr error
	for r := range resultsCh {
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			log.Error("confluence ingest: page failed", "err", r.err)
			continue
		}
		normalizedPages = append(normalizedPages, r.norm)
	}

	// --- Build URL resolver from newly ingested pages ---
	resolver := confluence.NewMapResolver()
	for _, n := range normalizedPages {
		resolver.Record(n.ID, n.SpaceKey, n.Title, n.URL)
	}

	// --- Resolve link sentinels, chunk, embed, persist ---
	result := &ConfluenceResult{
		PagesFetched: len(queued),
		StartedAt:    start,
		Watermark:    watermark,
	}
	advanceMax := watermark
	for _, n := range normalizedPages {
		if ctx.Err() != nil {
			break
		}
		resolved := confluence.ResolveLinks(n.BodyMarkdown(), resolver)
		chunks, err := chunk.Confluence(n, resolved, opts.ChunkOpts)
		if err != nil {
			log.Error("confluence ingest: chunk failed", "page", n.ID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}

		rows := make([]store.ChunkRow, 0, len(chunks))
		if len(chunks) > 0 {
			texts := make([]string, len(chunks))
			for i, c := range chunks {
				texts[i] = c.Content
			}
			vectors, err := embedInBatches(ctx, deps.Embedder, texts, opts.BatchSize)
			if err != nil {
				log.Error("confluence ingest: embed failed", "page", n.ID, "err", err)
				if firstErr == nil {
					firstErr = err
				}
				continue
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
			SourceType:     sourceTypeConfluence,
			SourceKey:      n.ID,
			ProjectOrSpace: n.SpaceKey,
			Title:          n.Title,
			URL:            n.URL,
			BodyMarkdown:   resolved,
			Extra:          n.Extra,
			UpdatedAt:      n.UpdatedAt,
		})
		if err != nil {
			log.Error("confluence ingest: upsert failed", "page", n.ID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := deps.Store.ReplaceChunks(ctx, sourceID, rows); err != nil {
			log.Error("confluence ingest: chunk replace failed", "page", n.ID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		result.SourcesUpserted++
		result.ChunksWritten += len(rows)
		result.Embeddings += len(rows)
		if n.UpdatedAt.After(advanceMax) {
			advanceMax = n.UpdatedAt
		}
	}
	result.FinishedAt = time.Now().UTC()

	if firstErr != nil {
		return result, firstErr
	}

	newWatermark := start
	if advanceMax.After(newWatermark) {
		newWatermark = advanceMax
	}
	if err := deps.Store.SetWatermark(ctx, sourceTypeConfluence, newWatermark, ""); err != nil {
		return result, err
	}
	result.Watermark = newWatermark

	log.Info("confluence ingest: done",
		"pages", result.PagesFetched,
		"sources", result.SourcesUpserted,
		"chunks", result.ChunksWritten,
		"embeddings", result.Embeddings,
		"elapsed", result.FinishedAt.Sub(result.StartedAt).String(),
		"watermark", result.Watermark.Format(time.RFC3339),
	)
	return result, nil
}

// ---- helpers ----

type pageTitleCache struct {
	mu sync.Mutex
	m  map[string]string
}

func newPageTitleCache() *pageTitleCache { return &pageTitleCache{m: map[string]string{}} }

func (c *pageTitleCache) put(id, title string) {
	c.mu.Lock()
	c.m[id] = title
	c.mu.Unlock()
}
func (c *pageTitleCache) get(id string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[id]
	return v, ok
}

// resolveAncestorTitles enriches ancestor IDs with titles, fetching the
// ones we don't already have in the cache via a single bulk call.
func resolveAncestorTitles(ctx context.Context, c *confluence.Client, cache *pageTitleCache, ids []string) ([]string, error) {
	titles := make([]string, 0, len(ids))
	var missing []string
	for _, id := range ids {
		if _, ok := cache.get(id); !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		pages, err := c.GetPagesByIDs(ctx, missing)
		if err != nil {
			return nil, err
		}
		for _, p := range pages {
			cache.put(p.ID, p.Title)
		}
	}
	for _, id := range ids {
		if t, ok := cache.get(id); ok {
			titles = append(titles, t)
		}
	}
	return titles, nil
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

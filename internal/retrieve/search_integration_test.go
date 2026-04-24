//go:build integration

package retrieve

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/treetop/rag-svc/internal/embed"
	"github.com/treetop/rag-svc/internal/store"
)

// The -tags=integration build tag keeps these out of `go test ./...` by
// default. Run them with:
//   go test -tags=integration ./internal/retrieve/...
//
// The whole file expects Docker to be available.

// testEnv is the shared Postgres container for every test in this file.
type testEnv struct {
	container testcontainers.Container
	pool      *pgxpool.Pool
	dsn       string
}

var env *testEnv

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	env, err = setupEnv(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "setup: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	if env != nil {
		env.pool.Close()
		_ = env.container.Terminate(ctx)
	}
	os.Exit(code)
}

func setupEnv(ctx context.Context) (*testEnv, error) {
	req := testcontainers.ContainerRequest{
		Image:        "pgvector/pgvector:pg16",
		ExposedPorts: []string{"5432/tcp"},
		Env: map[string]string{
			"POSTGRES_USER":     "test",
			"POSTGRES_PASSWORD": "test",
			"POSTGRES_DB":       "test",
		},
		WaitingFor: wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(60 * time.Second),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return nil, fmt.Errorf("start container: %w", err)
	}
	host, err := c.Host(ctx)
	if err != nil {
		return nil, err
	}
	port, err := c.MappedPort(ctx, "5432/tcp")
	if err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("postgres://test:test@%s:%s/test?sslmode=disable", host, port.Port())

	if err := store.Migrate(dsn); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pool: %w", err)
	}
	return &testEnv{container: c, pool: pool, dsn: dsn}, nil
}

// ---- fixture loader ----

type fixtureChunk struct {
	content string
	kind    string
}

type fixtureSource struct {
	sourceType     string
	sourceKey      string
	projectOrSpace string
	title          string
	url            string
	updatedAt      time.Time
	extra          map[string]any
	chunks         []fixtureChunk
}

// loadFixture inserts one source row + its chunks. Uses the deterministic
// fake embedder so the same fixture always produces the same vectors, which
// keeps ranking tests stable.
func (t *testEnv) loadFixture(ctx context.Context, tb testing.TB, f fixtureSource) int64 {
	tb.Helper()
	extra, _ := json.Marshal(f.extra)
	var id int64
	err := t.pool.QueryRow(ctx, `
INSERT INTO sources (source_type, source_key, project_or_space, title, url, body_markdown, extra, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
ON CONFLICT (source_type, source_key) DO UPDATE SET title = EXCLUDED.title
RETURNING id`,
		f.sourceType, f.sourceKey, nullable(f.projectOrSpace), f.title, f.url, joinChunks(f.chunks), extra, f.updatedAt,
	).Scan(&id)
	if err != nil {
		tb.Fatalf("insert source %s: %v", f.sourceKey, err)
	}
	// Clear any existing chunks so re-loads are idempotent.
	if _, err := t.pool.Exec(ctx, `DELETE FROM chunks WHERE source_id = $1`, id); err != nil {
		tb.Fatalf("clear chunks: %v", err)
	}
	fake := embed.NewFake(embed.Dim)
	texts := make([]string, len(f.chunks))
	for i, c := range f.chunks {
		texts[i] = c.content
	}
	vecs, _ := fake.Embed(ctx, texts)
	for i, c := range f.chunks {
		if _, err := t.pool.Exec(ctx, `
INSERT INTO chunks (source_id, chunk_index, content, token_count, embedding, chunk_kind)
VALUES ($1,$2,$3,$4,$5::vector,$6)`,
			id, i, c.content, len(strings.Fields(c.content)), formatVector(vecs[i]), c.kind,
		); err != nil {
			tb.Fatalf("insert chunk %d: %v", i, err)
		}
	}
	return id
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func joinChunks(cs []fixtureChunk) string {
	var b strings.Builder
	for _, c := range cs {
		b.WriteString(c.content)
		b.WriteString("\n\n")
	}
	return b.String()
}

func (t *testEnv) truncate(ctx context.Context, tb testing.TB) {
	tb.Helper()
	if _, err := t.pool.Exec(ctx, `TRUNCATE chunks, sources RESTART IDENTITY CASCADE`); err != nil {
		tb.Fatal(err)
	}
}

// ---- tests ----

func TestSearch_TicketKeyShortcutPrependsExactMatch(t *testing.T) {
	ctx := context.Background()
	env.truncate(ctx, t)

	// Load a Jira issue with key PLAT-1 plus an unrelated issue.
	env.loadFixture(ctx, t, fixtureSource{
		sourceType: "jira", sourceKey: "PLAT-1",
		projectOrSpace: "PLAT",
		title:          "Credential rotation runbook",
		url:            "https://x.jira.com/browse/PLAT-1",
		updatedAt:      time.Now().Add(-24 * time.Hour),
		chunks:         []fixtureChunk{{content: "How to rotate credentials in production.", kind: "body"}},
	})
	env.loadFixture(ctx, t, fixtureSource{
		sourceType: "jira", sourceKey: "OPS-42",
		projectOrSpace: "OPS",
		title:          "Unrelated ticket",
		url:            "https://x.jira.com/browse/OPS-42",
		updatedAt:      time.Now().Add(-48 * time.Hour),
		chunks:         []fixtureChunk{{content: "something else entirely.", kind: "body"}},
	})

	hits, err := SearchHits(ctx, Deps{
		Pool:     env.pool,
		Embedder: embed.NewFake(embed.Dim),
	}, Query{Text: "PLAT-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("expected at least one hit")
	}
	if hits[0].ID != "jira:PLAT-1" {
		t.Errorf("expected PLAT-1 first, got %q", hits[0].ID)
	}
	if hits[0].Score != 1.0 {
		t.Errorf("ticket-key hit score: got %v want 1.0", hits[0].Score)
	}
	// No duplicate of PLAT-1 in the tail.
	for _, h := range hits[1:] {
		if h.ID == hits[0].ID {
			t.Errorf("duplicate ticket-key hit in tail: %+v", h)
		}
	}
}

func TestSearch_HybridReturnsVectorMatches(t *testing.T) {
	ctx := context.Background()
	env.truncate(ctx, t)

	// Three distinct issues. Use distinct content so the fake-embedder's
	// hash-based vectors stay apart; the test then queries by a chunk's
	// own content and expects the winning hit to be that chunk's source.
	env.loadFixture(ctx, t, fixtureSource{
		sourceType: "jira", sourceKey: "PLAT-100",
		projectOrSpace: "PLAT", title: "Runbook A", url: "http://x/a",
		updatedAt: time.Now(),
		chunks:    []fixtureChunk{{content: "alpha unique content about pineapples.", kind: "body"}},
	})
	env.loadFixture(ctx, t, fixtureSource{
		sourceType: "jira", sourceKey: "PLAT-200",
		projectOrSpace: "PLAT", title: "Runbook B", url: "http://x/b",
		updatedAt: time.Now(),
		chunks:    []fixtureChunk{{content: "beta unrelated content about bananas.", kind: "body"}},
	})
	env.loadFixture(ctx, t, fixtureSource{
		sourceType: "jira", sourceKey: "PLAT-300",
		projectOrSpace: "PLAT", title: "Runbook C", url: "http://x/c",
		updatedAt: time.Now(),
		chunks:    []fixtureChunk{{content: "gamma random content about carrots.", kind: "body"}},
	})

	hits, err := SearchHits(ctx, Deps{
		Pool:     env.pool,
		Embedder: embed.NewFake(embed.Dim),
	}, Query{Text: "alpha unique content about pineapples.", Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	if hits[0].ID != "jira:PLAT-100" {
		t.Errorf("top hit: got %q want jira:PLAT-100 (hits=%+v)", hits[0].ID, hits)
	}
}

func TestSearch_SourceFilterNarrowsResults(t *testing.T) {
	ctx := context.Background()
	env.truncate(ctx, t)

	env.loadFixture(ctx, t, fixtureSource{
		sourceType: "jira", sourceKey: "PLAT-1",
		projectOrSpace: "PLAT", title: "Jira ticket", url: "http://j/1",
		updatedAt: time.Now(),
		chunks:    []fixtureChunk{{content: "keyword shared between sources", kind: "body"}},
	})
	env.loadFixture(ctx, t, fixtureSource{
		sourceType: "document", sourceKey: "doc-abc",
		title:     "A document",
		url:       "http://d/abc",
		updatedAt: time.Now(),
		chunks:    []fixtureChunk{{content: "keyword shared between sources", kind: "body"}},
	})

	hits, err := SearchHits(ctx, Deps{
		Pool:     env.pool,
		Embedder: embed.NewFake(embed.Dim),
	}, Query{
		Text:    "keyword shared between sources",
		Filters: Filters{Sources: []string{"jira"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.Source != "jira" {
			t.Errorf("source filter leaked: %+v", h)
		}
	}
}

func TestSearch_UpdatedAfterFilter(t *testing.T) {
	ctx := context.Background()
	env.truncate(ctx, t)

	now := time.Now().UTC()
	env.loadFixture(ctx, t, fixtureSource{
		sourceType: "jira", sourceKey: "PLAT-OLD",
		projectOrSpace: "PLAT", title: "Old", url: "http://x/old",
		updatedAt: now.Add(-30 * 24 * time.Hour),
		chunks:    []fixtureChunk{{content: "common keyword", kind: "body"}},
	})
	env.loadFixture(ctx, t, fixtureSource{
		sourceType: "jira", sourceKey: "PLAT-NEW",
		projectOrSpace: "PLAT", title: "New", url: "http://x/new",
		updatedAt: now.Add(-1 * 24 * time.Hour),
		chunks:    []fixtureChunk{{content: "common keyword", kind: "body"}},
	})

	hits, err := SearchHits(ctx, Deps{
		Pool:     env.pool,
		Embedder: embed.NewFake(embed.Dim),
	}, Query{
		Text:    "common keyword",
		Filters: Filters{UpdatedAfter: now.Add(-7 * 24 * time.Hour)},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.ID == "jira:PLAT-OLD" {
			t.Errorf("old source leaked past UpdatedAfter filter")
		}
	}
}

func TestSearch_SnippetHasMarkTags(t *testing.T) {
	ctx := context.Background()
	env.truncate(ctx, t)

	env.loadFixture(ctx, t, fixtureSource{
		sourceType: "jira", sourceKey: "PLAT-SNIP",
		projectOrSpace: "PLAT", title: "Snippet test", url: "http://x/s",
		updatedAt: time.Now(),
		chunks: []fixtureChunk{{
			content: "This paragraph discusses credential rotation and the operational runbook to follow when rotating them.",
			kind:    "body",
		}},
	})

	hits, err := SearchHits(ctx, Deps{
		Pool:     env.pool,
		Embedder: embed.NewFake(embed.Dim),
	}, Query{Text: "credential rotation"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("expected at least one hit")
	}
	if !strings.Contains(hits[0].Snippet, "<mark>") {
		t.Errorf("snippet missing <mark>: %q", hits[0].Snippet)
	}
}

func TestSearch_RecencyBoostPrefersNewer(t *testing.T) {
	ctx := context.Background()
	env.truncate(ctx, t)

	now := time.Now().UTC()
	// Two issues with identical content so vector/FTS scores tie; only the
	// recency boost should separate them.
	env.loadFixture(ctx, t, fixtureSource{
		sourceType: "jira", sourceKey: "OLDIE",
		projectOrSpace: "X", title: "Old", url: "http://x/old",
		updatedAt: now.Add(-365 * 24 * time.Hour),
		chunks:    []fixtureChunk{{content: "identical body text for tie breaking", kind: "body"}},
	})
	env.loadFixture(ctx, t, fixtureSource{
		sourceType: "jira", sourceKey: "NEWY",
		projectOrSpace: "X", title: "New", url: "http://x/new",
		updatedAt: now.Add(-1 * time.Hour),
		chunks:    []fixtureChunk{{content: "identical body text for tie breaking", kind: "body"}},
	})

	hits, err := SearchHits(ctx, Deps{
		Pool:             env.pool,
		Embedder:         embed.NewFake(embed.Dim),
		RecencyDecayDays: 180,
	}, Query{Text: "identical body text for tie breaking", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 2 {
		t.Fatalf("want 2 hits, got %d", len(hits))
	}
	if hits[0].ID != "jira:NEWY" {
		t.Errorf("recency boost did not prefer newer: order=%q,%q", hits[0].ID, hits[1].ID)
	}
}

// silence unused when logger isn't wired.
var _ = slog.Default

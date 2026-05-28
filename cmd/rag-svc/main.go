package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/treetop/rag-svc/internal/answer"
	"github.com/treetop/rag-svc/internal/auth"
	"github.com/treetop/rag-svc/internal/blob"
	"github.com/treetop/rag-svc/internal/cache"
	"github.com/treetop/rag-svc/internal/chunk"
	"github.com/treetop/rag-svc/internal/config"
	"github.com/treetop/rag-svc/internal/embed"
	raghttp "github.com/treetop/rag-svc/internal/http"
	"github.com/treetop/rag-svc/internal/ingest"
	"github.com/treetop/rag-svc/internal/mcp"
	"github.com/treetop/rag-svc/internal/retrieve"
	"github.com/treetop/rag-svc/internal/sources/confluence"
	"github.com/treetop/rag-svc/internal/sources/document"
	"github.com/treetop/rag-svc/internal/sources/jira"
	"github.com/treetop/rag-svc/internal/store"
	"github.com/treetop/rag-svc/internal/web"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		// Cobra already prints the error; just exit non-zero.
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "rag-svc",
		Short:         "Retrieval-augmented generation service for Treetop Atlassian content",
		SilenceUsage:  true,
		SilenceErrors: false,
	}

	root.AddCommand(
		newServeCmd(),
		newMigrateCmd(),
		newIngestCmd(),
		newReindexCmd(),
		newTokenCmd(),
	)
	return root
}

// ---- serve ----

func newServeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the HTTP server (search, chat, upload, MCP, Slack, web UI)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, logger, err := bootstrap()
			if err != nil {
				return err
			}

			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			st, err := store.New(ctx, cfg.Core.DatabaseURL)
			if err != nil {
				return err
			}
			defer st.Close()

			rdb, err := cache.NewClient(ctx, cfg.Core)
			if err != nil {
				return err
			}
			defer rdb.Close()

			embedder := buildEmbedder(cfg, logger)
			embedCache := cache.NewEmbedCache(rdb, cfg.Search.QueryCacheTTL, embed.Dim, cfg.Core.RedisKeyPrefix)
			retrievalDeps := &retrieve.Deps{
				Pool:             st.Pool(),
				Embedder:         embedder,
				EmbedCache:       embedCache,
				Logger:           logger,
				VectorWeight:     cfg.Search.VectorWeight,
				FTSWeight:        cfg.Search.FTSWeight,
				RecencyDecayDays: cfg.Search.RecencyDecayDays,
			}

			blobClient, err := blob.New(ctx, cfg.Core)
			if err != nil {
				// Blob storage isn't required for /search — warn and
				// start the server without /upload rather than failing
				// the whole process. Prod deployments must set blob env.
				logger.Warn("blob storage unavailable; /upload will return 503", "err", err)
			}

			// Optional: real OIDC when OIDC_ISSUER is set. Otherwise the
			// server stays in stub mode with /dev/login.
			var oidcValidator *auth.OIDC
			if cfg.Auth.OIDCIssuer != "" && cfg.Auth.OIDCIssuer != "CHANGEME" {
				o, err := auth.NewOIDC(ctx, auth.OIDCConfig{
					Issuer:       cfg.Auth.OIDCIssuer,
					ClientID:     cfg.Auth.OIDCClientID,
					ClientSecret: cfg.Auth.OIDCClientSecret,
					RedirectURL:  cfg.Auth.OIDCRedirectURL,
					CookieName:   cfg.Auth.OIDCSessionCookie,
				})
				if err != nil {
					return err // OIDC configured but misconfigured — fail loud
				}
				oidcValidator = o
			}

			answerer := &answer.Answerer{
				Retrieval: *retrievalDeps,
				BaseURL:   cfg.LLM.AnswerBaseURL,
				APIKey:    cfg.LLM.AnswerAPIKey,
				Model:     cfg.LLM.AnswerModel,
			}
			webHandler, err := web.NewHandler(web.Deps{
				Answerer: answerer,
				Store:    st,
				Blob:     blobClient,
				OIDC:     oidcValidator,
				Logger:   logger,
				StubMode: oidcValidator == nil,
			})
			if err != nil {
				return err
			}

			mcpSrv := mcp.NewServer(mcp.ServerInfo{
				Name:    "rag-svc",
				Version: "0.1.0",
			}, mcp.BuiltinTools(mcp.ToolDeps{
				Retrieval: *retrievalDeps,
				Answerer:  answerer,
				Store:     st,
			}), logger)

			srv := raghttp.NewServer(cfg, st, rdb, logger).
				WithRetrieval(retrievalDeps).
				WithWeb(webHandler).
				WithMCP(mcpSrv).
				WithIngest(ingest.JiraDeps{
					Embedder: embedder,
					Store:    st,
					Logger:   logger,
				}, ingest.JiraOptions{
					BatchSize: cfg.LLM.EmbedBatchSize,
				})
			if blobClient != nil {
				srv = srv.WithBlob(blobClient)
			}
			if oidcValidator != nil {
				srv = srv.WithOIDC(oidcValidator)
			}
			return srv.Run(ctx)
		},
	}
}

// ---- migrate ----

func newMigrateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "Apply database migrations",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, logger, err := bootstrap()
			if err != nil {
				return err
			}
			logger.Info("applying migrations")
			if err := store.Migrate(cfg.Core.DatabaseURL); err != nil {
				return err
			}
			logger.Info("migrations applied")
			return nil
		},
	}
}

// ---- ingest ----

func newIngestCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ingest",
		Short: "Ingest content from an external source",
	}
	c.AddCommand(
		newIngestJiraCmd(),
		newIngestConfluenceCmd(),
		newIngestDocsCmd(),
	)
	return c
}

func newIngestDocsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "docs",
		Short: "Re-chunk and re-embed stored documents (no re-extraction)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, logger, err := bootstrap()
			if err != nil {
				return err
			}
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			st, err := store.New(ctx, cfg.Core.DatabaseURL)
			if err != nil {
				return err
			}
			defer st.Close()
			embedder := buildEmbedder(cfg, logger)

			// Iterate document sources in batches. The body text was
			// normalized at upload time and is stored in body_markdown, so
			// re-embedding doesn't need S3 access.
			rows, err := st.Pool().Query(ctx, `SELECT id, title, body_markdown, extra FROM sources WHERE source_type = 'document' ORDER BY id`)
			if err != nil {
				return err
			}
			defer rows.Close()

			type job struct {
				id    int64
				title string
				body  string
				extra map[string]any
			}
			var jobs []job
			for rows.Next() {
				var j job
				var extraBytes []byte
				if err := rows.Scan(&j.id, &j.title, &j.body, &extraBytes); err != nil {
					return err
				}
				_ = extraBytes // we don't need to unmarshal for re-chunking
				jobs = append(jobs, j)
			}
			if err := rows.Err(); err != nil {
				return err
			}

			logger.Info("ingest docs: starting", "sources", len(jobs))
			sources, chunksOut, embeddings := 0, 0, 0
			for _, j := range jobs {
				norm := &document.NormalizedDocument{
					Title: j.title,
					Body:  j.body,
				}
				chunks, err := chunk.Document(norm, chunk.DocumentOptions{})
				if err != nil {
					logger.Error("ingest docs: chunk failed", "source_id", j.id, "err", err)
					continue
				}
				if len(chunks) == 0 {
					continue
				}
				texts := make([]string, len(chunks))
				for i, c := range chunks {
					texts[i] = c.Content
				}
				vectors, err := embedTexts(ctx, embedder, texts, cfg.LLM.EmbedBatchSize)
				if err != nil {
					logger.Error("ingest docs: embed failed", "source_id", j.id, "err", err)
					continue
				}
				storeRows := make([]store.ChunkRow, 0, len(chunks))
				for i, c := range chunks {
					storeRows = append(storeRows, store.ChunkRow{
						ChunkIndex: c.Index,
						Content:    c.Content,
						TokenCount: c.TokenCount,
						Kind:       string(c.Kind),
						Embedding:  vectors[i],
					})
				}
				if err := st.ReplaceChunks(ctx, j.id, storeRows); err != nil {
					logger.Error("ingest docs: replace chunks failed", "source_id", j.id, "err", err)
					continue
				}
				sources++
				chunksOut += len(storeRows)
				embeddings += len(storeRows)
			}
			logger.Info("ingest docs: done", "sources", sources, "chunks", chunksOut, "embeddings", embeddings)
			return nil
		},
	}
}

// embedTexts batches through the embedder, identical to the helper used
// inside ingest/http packages — duplicated here to keep the CLI's
// dependency surface explicit.
func embedTexts(ctx context.Context, e embed.Embedder, texts []string, batchSize int) ([][]float32, error) {
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
		out = append(out, vecs...)
	}
	return out, nil
}

func newIngestConfluenceCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "confluence",
		Short: "Incrementally sync Confluence pages",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, logger, err := bootstrap()
			if err != nil {
				return err
			}
			if err := requireConfluence(cfg); err != nil {
				return err
			}

			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			st, err := store.New(ctx, cfg.Core.DatabaseURL)
			if err != nil {
				return err
			}
			defer st.Close()

			client := confluence.New(confluence.Options{
				BaseURL: cfg.Confluence.BaseURL,
				Email:   cfg.Confluence.Email,
				Token:   cfg.Confluence.Token,
			})
			embedder := buildEmbedder(cfg, logger)

			res, err := ingest.RunConfluence(ctx, ingest.ConfluenceDeps{
				Client:   client,
				Embedder: embedder,
				Store:    st,
				Logger:   logger,
			}, ingest.ConfluenceOptions{
				BaseURL:   cfg.Confluence.BaseURL,
				Spaces:    cfg.Confluence.Spaces,
				Workers:   cfg.Confluence.Workers,
				BatchSize: cfg.LLM.EmbedBatchSize,
			})
			if err != nil {
				return err
			}
			logger.Info("ingest confluence finished",
				"pages", res.PagesFetched,
				"sources_upserted", res.SourcesUpserted,
				"chunks", res.ChunksWritten,
				"embeddings", res.Embeddings,
				"watermark", res.Watermark,
			)
			return nil
		},
	}
}

func requireConfluence(cfg *config.Config) error {
	var missing []string
	if cfg.Confluence.BaseURL == "" {
		missing = append(missing, "CONFLUENCE_BASE_URL")
	}
	if cfg.Confluence.Email == "" {
		missing = append(missing, "CONFLUENCE_EMAIL")
	}
	if cfg.Confluence.Token == "" {
		missing = append(missing, "CONFLUENCE_TOKEN")
	}
	if len(cfg.Confluence.Spaces) == 0 {
		// Without a space allow-list we'd crawl every space visible to the
		// service account — refuse rather than accidentally hit huge ones.
		missing = append(missing, "CONFLUENCE_SPACES")
	}
	if len(missing) > 0 {
		return fmt.Errorf("ingest confluence: missing required env: %s", strings.Join(missing, ", "))
	}
	return nil
}

func newIngestJiraCmd() *cobra.Command {
	var rebuildLinks bool
	cmd := &cobra.Command{
		Use:   "jira",
		Short: "Incrementally sync Jira issues",
		Long: "By default runs an incremental sync against Jira from the stored " +
			"watermark. Pass --rebuild-links to instead walk every indexed Jira " +
			"source, refetch the issue, and rewrite only its source_links rows " +
			"(no re-chunk, no re-embed) — used after the source_links schema " +
			"migration to backfill historical edges.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, logger, err := bootstrap()
			if err != nil {
				return err
			}
			if err := requireJira(cfg); err != nil {
				return err
			}

			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			st, err := store.New(ctx, cfg.Core.DatabaseURL)
			if err != nil {
				return err
			}
			defer st.Close()

			client := jira.New(jira.Options{
				BaseURL: cfg.Jira.BaseURL,
				Email:   cfg.Jira.Email,
				Token:   cfg.Jira.Token,
			})

			deps := ingest.JiraDeps{
				Client: client,
				Store:  st,
				Logger: logger,
			}
			opts := ingest.JiraOptions{
				BaseURL:   cfg.Jira.BaseURL,
				Projects:  cfg.Jira.Projects,
				Workers:   cfg.Jira.Workers,
				BatchSize: cfg.LLM.EmbedBatchSize,
			}

			if rebuildLinks {
				res, err := ingest.RebuildJiraLinks(ctx, deps, opts)
				if err != nil {
					return err
				}
				logger.Info("rebuild-links finished",
					"processed", res.SourcesProcessed,
					"links_written", res.LinksWritten,
					"skipped", res.Skipped,
					"failed", res.Failed,
					"elapsed", res.FinishedAt.Sub(res.StartedAt).String(),
				)
				return nil
			}

			deps.Embedder = buildEmbedder(cfg, logger)
			res, err := ingest.RunJira(ctx, deps, opts)
			if err != nil {
				return err
			}
			logger.Info("ingest jira finished",
				"issues", res.IssuesFetched,
				"sources_upserted", res.SourcesUpserted,
				"chunks", res.ChunksWritten,
				"embeddings", res.Embeddings,
				"watermark", res.Watermark,
			)
			return nil
		},
	}
	cmd.Flags().BoolVar(&rebuildLinks, "rebuild-links", false,
		"refetch every indexed Jira source and rewrite only its source_links rows; skips chunking/embedding")
	return cmd
}

func requireJira(cfg *config.Config) error {
	var missing []string
	if cfg.Jira.BaseURL == "" {
		missing = append(missing, "JIRA_BASE_URL")
	}
	if cfg.Jira.Email == "" {
		missing = append(missing, "JIRA_EMAIL")
	}
	if cfg.Jira.Token == "" {
		missing = append(missing, "JIRA_TOKEN")
	}
	if len(missing) > 0 {
		return fmt.Errorf("ingest jira: missing required env: %s", strings.Join(missing, ", "))
	}
	return nil
}

// buildEmbedder returns a real OpenAI-compatible client when EMBED_API_KEY is
// set, and the deterministic fake otherwise. This keeps `ingest jira`
// runnable against a fresh dev DB without touching paid APIs — and when a
// real key is present, vectors match what step 3's retriever will see.
func buildEmbedder(cfg *config.Config, logger *slog.Logger) embed.Embedder {
	if cfg.LLM.EmbedAPIKey == "" || cfg.LLM.EmbedAPIKey == "CHANGEME" {
		logger.Warn("EMBED_API_KEY is unset — using deterministic fake embedder (vectors are hash-derived)")
		return embed.NewFake(embed.Dim)
	}
	return embed.New(embed.Options{
		BaseURL:   cfg.LLM.EmbedBaseURL,
		APIKey:    cfg.LLM.EmbedAPIKey,
		Model:     cfg.LLM.EmbedModel,
		Dim:       embed.Dim,
		BatchSize: cfg.LLM.EmbedBatchSize,
	})
}

// ---- reindex (stub) ----

func newReindexCmd() *cobra.Command {
	return stubCmd("reindex", "Drop and rebuild embeddings from stored source content")
}

// ---- token ----

func newTokenCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "token",
		Short: "Manage MCP bearer tokens",
	}
	c.AddCommand(newTokenCreateCmd(), newTokenListCmd(), newTokenRevokeCmd())
	return c
}

func newTokenCreateCmd() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "create --name NAME",
		Short: "Issue a new bearer token. The raw token prints once — copy it.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			cfg, logger, err := bootstrap()
			if err != nil {
				return err
			}
			_ = logger
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			st, err := store.New(ctx, cfg.Core.DatabaseURL)
			if err != nil {
				return err
			}
			defer st.Close()
			raw, tok, err := st.CreateToken(ctx, name)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Token created — copy this value now; it will not be shown again:")
			fmt.Fprintln(cmd.OutOrStdout())
			fmt.Fprintln(cmd.OutOrStdout(), "  "+raw)
			fmt.Fprintln(cmd.OutOrStdout())
			fmt.Fprintf(cmd.OutOrStdout(), "id=%s  name=%q  hash_prefix=%s\n", tok.ID, tok.Name, tok.HashPrefix)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Human-readable token name (required).")
	return cmd
}

func newTokenListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List bearer tokens (active and revoked)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, _, err := bootstrap()
			if err != nil {
				return err
			}
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			st, err := store.New(ctx, cfg.Core.DatabaseURL)
			if err != nil {
				return err
			}
			defer st.Close()
			tokens, err := st.ListTokens(ctx)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "%-36s  %-8s  %-24s  %-24s  %-24s  %s\n",
				"ID", "PREFIX", "NAME", "CREATED", "LAST USED", "STATUS")
			for _, t := range tokens {
				status := "active"
				if t.RevokedAt != nil {
					status = "revoked"
				}
				lastUsed := "-"
				if t.LastUsedAt != nil {
					lastUsed = t.LastUsedAt.UTC().Format(time.RFC3339)
				}
				fmt.Fprintf(w, "%-36s  %-8s  %-24s  %-24s  %-24s  %s\n",
					t.ID, t.HashPrefix, truncateStr(t.Name, 24),
					t.CreatedAt.UTC().Format(time.RFC3339), lastUsed, status)
			}
			return nil
		},
	}
}

func newTokenRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <id-or-prefix>",
		Short: "Revoke a token by full UUID or unambiguous id-prefix.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _, err := bootstrap()
			if err != nil {
				return err
			}
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			st, err := store.New(ctx, cfg.Core.DatabaseURL)
			if err != nil {
				return err
			}
			defer st.Close()
			tok, err := st.RevokeToken(ctx, args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "revoked %s (%s)\n", tok.ID, tok.Name)
			return nil
		},
	}
}

func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

func stubCmd(use, short string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(*cobra.Command, []string) error {
			return fmt.Errorf("%s: not implemented", use)
		},
	}
}

// ---- helpers ----

func bootstrap() (*config.Config, *slog.Logger, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	logger := newLogger(cfg.Core.LogLevel)
	return cfg, logger, nil
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	return slog.New(h)
}

# rag-svc (Treetop)

A retrieval-augmented generation service that indexes Treetop's Atlassian Cloud content (Jira + Confluence) plus uploaded documents, and exposes it to users through four client surfaces: a Chrome extension "supersearch," a web chat UI, a Slack app, and an MCP server for Claude Code.

This deployment is scoped to Treetop only. Avviato runs as a separate `rag-svc` deployment with its own database, its own ingestion, and its own OIDC config. Nothing is shared between instances.

## Goals

1. **Ingest** Jira issues (with comments), Confluence pages, and user-uploaded documents (markdown, text, HTML, PDF).
2. **Embed and store** chunks in Postgres with hybrid search (pgvector + full-text) support.
3. **Serve** four client surfaces from a single binary: `/search`, `/chat`, Slack webhook, MCP over HTTP JSON-RPC.
4. **Stay simple to deploy**: single Go binary, single Docker image, Postgres as the only external infra (plus S3 for uploads and Redis for caching).

Non-goals (v1):
- Attachment, image, or scanned-PDF OCR.
- Office formats (`.docx`, `.xlsx`, `.pptx`). Raw text/markdown/HTML/PDF only.
- Multi-tenant isolation within a single deployment. One `rag-svc` = one Atlassian org.
- Per-user permissions on Atlassian content. The service account's view is what everyone sees.
- Jira/Confluence Server/Data Center. Cloud only. (Confirmed via `/rest/api/2/serverInfo`: `deploymentType: "Cloud"`.)

## Deployment context

Treetop is on Atlassian Cloud with a vanity domain:

- Jira base URL: `https://treetopllc.jira.com`
- Confluence base URL: `https://treetopllc.jira.com/wiki` (same host, `/wiki` subpath — Cloud convention)
- Deployment type: Cloud (confirmed)
- API versions: Jira v3, Confluence v2
- Content formats: ADF for Jira, storage format (XHTML) for Confluence
- Auth: Basic auth with account email + API token (generated at `id.atlassian.com/manage-profile/security/api-tokens`)

Create a dedicated service account user in Atlassian for this, add it only to the projects and spaces that should be indexed, and generate the API token under that account. The service account's permission set is the effective ACL for everyone querying `rag-svc`.

## Architecture

```
                                          ┌────────────────────────────┐
                                          │      Chrome extension       │
                                          │  (Cmd-K supersearch)        │
                                          └────────────┬───────────────┘
                                                       │ GET /search
                                                       ▼
┌──────────────┐   webhook    ┌─────────────────────────────────────────┐
│  Slack app   │─────────────▶│                                          │
└──────────────┘              │                                          │
                              │              rag-svc (Go binary)         │
┌──────────────┐   browser    │                                          │
│  Web chat UI │─────────────▶│   ┌──────────┐  ┌──────────┐  ┌───────┐  │
│ (HTMX+OIDC)  │              │   │ /search  │  │  /chat   │  │  MCP  │  │
└──────────────┘              │   └────┬─────┘  └────┬─────┘  └───┬───┘  │
                              │        │             │            │      │
┌──────────────┐   JSON-RPC   │        ▼             ▼            │      │
│ Claude Code  │─────────────▶│   internal/retrieve (hybrid)      │      │
│  (MCP)       │              │        │                          │      │
└──────────────┘              │        ├──▶ internal/answer ──────┘      │
                              │        │                                  │
                              │        ▼                                  │
                              │   Postgres (pgvector + tsvector)          │
                              │        ▲                                  │
                              │        │ writes                           │
                              │   internal/ingest ◀── internal/sources/* │
                              └─────────────────────────────────────────┘
                                       ▲                   ▲
                                       │                   │
                                 ┌─────┴──────┐     ┌──────┴──────┐
                                 │   S3/MinIO │     │ Atlassian   │
                                 │  (uploads) │     │ Cloud APIs  │
                                 └────────────┘     └─────────────┘
```

## Repository layout

```
rag-svc/
├── cmd/rag-svc/main.go                # cobra subcommands
├── internal/
│   ├── config/                        # env loading, validation
│   ├── store/
│   │   ├── migrations/                # golang-migrate SQL files
│   │   └── store.go                   # pgx pool, repositories
│   ├── embed/                         # OpenAI-compatible embeddings client
│   ├── chunk/                         # chunking strategies per source type
│   ├── blob/                          # S3-compatible object storage
│   ├── sources/
│   │   ├── jira/
│   │   │   ├── client.go              # v3 API client
│   │   │   ├── adf.go                 # ADF → markdown converter
│   │   │   └── normalize.go           # Issue → NormalizedIssue
│   │   ├── confluence/
│   │   │   ├── client.go              # v2 API client
│   │   │   ├── storage.go             # storage-format XHTML → markdown
│   │   │   └── normalize.go
│   │   └── document/
│   │       ├── markdown.go
│   │       ├── html.go
│   │       └── pdf.go
│   ├── ingest/                        # fetch → chunk → embed → store orchestration
│   ├── retrieve/
│   │   ├── search.go                  # SearchHits — used by /search and /chat
│   │   └── rank.go                    # fusion, recency boost, ticket-key shortcut
│   ├── answer/                        # retrieve + generate (shared by web/slack/mcp)
│   ├── auth/
│   │   ├── oidc/                      # OIDC middleware for web UI + extension
│   │   ├── bearer/                    # static bearer tokens for MCP
│   │   └── slack/                     # Slack signing secret + per-workspace OAuth
│   ├── http/
│   │   ├── search.go                  # GET /search handler
│   │   ├── chat.go                    # POST /chat handler (SSE stream)
│   │   ├── upload.go                  # POST /upload handler
│   │   └── projects.go                # GET /projects, /spaces autocomplete
│   ├── web/                           # Go + HTMX chat UI templates & handlers
│   ├── slack/                         # Slack slash command + @mention handlers
│   └── mcp/                           # MCP server (HTTP JSON-RPC), tool handlers
├── extension/                         # Chrome extension (separate npm project)
├── deploy/
│   ├── compose/docker-compose.yml     # local dev
│   └── k8s/
│       ├── base/                      # kustomize base
│       └── overlays/
│           ├── dev/
│           └── prod-treetop/          # secrets, URLs, OIDC for Treetop
├── Dockerfile
├── Makefile
├── .env.example
├── README.md
└── CLAUDE.md                          # this file
```

## Binary subcommands

```
rag-svc serve              # HTTP server: /search, /chat, /upload, /mcp, /slack, web UI
rag-svc ingest jira        # sync Jira issues incrementally (uses updated_after watermark)
rag-svc ingest confluence  # sync Confluence pages incrementally
rag-svc ingest docs        # re-embed uploaded documents (rarely needed)
rag-svc reindex            # drop and rebuild embeddings from stored source content
rag-svc token create       # issue a bearer token for MCP clients
rag-svc token list
rag-svc token revoke <id>
rag-svc migrate            # run DB migrations
```

All subcommands read from the same `.env` / environment. Cobra for the CLI.

## Configuration

All via environment variables. Keep secrets out of logs (`go-redact` or equivalent on the config struct).

### Core

| Var                | Default                 | Purpose                                 |
|--------------------|-------------------------|-----------------------------------------|
| `DATABASE_URL`     | —                       | Postgres connection string. Required.   |
| `REDIS_URL`        | —                       | Redis connection. Required.             |
| `BLOB_ENDPOINT`    | —                       | S3 endpoint. MinIO locally, AWS in prod.|
| `BLOB_BUCKET`      | `rag-svc`               | Bucket name.                            |
| `BLOB_ACCESS_KEY`  | —                       | S3 access key.                          |
| `BLOB_SECRET_KEY`  | —                       | S3 secret key.                          |
| `HTTP_ADDR`        | `:8080`                 | Listen address.                         |
| `LOG_LEVEL`        | `info`                  | `debug` \| `info` \| `warn` \| `error`. |

### LLM providers (OpenAI-compatible)

| Var                  | Default                    | Purpose                                        |
|----------------------|----------------------------|------------------------------------------------|
| `EMBED_BASE_URL`     | `https://api.openai.com/v1`| Embeddings endpoint. Override for Ollama/vLLM. |
| `EMBED_API_KEY`      | —                          | API key for embeddings provider.               |
| `EMBED_MODEL`        | `text-embedding-3-small`   | 1536-dim.                                      |
| `EMBED_BATCH_SIZE`   | `96`                       | Inputs per request.                            |
| `ANSWER_BASE_URL`    | `https://api.openai.com/v1`| Completions endpoint. Override for local.      |
| `ANSWER_API_KEY`     | —                          | API key for completions provider.              |
| `ANSWER_MODEL`       | `gpt-4.1-mini`             | Answer generation model.                       |

### Jira (Treetop Atlassian Cloud)

| Var                    | Example                         | Purpose                                     |
|------------------------|---------------------------------|---------------------------------------------|
| `JIRA_BASE_URL`        | `https://treetopllc.jira.com`   | Treetop Jira base URL.                      |
| `JIRA_EMAIL`           | `rag-svc@treetopllc.com`        | Service account email.                      |
| `JIRA_TOKEN`           | `ATATT3x...`                    | API token (Basic auth password).            |
| `JIRA_PROJECTS`        | `ENG,OPS,PLAT`                  | Comma-separated allow-list. Empty = all.    |
| `JIRA_POLL_INTERVAL`   | `5m`                            | Incremental sync cadence.                   |
| `JIRA_REQUEST_TIMEOUT` | `30s`                           | Per-request timeout.                        |
| `JIRA_WORKERS`         | `4`                             | Concurrent fetch workers.                   |

### Confluence (Treetop Atlassian Cloud)

| Var                          | Example                              | Purpose                           |
|------------------------------|--------------------------------------|-----------------------------------|
| `CONFLUENCE_BASE_URL`        | `https://treetopllc.jira.com/wiki`   | Treetop Confluence base URL.      |
| `CONFLUENCE_EMAIL`           | `rag-svc@treetopllc.com`             | Reuses Jira service account.      |
| `CONFLUENCE_TOKEN`           | `ATATT3x...`                         | Same token works for both.        |
| `CONFLUENCE_SPACES`          | `ENG,DOCS`                           | Space key allow-list. Empty = all.|
| `CONFLUENCE_POLL_INTERVAL`   | `10m`                                | Incremental sync cadence.         |
| `CONFLUENCE_WORKERS`         | `4`                                  | Concurrent fetch workers.         |

### Auth

| Var                      | Example                                                  | Purpose                         |
|--------------------------|----------------------------------------------------------|---------------------------------|
| `OIDC_ISSUER`            | `https://login.treetopllc.com`                           | Treetop corporate IdP.          |
| `OIDC_CLIENT_ID`         | —                                                        |                                 |
| `OIDC_CLIENT_SECRET`     | —                                                        |                                 |
| `OIDC_REDIRECT_URL`      | `https://rag.treetopllc.com/auth/callback`               |                                 |
| `OIDC_SESSION_COOKIE`    | `rag_svc_session`                                        | Cookie name. Shared w/ extension.|
| `SLACK_SIGNING_SECRET`   | —                                                        | For verifying Slack webhooks.   |
| `SLACK_BOT_TOKEN`        | `xoxb-...`                                               | Per-workspace from OAuth install.|
| `EXTENSION_ID`           | `abcd…` (32 chars)                                       | For CORS allow-list.            |

### Search tuning

| Var                           | Default | Purpose                                         |
|-------------------------------|---------|-------------------------------------------------|
| `SEARCH_VECTOR_WEIGHT`        | `0.6`   | Fusion weight for vector similarity.            |
| `SEARCH_FTS_WEIGHT`           | `0.4`   | Fusion weight for BM25.                         |
| `SEARCH_RECENCY_DECAY_DAYS`   | `180`   | Half-life for recency boost.                    |
| `SEARCH_QUERY_CACHE_TTL`      | `300`   | Seconds to cache query embeddings in Redis.     |
| `SEARCH_RATE_LIMIT_PER_USER`  | `30`    | Requests per 10s window per authenticated user. |

## Postgres schema

Migrations live in `internal/store/migrations/`, managed by `golang-migrate`. Run with `rag-svc migrate`.

```sql
-- 0001_init.up.sql

CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- Source documents (parent rows)
CREATE TABLE sources (
    id              BIGSERIAL PRIMARY KEY,
    source_type     TEXT NOT NULL CHECK (source_type IN ('jira', 'confluence', 'document')),
    source_key      TEXT NOT NULL,               -- e.g. "PLAT-482", "123456" (confluence page id), sha256 (document)
    project_or_space TEXT,                       -- "PLAT", "ENG", NULL for documents
    title           TEXT NOT NULL,
    url             TEXT NOT NULL,
    body_markdown   TEXT NOT NULL,               -- normalized content for audit / reindex
    extra           JSONB NOT NULL DEFAULT '{}', -- status, issue_type, ancestry, filename, etc.
    updated_at      TIMESTAMPTZ NOT NULL,        -- source's last-modified
    indexed_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (source_type, source_key)
);

CREATE INDEX sources_project_space_idx ON sources (project_or_space);
CREATE INDEX sources_updated_at_idx ON sources (updated_at DESC);

-- Chunks (one row per embedding)
CREATE TABLE chunks (
    id              BIGSERIAL PRIMARY KEY,
    source_id       BIGINT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    chunk_index     INT NOT NULL,
    content         TEXT NOT NULL,
    token_count     INT NOT NULL,
    embedding       vector(1536) NOT NULL,
    tsv             tsvector GENERATED ALWAYS AS (to_tsvector('english', content)) STORED,
    UNIQUE (source_id, chunk_index)
);

CREATE INDEX chunks_embedding_hnsw ON chunks USING hnsw (embedding vector_cosine_ops);
CREATE INDEX chunks_tsv_gin ON chunks USING gin (tsv);
CREATE INDEX chunks_source_id_idx ON chunks (source_id);

-- Ingestion watermarks
CREATE TABLE ingest_state (
    source_type     TEXT PRIMARY KEY,
    last_synced_at  TIMESTAMPTZ NOT NULL,
    cursor          TEXT                         -- e.g. Atlassian nextPageToken mid-run
);

-- MCP bearer tokens
CREATE TABLE tokens (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name            TEXT NOT NULL,
    token_hash      TEXT NOT NULL UNIQUE,        -- sha256 of the token
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at    TIMESTAMPTZ,
    revoked_at      TIMESTAMPTZ
);
```

HNSW was chosen over IVFFlat because it doesn't need training and handles incremental insert well. For the corpus sizes we're targeting (~100k chunks initially, ~1M long-term), default HNSW params (`m=16`, `ef_construction=64`) are fine. Tune `ef_search` at query time if recall drops.

## Ingestion

### Jira

Client: `internal/sources/jira/client.go`, targeting v3 API at `${JIRA_BASE_URL}/rest/api/3`. Basic auth via `Authorization: Basic base64(email:token)`.

**Search endpoint.** Use `POST /rest/api/3/search/jql` (not the removed `/search`). Request body:

```json
{
  "jql": "project in (ENG, OPS, PLAT) AND updated >= \"2026-04-23 12:00\"",
  "fields": ["summary", "description", "status", "issuetype", "updated", "comment"],
  "expand": ["renderedFields"],
  "maxResults": 100,
  "nextPageToken": "..."
}
```

Pagination is token-based: the response includes `nextPageToken` when more results exist. Loop until absent. Per Atlassian community reports, tokens occasionally return stale errors — on a 4xx token error, restart the search from the last successful watermark rather than retrying the token. Add a safety cap of 10k issues per sync to prevent runaway loops.

**Issue fetch.** `GET /rest/api/3/issue/{key}?expand=renderedFields,comments` when per-issue detail is needed (the search response omits large body fields by default even when requested in `fields`). Incremental ingestion fetches issues returned by the JQL search; if the issue has more than ~50 comments the search result truncates, so re-fetch via the issue endpoint.

**Normalization.** `normalize.go` produces a `NormalizedIssue`:

```go
type NormalizedIssue struct {
    Key         string            // "PLAT-482"
    Title       string            // summary field
    Description string            // ADF → markdown
    Comments    []NormalizedComment
    Status      string
    IssueType   string
    Project     string
    URL         string            // ${JIRA_BASE_URL}/browse/{Key}
    UpdatedAt   time.Time
    Extra       map[string]any    // serialized to sources.extra
}
```

**ADF converter.** `internal/sources/jira/adf.go`. Walks the JSON node tree and emits markdown. Node set handled:

| ADF type           | Markdown                                      |
|--------------------|-----------------------------------------------|
| `paragraph`        | children + `\n\n`                             |
| `heading`          | `#{level} ` + children + `\n\n`               |
| `bulletList`       | `- ` + item children                          |
| `orderedList`      | `1. ` + item children (renumbered per list)   |
| `listItem`         | children                                      |
| `text`             | text, wrapped by `marks` (strong → `**`, em → `*`, code → `` ` ``, link → `[text](href)`)  |
| `codeBlock`        | ``` ```{language}\n{text}\n``` ```            |
| `blockquote`       | `> ` prefix on each line                      |
| `hardBreak`        | `  \n` (two-space line break)                 |
| `rule`             | `---`                                         |
| `mention`          | `@{name}`                                     |
| `inlineCard` / `blockCard` | `[{url}]({url})`                      |
| `emoji`            | `:{shortName}:` or the unicode char           |
| `table` / `tableRow` / `tableCell` / `tableHeader` | pipe-table markdown |
| `media` / `mediaGroup` | skip (attachments are out of scope v1)    |
| `panel`            | `> **{panelType}:** ` + children              |
| Unknown            | recurse children, emit text content           |

Golden-file tests: check in ADF fixtures captured from real Jira responses (IDs redacted) with expected markdown output. Every new unknown node type encountered in production becomes a new fixture.

**Comments.** Each comment becomes a separate `NormalizedComment{Author, CreatedAt, Body}`. Comments are concatenated onto the issue body during chunking with a header `\n\n## Comment by {author} on {date}\n\n` so that chunks containing comments are self-describing. A metadata flag `chunk_kind = "comment"` on chunks lets the retriever optionally boost or filter.

**Rate limiting.** Jira Cloud doesn't document a hard RPS limit but enforces one. On 429, respect `Retry-After` header, exponential backoff otherwise (base 1s, max 60s, jitter 0.3). Workers: 4 by default.

### Confluence

Client: `internal/sources/confluence/client.go`, targeting v2 API at `${CONFLUENCE_BASE_URL}/api/v2`. Same Basic auth as Jira.

**List pages.** `GET /api/v2/spaces/{id}/pages?limit=100&cursor=...&sort=-modified-date`. Cursor-based pagination via `Link` header and `_links.next`. Filter client-side by `modified-date > last_sync_watermark` since the v2 API doesn't support server-side date filtering reliably.

**Fetch page.** `GET /api/v2/pages/{id}?body-format=storage&include-version=true`. The body comes back as storage-format XHTML in `body.storage.value`.

**Storage format → markdown.** `internal/sources/confluence/storage.go`. Use `goquery` to walk the XHTML. Standard HTML tags map to markdown normally (`<h1>` → `#`, `<p>` → paragraph, etc.). Atlassian macros require custom handling:

| Macro                                               | Output                                    |
|-----------------------------------------------------|-------------------------------------------|
| `<ac:structured-macro ac:name="code">`              | fenced code block, language from `<ac:parameter ac:name="language">` |
| `<ac:structured-macro ac:name="info|note|warning">` | `> **Info:** ` + body                     |
| `<ac:structured-macro ac:name="expand">`            | body unwrapped (ignore collapse)          |
| `<ac:structured-macro ac:name="toc">`               | skip                                      |
| `<ac:link><ri:page ri:content-title="Foo"/></ac:link>` | `[Foo](page-url-resolved-at-runtime)`   |
| `<ac:link><ri:user ri:account-id="..."/></ac:link>` | `@user`                                   |
| `<ac:image><ri:attachment/>`                        | skip (attachments v1 non-goal)            |
| Unknown macro                                       | emit inner text content                   |

Page-to-page links need resolution. Store a `page_id → url` map built during ingestion; if a link references a page not yet seen, leave it as a placeholder and fix up on a second pass.

**Ancestors.** Each page's `extra` field stores its ancestor page titles as a breadcrumb: `{"breadcrumb": ["Engineering", "Runbooks", "Credential Rotation"]}`. This is useful context for search snippets and for LLM answers.

**Rate limiting.** Same pattern as Jira — respect `Retry-After`, exponential backoff, 4 workers.

### Documents

Upload via `POST /upload` from the web chat UI. Multipart form, one file per request, max 50MB.

Flow: compute SHA-256, store in S3 at `${sha256}.${ext}`, extract text based on content type (markdown/text pass through, HTML stripped via goquery, PDF extracted via `pdfcpu` or `unidoc` — evaluate both, `pdfcpu` is MIT-licensed and sufficient for most PDFs with embedded text), normalize to markdown, insert into `sources` with `source_type = 'document'` and `source_key = sha256`.

Content addressing means the same file uploaded twice dedupes. Re-upload only triggers re-embedding if the text extraction changed (unlikely unless the PDF library changes).

Scanned PDFs (no embedded text) are detected by a near-zero extraction and logged as skipped. OCR is an explicit v1 non-goal.

### Chunking

`internal/chunk`. Strategy per source type:

| Source     | Strategy                                                                           |
|------------|------------------------------------------------------------------------------------|
| Jira       | One chunk per issue body (description + truncated first N comments up to 4k tokens) + additional chunks for overflow comments, 4k tokens max, 200 token overlap. |
| Confluence | Recursive header-aware split. Keep headings with their sections. 1k token target, 150 overlap. |
| Document   | Recursive split on paragraphs then sentences. 1k token target, 150 overlap.        |

Token counts via `tiktoken-go` with the `cl100k_base` encoder (compatible with `text-embedding-3-small`).

Each chunk carries a `chunk_kind` tag (`body`, `comment`, `section`) that the retriever can use for filtering or re-ranking later.

## Retrieval

`internal/retrieve/search.go`. Single entry point used by `/search`, `/chat`, MCP, and Slack:

```go
type Query struct {
    Text          string
    Filters       Filters   // project, space, source, updated_after
    Limit         int
    UserContext   *UserCtx  // optional: for logging/telemetry
}

type Filters struct {
    Sources         []string  // "jira" | "confluence" | "document"
    Projects        []string  // Jira project keys
    Spaces          []string  // Confluence space keys
    UpdatedAfter    time.Time
}

type Hit struct {
    ID              string        // "confluence:12345" | "jira:PLAT-482" | "document:sha256:..."
    Source          string
    Title           string
    Snippet         string        // with <mark> tags around matches
    URL             string
    ProjectOrSpace  string
    UpdatedAt       time.Time
    Score           float64
    Extra           map[string]any
}

func SearchHits(ctx context.Context, q Query) ([]Hit, error)
```

### Ranking (`rank.go`)

1. **Ticket-key shortcut.** If `q.Text` matches `^[A-Z][A-Z0-9_]+-\d+$`, `SELECT ... WHERE source_type='jira' AND source_key=?`. Return as `score=1.0` if found, then append the hybrid results below it.
2. **Vector branch.** Embed the query (Redis-cached by `md5(text)`, TTL `SEARCH_QUERY_CACHE_TTL`). Query `chunks` with `ORDER BY embedding <=> $1 LIMIT 50`. Cosine distance → similarity via `1 - distance`, normalized to `[0, 1]`.
3. **FTS branch.** `SELECT ..., ts_rank_cd(tsv, plainto_tsquery('english', $1)) AS score FROM chunks WHERE tsv @@ plainto_tsquery('english', $1) ORDER BY score DESC LIMIT 50`. Normalize by dividing by max in the result set.
4. **Fusion.** Outer-join on `chunk_id`; score = `VECTOR_WEIGHT * vector + FTS_WEIGHT * fts` (missing side = 0).
5. **Recency boost.** `score *= 1 + 0.05 * exp(-age_days / SEARCH_RECENCY_DECAY_DAYS)`.
6. **Dedup to source.** Group by `source_id`, keep the highest-scoring chunk per source. Use its content to generate the snippet.
7. **Snippet.** Postgres `ts_headline` with `MaxWords=40, MinWords=20, StartSel=<mark>, StopSel=</mark>`.

### Vector + FTS in one query

Use a CTE to run both branches in a single round-trip:

```sql
WITH vector_hits AS (
  SELECT chunk_id, 1 - (embedding <=> $1::vector) AS v_score
  FROM chunks ORDER BY embedding <=> $1::vector LIMIT 50
),
fts_hits AS (
  SELECT id AS chunk_id, ts_rank_cd(tsv, q) AS f_score
  FROM chunks, plainto_tsquery('english', $2) q
  WHERE tsv @@ q ORDER BY f_score DESC LIMIT 50
),
fused AS (
  SELECT COALESCE(v.chunk_id, f.chunk_id) AS chunk_id,
         COALESCE(v.v_score, 0) * $3 + COALESCE(f.f_score / NULLIF((SELECT MAX(f_score) FROM fts_hits), 0), 0) * $4 AS score
  FROM vector_hits v FULL OUTER JOIN fts_hits f USING (chunk_id)
)
SELECT s.id, s.source_type, s.source_key, s.title, s.url, s.project_or_space, s.updated_at, s.extra,
       ts_headline('english', c.content, plainto_tsquery('english', $2),
                   'MaxWords=40, MinWords=20, StartSel=<mark>, StopSel=</mark>') AS snippet,
       MAX(f.score) AS score
FROM fused f
JOIN chunks c ON c.id = f.chunk_id
JOIN sources s ON s.id = c.source_id
WHERE ($5::text[] IS NULL OR s.source_type = ANY($5))
  AND ($6::text[] IS NULL OR s.project_or_space = ANY($6))
  AND ($7::timestamptz IS NULL OR s.updated_at > $7)
GROUP BY s.id, c.content, c.id
ORDER BY score DESC
LIMIT $8;
```

Target p95 < 300ms warm, < 120ms when the query embedding hits Redis.

## HTTP endpoints

### `GET /search`

Used by the Chrome extension. Fast path — no LLM.

Query params: `q` (required, 1–512 chars), `limit` (1–50, default 10), `source` (repeatable), `project` (repeatable), `space` (repeatable), `updated_after` (RFC 3339).

Auth: OIDC session cookie. On 401, respond with `WWW-Authenticate: Cookie realm="rag-svc"` so the extension knows to prompt the user to sign in at the web UI.

CORS: allow `Origin` when it matches `chrome-extension://${EXTENSION_ID}` or the configured web UI origin. `Access-Control-Allow-Credentials: true`.

Response shape (see "Search endpoint" section in the extension spec below).

Rate limit: `SEARCH_RATE_LIMIT_PER_USER` per 10s per authenticated user. 429 with `retry_after_ms`.

### `POST /chat`

Used by the web chat UI and (via deep-link) by the extension's Ask AI fall-through.

Request: `{"query": "...", "conversation_id": "uuid?", "filters": {...}}`.

Response: Server-Sent Events stream. Events: `retrieve` (hits used for context), `token` (LLM token stream), `done` (final message ID), `error`.

Pipeline: `internal/retrieve` → `internal/answer` which formats a prompt with retrieved chunks, streams through `ANSWER_MODEL`, and persists the conversation.

Auth: OIDC session cookie.

### `POST /upload`

Used by the web chat UI's drag-and-drop. Multipart, one file, max 50MB. Response includes the document's source ID once ingestion finishes (synchronously for small files, via polling for large PDFs).

### `GET /projects` and `GET /spaces`

Autocomplete endpoints for the extension's filter syntax. Return distinct `project_or_space` values from `sources` for the given source type. Cached 1 hour.

### `GET /healthz`, `GET /readyz`

Standard liveness and readiness probes.

### `/mcp`

MCP server over HTTP JSON-RPC. See MCP section below.

### `/slack/*`

Slack slash command and events. See Slack section below.

### Web UI routes

`GET /chat` (SPA-like page with HTMX), `POST /chat/messages` (partial HTML responses), `GET /conversations`, `GET /login`, `GET /auth/callback`, `POST /logout`. See Web UI section.

## Client surface: Chrome extension

Separate npm project under `extension/`. Manifest V3, content script on `https://treetopllc.jira.com/*`, Cmd-K overlay, calls `GET /search` on the Treetop `rag-svc` backend.

### Manifest

```json
{
  "manifest_version": 3,
  "name": "rag-svc supersearch",
  "version": "0.1.0",
  "description": "Cmd-K search across Treetop's Jira, Confluence, and indexed documents.",
  "permissions": ["storage", "cookies"],
  "host_permissions": ["https://treetopllc.jira.com/*"],
  "optional_host_permissions": ["https://*/*"],
  "background": { "service_worker": "src/background.ts", "type": "module" },
  "content_scripts": [{
    "matches": ["https://treetopllc.jira.com/*"],
    "js": ["src/content.ts"],
    "run_at": "document_idle"
  }],
  "action": { "default_title": "rag-svc supersearch" },
  "options_page": "public/options.html",
  "icons": { "16": "public/icon-16.png", "48": "public/icon-48.png", "128": "public/icon-128.png" }
}
```

Backend URL configurable via options page, stored in `chrome.storage.sync`. For Treetop, users set it to `https://rag.treetopllc.com`. When the Avviato instance exists, Avviato users set their extension to `https://rag.avviato.example.com`. Same extension binary, different config. A future v2 could support multiple backends routed by hostname match, but v1 is one-backend-per-install for simplicity.

### UX

- **Trigger.** Capturing keydown listener for `Cmd-K` / `Ctrl-K`. `Esc` dismisses.
- **Overlay.** ~640px centered modal, mounted in closed Shadow DOM to isolate from Jira's CSS.
- **Input.** Single text field with debounced search (150ms). Aborts in-flight request on new keystroke.
- **Results.** Icon, title, snippet (with `<mark>` highlights), metadata line (project/space, updated relative time, status/type for Jira from `extra`).
- **Navigation.** Arrow keys move selection, `Enter` opens in current tab, `Cmd/Ctrl+Enter` new tab.
- **Filter syntax.** `project:PLAT`, `space:ENG`, `source:jira`, `after:2026-01-01` parsed client-side and mapped to query params. Non-matching tokens are free text.
- **Fall-through to Ask AI.** On zero results or `Tab` keypress, switch to Ask mode: same input posts to `/chat`, response streams into the overlay. "Open in chat" deep-links to the web UI for follow-ups. No conversation state in the extension.

### Auth flow

1. Content script posts message to service worker with query + filters.
2. Service worker fetches `${backendUrl}/search?...` with `credentials: "include"`. The session cookie (set by the web UI's OIDC flow on the `rag.treetopllc.com` domain) is sent automatically.
3. On 401, overlay shows "Sign in at rag.treetopllc.com/chat" with a link that opens the web UI in a new tab. After login, the extension works on the next Cmd-K.

The extension never handles OIDC, never sees a token, never stores auth credentials. All it does is ride the session cookie the web UI already established.

### Telemetry

Local-only in v1. `chrome.storage.local` records query latency, result-click position, and fall-through rate. Options page has an opt-in toggle to POST a daily batch to `/telemetry` (backend endpoint is a v2 concern; v1 just collects locally).

### Build

Vite + `@crxjs/vite-plugin`. `npm run build` produces `dist/`. CI builds on tag push and attaches a zip to a GitHub release. For v1, distribute via unpacked load (`chrome://extensions` → "Load unpacked") and document in `extension/README.md`. No Chrome Web Store submission yet — the review cycle is a distraction for a portfolio project.

## Client surface: Web chat UI

Go + HTMX. Templates in `internal/web/templates/`, static assets in `internal/web/static/`.

- **Login.** `GET /login` redirects to OIDC. `GET /auth/callback` exchanges the code, creates a session, sets `rag_svc_session` cookie (HTTPOnly, Secure, SameSite=Lax, domain-scoped to the `rag-svc` host).
- **Chat.** `GET /chat` renders the conversation list sidebar + chat pane. Messages post via HTMX to `POST /chat/messages`, which returns the assistant's streamed response as SSE → HTMX swaps it into the DOM.
- **Upload.** Drag-and-drop zone posts to `/upload`. Successful uploads appear in a "Documents" tab with title, size, upload date, and a delete action.
- **Conversations.** Persisted in Postgres under a `conversations` + `messages` schema (add to `0002_conversations.sql`). Users see only their own.
- **Source citations.** Assistant responses include inline footnote-style links: `[Credential rotation runbook][^1]`. Citations come from the `retrieve` event in the SSE stream and are rendered as a collapsible panel below the message.

## Client surface: Slack app

- **Install.** Per-workspace OAuth install flow at `GET /slack/install`. Stores workspace bot token in Postgres (`slack_installations` table in `0003_slack.sql`).
- **Slash command.** `/rag <query>`. Webhook at `POST /slack/commands` verifies the signing secret, runs `SearchHits` + `answer`, replies with the answer + top 3 source links as a Slack block.
- **Mention.** `@rag-svc ...` in any channel the bot is in. Webhook at `POST /slack/events`. Same pipeline as slash command.
- **Auth model.** The Slack user's identity is known (workspace + user ID) but not mapped to OIDC. Queries run as the `rag-svc` service account's view of Treetop's content, which is the same view all users get. This is consistent with the "single-tenant, single ACL" stance.

## Client surface: MCP server

HTTP JSON-RPC at `POST /mcp`. Authenticated via static bearer tokens issued by `rag-svc token create`.

Tools exposed:

| Tool              | Purpose                                                     |
|-------------------|-------------------------------------------------------------|
| `search`          | Hybrid search with filters. Returns ranked hits.            |
| `get_issue`       | Fetch a Jira issue by key, including comments.              |
| `get_page`        | Fetch a Confluence page by ID or URL.                       |
| `list_projects`   | List indexed Jira projects.                                 |
| `list_spaces`     | List indexed Confluence spaces.                             |
| `ask`             | End-to-end RAG: retrieve + generate. Returns text + sources.|

### Using rag-svc from Claude Code

`.mcp.json` in the user's project:

```json
{
  "mcpServers": {
    "rag-treetop": {
      "type": "http",
      "url": "https://rag.treetopllc.com/mcp",
      "headers": { "Authorization": "Bearer ${RAG_TREETOP_TOKEN}" }
    }
  }
}
```

Generate the token with `rag-svc token create --name "claude-code-salvo"`, save output to a password manager, export as env var. The token is a UUID-like string; only its `sha256` is stored in `tokens.token_hash`.

In conversation, Claude Code uses `search` or `ask` to ground its answers in Treetop's actual Jira and Confluence content. Typical pattern:

> User: "How does our credential rotation work?"
> Claude: [calls `search(query="credential rotation")`, gets the runbook page + related tickets, answers with citations]

The tools' schemas are conservative — inputs and outputs are strictly typed so Claude doesn't have to guess.

## Deployment

### Local dev

`docker compose up` brings up Postgres (with pgvector), Redis, MinIO, and `rag-svc`. Seed data available via `make seed` which ingests a small fixture set from recorded Atlassian responses — no live API calls needed to run the app locally.

`.env.example` ships with safe defaults (local Postgres, dummy OIDC, bearer token disabled). Developer copies to `.env` and fills in their own Atlassian token to run live ingestion.

### Production

Kubernetes with kustomize. `deploy/k8s/overlays/prod-treetop/` contains:

- Treetop-specific `ConfigMap` with URLs, OIDC issuer, etc.
- `Secret` with Atlassian token, OIDC client secret, Slack tokens, embed/answer API keys. Sourced from Vault in CI (not committed).
- `Ingress` for `rag.treetopllc.com`.
- Horizontal pod autoscaler on CPU (ingestion is bursty).
- CronJob resources for `rag-svc ingest jira` and `rag-svc ingest confluence` on `JIRA_POLL_INTERVAL` and `CONFLUENCE_POLL_INTERVAL`.

Postgres is a managed service (RDS or equivalent) with `pgvector` enabled — not run in the cluster. Same for Redis and S3.

The Avviato deployment uses `overlays/prod-avviato/` with its own values. Same image, different config and secrets. No shared state.

## Testing

- **Unit.** Converters (ADF, storage format), ranking fusion, filter parser, chunking boundaries.
- **Golden files.** ADF and storage-format fixtures captured from real responses (IDs redacted). Every new edge case becomes a new fixture.
- **Integration.** `testcontainers-go` spins up Postgres + Redis + MinIO. Fixture-loaded DB, run full queries, assert hit ordering and scoring.
- **Contract.** JSON schema for `/search` response is authored as Go structs; TS types in the extension are generated from the schema via `quicktype` in the build step. Mismatches break CI.
- **E2E.** Headless Chrome loads the unpacked extension against a fixture Jira page + local `rag-svc`, asserts Cmd-K opens the overlay and known queries return expected top hits.

CI: CircleCI. Jobs per package (fast unit tests), one integration job, one extension build + E2E job. Parallelize for sub-5-minute CI.

## Rollout order

A suggested build sequence for Claude Code to follow. Each step is independently shippable.

1. **Skeleton.** `cmd/rag-svc/main.go`, config loader, store package with migrations, `/healthz`. `docker compose up` starts the server.
2. **Jira ingestion.** Client, ADF converter (with golden-file tests), normalizer, chunker, embeddings client, `rag-svc ingest jira`. Smoke-test against a small Treetop project.
3. **Retrieval.** `internal/retrieve` with the hybrid CTE query, rank fusion, ticket-key shortcut. Write Go-level tests against a fixture-loaded test DB.
4. **`/search` endpoint.** Handler, OIDC middleware (stub user first, real OIDC second), CORS. Hit it with `curl`.
5. **Chrome extension.** Manifest, content script, overlay component, debounced fetch, filter parser. Dogfood against Treetop for a week.
6. **Confluence ingestion.** Client, storage-format converter, normalizer. Wire into existing chunker.
7. **Documents.** Upload endpoint, S3 client, PDF extraction. Web UI drag-and-drop comes with the web UI milestone.
8. **Web chat UI.** OIDC real, HTMX templates, `/chat` SSE endpoint, conversation persistence, `internal/answer`.
9. **MCP server.** Tool handlers, bearer auth, `rag-svc token` subcommands. Test against Claude Code locally.
10. **Slack app.** OAuth install, slash command, mention handler. Test in a private workspace.
11. **Extension Ask AI fall-through.** Once `/chat` is stable, add Tab-switch behavior.
12. **K8s prod overlays.** `prod-treetop` overlay, CronJobs, ingress. Deploy.

Steps 1–5 give you a working "supersearch" product. Everything after is additive.

## Portfolio notes

Things worth calling out in the README and in any demo:

- **Hand-rolled ADF and storage-format converters.** No good Go libraries exist; writing these against the specs is a clear "I read the docs" signal.
- **Hybrid retrieval in a single SQL query.** The CTE that fuses pgvector and FTS in one round-trip, with filters, is the kind of thing people often solve with application-level orchestration. Doing it in SQL is faster and a nice calling card.
- **Four client surfaces, one retrieval core.** Search, chat, Slack, MCP all share the same `internal/retrieve` and `internal/answer` packages. The diagram makes this visible.
- **Deployment isolation by design.** Treetop and Avviato as independent deployments rather than multi-tenant features shows deliberate scope control.
- **Deprecation-aware.** Using `/rest/api/3/search/jql` (the post-August-2025 endpoint) rather than the legacy search shows you're tracking the platform.

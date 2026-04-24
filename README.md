# rag-svc (Treetop)

A retrieval-augmented generation service that indexes Treetop's Atlassian Cloud content (Jira + Confluence) and user-uploaded documents, and exposes it through four client surfaces: a Chrome extension "supersearch," a web chat UI, a Slack app, and an MCP server for Claude Code.

See [`CLAUDE.md`](CLAUDE.md) for the full design: architecture, schema, retrieval strategy, ingestion, and the 12-step rollout plan.

## Status

Rollout step 9 complete. What works end-to-end:

- **MCP server** at `POST /mcp` — streamable-HTTP JSON-RPC 2.0.
  Methods: `initialize`, `ping`, `tools/list`, `tools/call`, plus
  `notifications/initialized` ack. Batches supported.
- **Six tools**: `search`, `ask`, `list_projects`, `list_spaces`,
  `get_issue`, `get_page`. Inputs advertised via JSON Schema in
  `tools/list`. Outputs returned as a single `text` content block
  containing JSON-stringified structured data. `get_issue`/`get_page`
  read the stored `sources` row (no live Atlassian round-trip).
- **Bearer auth**: `Authorization: Bearer rag_<uuid>`. Token raw value
  is shown once at creation and never stored — only `sha256` lives in
  the `tokens` table. Per-request `last_used_at` update. Revoked
  tokens 401 with the JSON-RPC-shaped error body.
- **`rag-svc token create|list|revoke`** CLI subcommands. `revoke`
  accepts full UUIDs or unambiguous prefixes.

Previously complete (step 8):

- **Web chat UI** at `GET /chat`: HTMX-driven, closed Shadow-free (plain
  Go `html/template` + `embed.FS`). Conversation list sidebar + streaming
  chat pane. `POST /chat/messages` saves the user message and returns an
  HTML fragment whose assistant placeholder subscribes via
  `htmx-ext-sse` to `GET /chat/stream?c=&m=`. That stream performs
  retrieval → prompt assembly → OpenAI-compatible completion (token
  events), then persists the assistant message (with citations) and
  replaces the placeholder with the final rendered bubble.
- **`internal/answer`**: retrieval + system-prompt-with-numbered-context
  + streaming LLM. Persists both sides of the turn. `BuildPrompt` is
  exported so future ports (MCP's `ask` tool) share the formatter.
- **Conversations persistence** (migration 0003): `conversations` +
  `messages` with JSONB citations. User-scoped queries prevent cross-
  user leaks; `ErrConversationNotFound` masks ownership mismatches.
- **Real OIDC** (behind `OIDC_ISSUER`) via `coreos/go-oidc` — `/login`,
  `/auth/callback`, session cookie carries the ID token, middleware
  validates on every request. When `OIDC_ISSUER` is unset, stub mode
  stays enabled so local dev still works via `/dev/login`.
- **Documents page**: `/documents` lists indexed uploads with title,
  extraction method, size, and inline/download links. **`GET /documents/{sha}`**
  streams the blob with the original `Content-Type` and filename.
- **Browser redirects on 401** for HTML requests, keeping the API-client
  401+`WWW-Authenticate` posture for the Chrome extension and curl.

Previously complete (step 7):

- **Documents** (`POST /upload`): multipart upload, 50MB cap, content-type
  allowlist (markdown / text / HTML / PDF), SHA-256 dedup, blob stored in
  S3/MinIO at `documents/<sha>.<ext>`, text extracted (markdown and text
  pass through, HTML via goquery with script/style strip, PDF via
  `ledongthuc/pdf`), scanned-PDF detection → 422, uploader recorded in
  `extra`, chunked with the Document strategy (paragraph → sentence →
  hard split, 1k / 150, `chunk_kind=section`), embedded, upserted. Same
  middleware stack as `/search` (auth, CORS, rate limit).
  `rag-svc ingest docs` re-chunks + re-embeds from stored body_markdown
  without hitting S3.

Previously complete (step 6):

- **Confluence ingestion** (`rag-svc ingest confluence`): v2 API client
  (`/spaces`, `/spaces/{id}/pages` cursor-paginated, `/pages/{id}` with
  storage-format body, `/pages/{id}/ancestors` + bulk title resolution),
  storage-format → markdown converter (macros: code, info/note/warning,
  expand, jira; drops: toc/attachments/lucidchart/view-file), sentinel-
  based two-pass page-link resolver, breadcrumb in `extra`.
  Header-aware chunker (1k tokens, 150 overlap, `chunk_kind=section`).

Previously complete (step 5):

- **Chrome extension** under `extension/` — Manifest V3, Cmd-K overlay in
  closed Shadow DOM on `treetopllc.jira.com`, debounced `/search` fetch
  with filter-token parser (`project:`/`space:`/`source:`/`after:`),
  pinned extension ID (`mcgmonphpfgfkhpjmmcgcelgcmpjcmmc`). See
  [`extension/README.md`](extension/README.md) for load-unpacked +
  manual test flow.

Previously complete (step 4):

- HTTP server: `/healthz`, `/readyz`, `/search`, `/projects`, `/spaces`,
  `/dev/login` (stub-mode only).
- Migrations: initial schema + `chunk_kind` column on chunks.
- **Jira ingestion** (`rag-svc ingest jira`): incremental sync, ADF →
  markdown, 4k-token chunks with 200-token overlap, OpenAI-compatible
  embeddings, transactional upsert, per-source watermark. Idempotent.
- **Hybrid retrieval** via `GET /search`: vector (pgvector HNSW) + BM25
  fused in one CTE, recency boost, source-level dedup, ticket-key
  shortcut (score=1.0).
- **Query-embedding cache** (Valkey/Redis): 1000× speedup on repeat
  queries. Keyed by `md5(model||text)`.
- **Auth + middleware** on `/search`, `/projects`, `/spaces`:
  stub session cookies (`stub:<email>` when `OIDC_ISSUER` is unset),
  CORS allowlist from `EXTENSION_ID` + `WEB_UI_ORIGIN`,
  per-user rate limit (`SEARCH_RATE_LIMIT_PER_USER` per 10s,
  Redis INCR+EXPIRE, fail-open on Redis error),
  `WWW-Authenticate: Cookie realm="rag-svc"` on 401.
- **Redis HA**: `REDIS_MODE=standalone|sentinel` targeting K8s
  Valkey-Sentinel via go-redis failover client.

Real OIDC login (`/login` + `/auth/callback`) lands in step 8 (web UI).
Confluence (step 6), documents (step 7), Slack, and MCP still to come.

## Getting started (docker compose — local dev only)

> Compose is the local development loop. Production deploys to Kubernetes via kustomize overlays under `deploy/k8s/` (coming in rollout step 12). Nothing in `deploy/compose/` is intended to run in production.

```bash
git clone <this-repo> rag-svc && cd rag-svc
cp .env.example .env      # defaults are safe for local compose — Atlassian/LLM/OIDC vars stay as CHANGEME
docker compose -f deploy/compose/docker-compose.yml up --build
```

Compose brings up:

- Postgres 16 with `pgvector` (`pgvector/pgvector:pg16`)
- Redis 7
- MinIO (S3-compatible blob store)
- `rag-svc` itself, which runs migrations then starts the HTTP server on `:8080`

Smoke test:

```bash
curl -s localhost:8080/healthz
# → {"status":"ok"}

curl -s localhost:8080/readyz
# → {"status":"ok","checks":{"postgres":"ok","redis":"ok"}}

# In stub-auth mode (OIDC_ISSUER unset), /dev/login sets a session cookie:
curl -s -c /tmp/jar 'localhost:8080/dev/login?email=you@example.com'
curl -s -b /tmp/jar 'localhost:8080/search?q=credential+rotation'
curl -s -b /tmp/jar localhost:8080/projects
```

## Running outside compose

If you already have Postgres and Redis (e.g., the LAN cnpg cluster at `192.168.50.49`), you can point `rag-svc` at them:

```bash
cp .env.example .env
# Uncomment the LAN DATABASE_URL line or swap your own DSN in.
make build
./bin/rag-svc migrate
./bin/rag-svc serve
```

`.env.example` contains a commented-out alternative `DATABASE_URL` shape
for pointing at a managed or LAN Postgres instead of compose.

## Subcommands

| Command | Status | Purpose |
|---|---|---|
| `rag-svc serve` | live | HTTP server: `/healthz`, `/readyz` (more endpoints land in later steps) |
| `rag-svc migrate` | live | Apply DB migrations (`internal/store/migrations/`) |
| `rag-svc ingest jira` | live | Incremental Jira sync (requires `JIRA_BASE_URL`, `JIRA_EMAIL`, `JIRA_TOKEN`; filters via `JIRA_PROJECTS`) |
| `rag-svc ingest confluence` | live | Incremental Confluence sync (requires `CONFLUENCE_BASE_URL`, `CONFLUENCE_EMAIL`, `CONFLUENCE_TOKEN`, `CONFLUENCE_SPACES`) |
| `rag-svc ingest docs` | live | Re-chunk and re-embed existing document sources from stored `body_markdown` (no S3 access) |
| `rag-svc reindex` | stub | Rebuild embeddings from stored source content |
| `rag-svc token create` / `list` / `revoke` | live | Manage MCP bearer tokens (only sha256 stored; raw token shown once) |

Stubs print `not implemented` and exit non-zero. They'll be filled in as their respective milestones land.

## Configuration

All configuration is environment variables. See [`.env.example`](.env.example) for the complete list with defaults.

- **Required at startup**: `DATABASE_URL`, `REDIS_URL`. Missing either causes the binary to exit with a descriptive error.
- **Required per subcommand**: Atlassian/OIDC/LLM credentials are loaded but not enforced at startup. Each subcommand (once implemented) validates the credentials it needs.

This split lets `docker compose up` boot the server with CHANGEME placeholders for Atlassian/OIDC/LLM, while a real `rag-svc ingest jira` invocation will fail loud if `JIRA_TOKEN` is unset.

## Development

```bash
make build        # ./bin/rag-svc
make test         # unit tests
make lint         # go vet + gofmt
make compose-up   # docker compose up --build
make compose-down # docker compose down -v (drops volumes)
```

## Repository layout

```
rag-svc/
├── cmd/rag-svc/main.go        # cobra entry point
├── internal/
│   ├── config/                # env loading + validation
│   ├── store/                 # pgx pool, migrations, repositories
│   │   └── migrations/        # golang-migrate SQL files
│   └── http/                  # HTTP handlers (healthz/readyz today)
├── deploy/compose/            # docker-compose for local dev
├── Dockerfile                 # multi-stage alpine build
├── Makefile
├── .env.example
├── README.md                  # this file
└── CLAUDE.md                  # full design + spec
```

Later steps will add `internal/embed`, `internal/chunk`, `internal/blob`, `internal/sources/{jira,confluence,document}`, `internal/ingest`, `internal/retrieve`, `internal/answer`, `internal/auth/{oidc,bearer,slack}`, `internal/web`, `internal/slack`, `internal/mcp`, and the `extension/` Chrome project.

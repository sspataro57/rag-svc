CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- Source documents (parent rows)
CREATE TABLE sources (
    id               BIGSERIAL PRIMARY KEY,
    source_type      TEXT NOT NULL CHECK (source_type IN ('jira', 'confluence', 'document')),
    source_key       TEXT NOT NULL,
    project_or_space TEXT,
    title            TEXT NOT NULL,
    url              TEXT NOT NULL,
    body_markdown    TEXT NOT NULL,
    extra            JSONB NOT NULL DEFAULT '{}',
    updated_at       TIMESTAMPTZ NOT NULL,
    indexed_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (source_type, source_key)
);

CREATE INDEX sources_project_space_idx ON sources (project_or_space);
CREATE INDEX sources_updated_at_idx ON sources (updated_at DESC);

-- Chunks (one row per embedding)
CREATE TABLE chunks (
    id           BIGSERIAL PRIMARY KEY,
    source_id    BIGINT NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    chunk_index  INT NOT NULL,
    content      TEXT NOT NULL,
    token_count  INT NOT NULL,
    embedding    vector(1536) NOT NULL,
    tsv          tsvector GENERATED ALWAYS AS (to_tsvector('english', content)) STORED,
    UNIQUE (source_id, chunk_index)
);

CREATE INDEX chunks_embedding_hnsw ON chunks USING hnsw (embedding vector_cosine_ops);
CREATE INDEX chunks_tsv_gin ON chunks USING gin (tsv);
CREATE INDEX chunks_source_id_idx ON chunks (source_id);

-- Ingestion watermarks
CREATE TABLE ingest_state (
    source_type    TEXT PRIMARY KEY,
    last_synced_at TIMESTAMPTZ NOT NULL,
    cursor         TEXT
);

-- MCP bearer tokens
CREATE TABLE tokens (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name         TEXT NOT NULL,
    token_hash   TEXT NOT NULL UNIQUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ
);

DROP TABLE IF EXISTS tokens;
DROP TABLE IF EXISTS ingest_state;
DROP TABLE IF EXISTS chunks;
DROP TABLE IF EXISTS sources;

-- pg_trgm and vector extensions are intentionally left in place on down —
-- other applications in the same database may depend on them.

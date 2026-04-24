ALTER TABLE chunks
    ADD COLUMN chunk_kind TEXT NOT NULL DEFAULT 'body'
    CHECK (chunk_kind IN ('body', 'comment', 'section'));

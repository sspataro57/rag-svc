-- Directed graph of relationships between sources. One row per outgoing
-- edge. Targets are stored by (type, key) rather than source_id so we can
-- record links to sources that haven't been ingested yet (e.g. a Jira
-- ticket linking to a project we don't index) and have them light up
-- automatically once the target arrives.
--
-- Re-ingest replaces a source's outgoing edges wholesale (ReplaceSourceLinks
-- mirrors ReplaceChunks), so removing a link in Jira propagates here.
CREATE TABLE source_links (
    source_id          BIGINT      NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
    target_source_type TEXT        NOT NULL,
    target_source_key  TEXT        NOT NULL,
    kind               TEXT        NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY (source_id, target_source_type, target_source_key, kind)
);

-- Reverse lookup: "what links to this source?" The PK already covers the
-- forward direction.
CREATE INDEX source_links_target_idx
    ON source_links (target_source_type, target_source_key);

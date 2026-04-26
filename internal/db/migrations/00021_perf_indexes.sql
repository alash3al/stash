-- +goose Up
-- Performance indexes for the Stash memory system.
-- Covers the most frequent query patterns in consolidation and recall hot paths.

-- Composite index for decay queries: selects facts by namespace + age (updated_at).
-- Used by DecayConfidence: WHERE namespace_id = $1 AND updated_at < now() - interval
CREATE INDEX IF NOT EXISTS facts_decay_idx
    ON facts (namespace_id, updated_at)
    WHERE deleted_at IS NULL AND valid_until IS NULL;

-- Composite covering index for consolidation checkpoint queries.
-- FetchFacts uses: WHERE namespace_id = $1 AND deleted_at IS NULL AND id > $2 ORDER BY id
CREATE INDEX IF NOT EXISTS facts_consolidation_idx
    ON facts (namespace_id, id)
    WHERE deleted_at IS NULL;

-- Same pattern for episodes consolidation fetch.
CREATE INDEX IF NOT EXISTS episodes_consolidation_idx
    ON episodes (namespace_id, id)
    WHERE deleted_at IS NULL;

-- NOTE: embedding_cache already has PRIMARY KEY (text_hash, model), which PostgreSQL
-- automatically backs with a unique B-tree index — no additional index needed.

-- +goose Down
DROP INDEX IF EXISTS facts_decay_idx;
DROP INDEX IF EXISTS facts_consolidation_idx;
DROP INDEX IF EXISTS episodes_consolidation_idx;

-- +goose Up
-- Unique index on relationships to enable ON CONFLICT DO NOTHING deduplication.
-- Replaces the previous SELECT-then-INSERT round trip in the consolidation pipeline.
--
-- Scoped to rows where:
--   - deleted_at IS NULL  (active relationships only)
--   - source_fact_id IS NOT NULL (always true in the consolidation path)
--
-- This partial form keeps the index small and allows the ON CONFLICT target in
-- the application to reference it precisely without expression workarounds.
CREATE UNIQUE INDEX IF NOT EXISTS relationships_dedup_idx
    ON relationships (namespace_id, from_entity, relation_type, to_entity, source_fact_id)
    WHERE deleted_at IS NULL AND source_fact_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS relationships_dedup_idx;

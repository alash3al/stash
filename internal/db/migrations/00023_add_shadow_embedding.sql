-- +goose Up
-- Shadow embedding columns for a zero-downtime embedding-model migration (TOL-297).
--
-- Why a shadow column rather than re-embedding in place: the retrieval path has
-- no model predicate. RecallFacts filters on namespace, vector and limit only,
-- so it cannot tell a bge-m3 vector from a Qwen one. Overwrite the live column
-- progressively and every query compares a new-model query vector against
-- old-model stored vectors — cosine similarity across models is noise, and the
-- noise can outrank real hits. That failure is silent, which makes it worse
-- than missing data.
--
-- With a shadow column both representations coexist. Live recall keeps using
-- `embedding` untouched while `embedding_shadow` fills in the background, and
-- the read path moves exactly once, after validation.
--
-- The column is created UNCONSTRAINED. The migrator sets its dimension and
-- builds the HNSW index on first run, mirroring how db.go already handles the
-- live vector columns. That keeps the target dimension a matter of
-- configuration rather than something frozen into a migration file.
ALTER TABLE episodes ADD COLUMN IF NOT EXISTS embedding_shadow vector;
ALTER TABLE episodes ADD COLUMN IF NOT EXISTS embedding_shadow_model TEXT;

ALTER TABLE facts ADD COLUMN IF NOT EXISTS embedding_shadow vector;
ALTER TABLE facts ADD COLUMN IF NOT EXISTS embedding_shadow_model TEXT;

-- Partial indexes make "what is left to migrate" cheap for the wave driver,
-- which asks that question once per batch.
CREATE INDEX IF NOT EXISTS episodes_shadow_pending_idx
    ON episodes (namespace_id) WHERE embedding_shadow IS NULL AND deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS facts_shadow_pending_idx
    ON facts (namespace_id) WHERE embedding_shadow IS NULL AND deleted_at IS NULL;

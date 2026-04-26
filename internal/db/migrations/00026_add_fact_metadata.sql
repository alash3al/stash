-- +goose Up
ALTER TABLE facts ADD COLUMN metadata JSONB;
CREATE INDEX ON facts (namespace_id) WHERE metadata IS NOT NULL;

-- +goose Down
ALTER TABLE facts DROP COLUMN IF EXISTS metadata;

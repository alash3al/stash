-- +goose Up
-- Allow causal links to point to either a fact or a failure.
ALTER TABLE causal_links ALTER COLUMN effect_fact_id DROP NOT NULL;
ALTER TABLE causal_links ADD COLUMN effect_failure_id BIGINT NULL REFERENCES failures(id) ON DELETE CASCADE;

-- Ensure a causal link has exactly one effect (either a fact or a failure).
ALTER TABLE causal_links ADD CONSTRAINT causal_links_effect_check 
    CHECK ((effect_fact_id IS NOT NULL AND effect_failure_id IS NULL) OR (effect_fact_id IS NULL AND effect_failure_id IS NOT NULL));

-- Update the unique index to include the new column.
DROP INDEX IF EXISTS causal_links_cause_fact_id_effect_fact_id_idx;
CREATE UNIQUE INDEX causal_links_fact_effect_uidx ON causal_links (cause_fact_id, effect_fact_id) 
    WHERE effect_fact_id IS NOT NULL AND deleted_at IS NULL;
CREATE UNIQUE INDEX causal_links_failure_effect_uidx ON causal_links (cause_fact_id, effect_failure_id) 
    WHERE effect_failure_id IS NOT NULL AND deleted_at IS NULL;

-- +goose Down
ALTER TABLE causal_links DROP CONSTRAINT causal_links_effect_check;
ALTER TABLE causal_links DROP COLUMN effect_failure_id;
ALTER TABLE causal_links ALTER COLUMN effect_fact_id SET NOT NULL;
DROP INDEX IF EXISTS causal_links_fact_effect_uidx;
DROP INDEX IF EXISTS causal_links_failure_effect_uidx;
CREATE UNIQUE INDEX causal_links_cause_fact_id_effect_fact_id_idx ON causal_links (cause_fact_id, effect_fact_id) WHERE deleted_at IS NULL;

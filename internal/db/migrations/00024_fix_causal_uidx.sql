-- +goose Up
DROP INDEX IF EXISTS causal_links_fact_effect_uidx;
DROP INDEX IF EXISTS causal_links_failure_effect_uidx;
CREATE UNIQUE INDEX causal_links_uidx ON causal_links (cause_fact_id, COALESCE(effect_fact_id, 0), COALESCE(effect_failure_id, 0)) WHERE deleted_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS causal_links_uidx;
CREATE UNIQUE INDEX causal_links_fact_effect_uidx ON causal_links (cause_fact_id, effect_fact_id) WHERE effect_fact_id IS NOT NULL AND deleted_at IS NULL;
CREATE UNIQUE INDEX causal_links_failure_effect_uidx ON causal_links (cause_fact_id, effect_failure_id) WHERE effect_failure_id IS NOT NULL AND deleted_at IS NULL;

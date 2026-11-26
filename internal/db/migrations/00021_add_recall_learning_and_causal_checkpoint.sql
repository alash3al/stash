-- +goose Up
ALTER TABLE consolidation_progress
    ADD COLUMN last_causal_fact_id BIGINT NOT NULL DEFAULT 0;

-- Existing installations may contain thousands of facts. Initialize from the
-- relationship/fact checkpoint so this release does not silently trigger an
-- unbounded historical LLM replay. A graph backfill must be an explicit,
-- separately throttled operation. New namespaces still begin at zero.
UPDATE consolidation_progress SET last_causal_fact_id = last_fact_id;

-- Recall impressions intentionally omit raw query text. The hash is sufficient
-- for replay/dedup metrics without turning the learning ledger into a second
-- copy of potentially sensitive prompts.
CREATE TABLE recall_impressions (
    id              BIGSERIAL   PRIMARY KEY,
    namespace_ids   BIGINT[]    NOT NULL,
    query_hash      TEXT        NOT NULL,
    caller          TEXT        NOT NULL DEFAULT 'unknown',
    result_count    INTEGER     NOT NULL CHECK (result_count >= 0),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX recall_impressions_created_at_idx ON recall_impressions (created_at);
CREATE INDEX recall_impressions_query_hash_idx ON recall_impressions (query_hash);

CREATE TABLE recall_impression_results (
    impression_id   BIGINT      NOT NULL REFERENCES recall_impressions(id) ON DELETE CASCADE,
    memory_type     TEXT        NOT NULL CHECK (memory_type IN ('fact', 'episode')),
    memory_id       BIGINT      NOT NULL,
    namespace_id    BIGINT      NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    rank            INTEGER     NOT NULL CHECK (rank > 0),
    semantic_score  REAL        NOT NULL,
    utility_score   REAL        NOT NULL DEFAULT 0 CHECK (utility_score >= -1 AND utility_score <= 1),
    final_score     REAL        NOT NULL,
    PRIMARY KEY (impression_id, memory_type, memory_id)
);

CREATE INDEX recall_impression_results_memory_idx
    ON recall_impression_results (memory_type, memory_id);

CREATE TABLE memory_utility (
    memory_type       TEXT        NOT NULL CHECK (memory_type IN ('fact', 'episode')),
    memory_id         BIGINT      NOT NULL,
    namespace_id      BIGINT      NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    helpful_count     BIGINT      NOT NULL DEFAULT 0 CHECK (helpful_count >= 0),
    harmful_count     BIGINT      NOT NULL DEFAULT 0 CHECK (harmful_count >= 0),
    neutral_count     BIGINT      NOT NULL DEFAULT 0 CHECK (neutral_count >= 0),
    recall_count      BIGINT      NOT NULL DEFAULT 0 CHECK (recall_count >= 0),
    utility_score     REAL        NOT NULL DEFAULT 0 CHECK (utility_score >= -1 AND utility_score <= 1),
    last_recalled_at  TIMESTAMPTZ NULL,
    last_feedback_at  TIMESTAMPTZ NULL,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (memory_type, memory_id)
);

CREATE INDEX memory_utility_namespace_score_idx
    ON memory_utility (namespace_id, utility_score DESC);

CREATE TABLE recall_feedback (
    id                BIGSERIAL   PRIMARY KEY,
    impression_id     BIGINT      NOT NULL REFERENCES recall_impressions(id) ON DELETE CASCADE,
    memory_type       TEXT        NOT NULL CHECK (memory_type IN ('fact', 'episode')),
    memory_id         BIGINT      NOT NULL,
    signal            TEXT        NOT NULL CHECK (signal IN ('helpful', 'harmful', 'neutral')),
    idempotency_key   TEXT        NOT NULL UNIQUE,
    reason            TEXT        NULL CHECK (char_length(reason) <= 1000),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (impression_id, memory_type, memory_id)
        REFERENCES recall_impression_results(impression_id, memory_type, memory_id)
        ON DELETE CASCADE
);

CREATE INDEX recall_feedback_impression_idx ON recall_feedback (impression_id);
CREATE INDEX recall_feedback_memory_idx ON recall_feedback (memory_type, memory_id);
CREATE UNIQUE INDEX recall_feedback_one_signal_per_result_idx
    ON recall_feedback (impression_id, memory_type, memory_id);

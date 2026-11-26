-- +goose Up
-- Records that repeatedly fail a consolidation stage for a PERMANENT reason.
--
-- Without this table a single unparseable record blocks its stage's watermark
-- forever, so every cycle re-sends it to the paid reasoner. Tracking attempts
-- lets the watermark advance past a record that can never succeed, while a
-- transient provider failure still retries normally and loses nothing.
CREATE TABLE consolidation_quarantine (
    namespace_id    BIGINT          NOT NULL REFERENCES namespaces(id) ON DELETE CASCADE,
    stage           TEXT            NOT NULL,
    record_id       BIGINT          NOT NULL,
    attempts        INTEGER         NOT NULL DEFAULT 0,
    last_error      TEXT            NOT NULL DEFAULT '',
    first_seen_at   TIMESTAMPTZ     NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ     NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace_id, stage, record_id)
);

-- Operator view: "what is Stash refusing to process, and why."
CREATE INDEX consolidation_quarantine_stage_idx
    ON consolidation_quarantine (stage, updated_at DESC);

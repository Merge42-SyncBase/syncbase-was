CREATE TABLE processing_step_attempt (
    run_id uuid NOT NULL REFERENCES processing_run(id),
    automatic_attempt integer NOT NULL CHECK (automatic_attempt BETWEEN 1 AND 3),
    stage text NOT NULL CHECK (stage IN ('METADATA', 'PARSE', 'CHUNK', 'EMBED', 'STORE', 'ACTIVATE')),
    fencing_token bigint NOT NULL CHECK (fencing_token > 0),
    outcome text NOT NULL CHECK (outcome IN ('RUNNING', 'RETRY_SCHEDULED', 'SUCCEEDED', 'FAILED', 'SUPERSEDED')),
    error_code text,
    started_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    finished_at timestamptz,
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (run_id, automatic_attempt, stage)
);

CREATE INDEX processing_step_attempt_run_idx
    ON processing_step_attempt (run_id, automatic_attempt, stage);

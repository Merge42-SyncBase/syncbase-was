ALTER TABLE processing_step_attempt
    DROP CONSTRAINT processing_step_attempt_pkey;

ALTER TABLE processing_step_attempt
    ADD PRIMARY KEY (run_id, fencing_token, stage);

DROP INDEX processing_step_attempt_run_idx;

CREATE INDEX processing_step_attempt_run_idx
    ON processing_step_attempt (run_id, automatic_attempt, stage, fencing_token);

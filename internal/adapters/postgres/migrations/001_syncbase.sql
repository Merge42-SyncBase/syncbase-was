CREATE TABLE processing_profile (
    fingerprint text PRIMARY KEY CHECK (fingerprint ~ '^[0-9a-f]{64}$'),
    canonical_json jsonb NOT NULL,
    parser_id text NOT NULL CHECK (parser_id <> ''),
    chunker_id text NOT NULL CHECK (chunker_id <> ''),
    embedding_model_id text NOT NULL CHECK (embedding_model_id <> ''),
    vector_dimension integer NOT NULL CHECK (vector_dimension = 384),
    distance text NOT NULL CHECK (distance = 'cosine'),
    minimum_score double precision NOT NULL CHECK (minimum_score BETWEEN 0.0 AND 1.0),
    active boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE UNIQUE INDEX processing_profile_one_active ON processing_profile ((true)) WHERE active;

CREATE TABLE document (
    id uuid PRIMARY KEY,
    display_name text NOT NULL CHECK (char_length(display_name) BETWEEN 1 AND 200),
    normalized_name text NOT NULL CHECK (normalized_name <> ''),
    next_version integer NOT NULL DEFAULT 1 CHECK (next_version > 0),
    active_version_id uuid,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX document_normalized_name_idx ON document (normalized_name);

CREATE TABLE document_version (
    id uuid PRIMARY KEY,
    document_id uuid NOT NULL REFERENCES document(id),
    version_number integer NOT NULL CHECK (version_number > 0),
    content_sha256 text NOT NULL CHECK (content_sha256 ~ '^[0-9a-f]{64}$'),
    byte_size bigint NOT NULL CHECK (byte_size BETWEEN 1 AND 104857600),
    original_file_name text NOT NULL CHECK (original_file_name <> ''),
    storage_key text NOT NULL CHECK (storage_key <> ''),
    profile_fingerprint text NOT NULL REFERENCES processing_profile(fingerprint),
    status text NOT NULL CHECK (status IN ('QUEUED', 'PROCESSING', 'ACTIVE', 'FAILED', 'SUPERSEDED')),
    page_count integer CHECK (page_count BETWEEN 1 AND 500),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (document_id, version_number),
    UNIQUE (document_id, id)
);

ALTER TABLE document ADD CONSTRAINT document_active_version_fk
    FOREIGN KEY (id, active_version_id)
    REFERENCES document_version(document_id, id)
    DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE processing_run (
    id uuid PRIMARY KEY,
    version_id uuid NOT NULL REFERENCES document_version(id),
    retry_of_run_id uuid REFERENCES processing_run(id),
    retry_request_key text,
    status text NOT NULL CHECK (status IN ('QUEUED', 'RUNNING', 'SUCCEEDED', 'FAILED', 'SUPERSEDED')),
    stage text NOT NULL CHECK (stage IN ('METADATA', 'PARSE', 'CHUNK', 'EMBED', 'STORE', 'ACTIVATE')),
    fencing_token bigint,
    lease_owner text,
    lease_until timestamptz,
    automatic_attempts integer NOT NULL DEFAULT 0 CHECK (automatic_attempts BETWEEN 0 AND 3),
    next_automatic_retry_at timestamptz,
    activation_outcome text NOT NULL DEFAULT 'NOT_ATTEMPTED'
        CHECK (activation_outcome IN ('ACTIVATED', 'SKIPPED_SUPERSEDED', 'NOT_ATTEMPTED')),
    error_code text,
    error_detail text,
    correlation_id text NOT NULL CHECK (correlation_id <> ''),
    queued_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    started_at timestamptz,
    finished_at timestamptz,
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE UNIQUE INDEX processing_run_one_live_per_version
    ON processing_run (version_id) WHERE status IN ('QUEUED', 'RUNNING');
CREATE UNIQUE INDEX processing_run_retry_request_unique
    ON processing_run (retry_request_key) WHERE retry_request_key IS NOT NULL;
CREATE UNIQUE INDEX processing_run_one_live_retry_per_parent
    ON processing_run (retry_of_run_id)
    WHERE retry_of_run_id IS NOT NULL AND status IN ('QUEUED', 'RUNNING');
CREATE INDEX processing_run_fifo_idx ON processing_run (queued_at, id) WHERE status = 'QUEUED';

CREATE TABLE queue_control (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    next_fencing_token bigint NOT NULL DEFAULT 1 CHECK (next_fencing_token > 0)
);
INSERT INTO queue_control(singleton, next_fencing_token) VALUES (true, 1);

CREATE TABLE upload_request (
    request_key text PRIMARY KEY CHECK (request_key <> ''),
    operation text NOT NULL CHECK (operation IN ('NEW_DOCUMENT', 'NEW_VERSION')),
    target_document_id uuid REFERENCES document(id),
    content_sha256 text NOT NULL CHECK (content_sha256 ~ '^[0-9a-f]{64}$'),
    byte_size bigint NOT NULL CHECK (byte_size BETWEEN 1 AND 104857600),
    status text NOT NULL CHECK (status IN ('PENDING', 'ACCEPTED', 'CONFLICT', 'EXPIRED')),
    document_id uuid REFERENCES document(id),
    version_id uuid REFERENCES document_version(id),
    processing_run_id uuid REFERENCES processing_run(id),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    expires_at timestamptz NOT NULL DEFAULT (clock_timestamp() + interval '24 hours'),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE processing_checkpoint (
    run_id uuid NOT NULL REFERENCES processing_run(id),
    stage text NOT NULL CHECK (stage IN ('METADATA', 'PARSE', 'CHUNK', 'EMBED', 'STORE', 'ACTIVATE')),
    input_sha256 text NOT NULL CHECK (input_sha256 ~ '^[0-9a-f]{64}$'),
    output_sha256 text NOT NULL CHECK (output_sha256 ~ '^[0-9a-f]{64}$'),
    format_version text NOT NULL CHECK (format_version <> ''),
    artifact_key text NOT NULL CHECK (artifact_key <> ''),
    artifact_size bigint NOT NULL CHECK (artifact_size >= 0),
    fencing_token bigint NOT NULL CHECK (fencing_token > 0),
    completed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (run_id, stage)
);

CREATE TABLE search_chunk (
    version_id uuid NOT NULL REFERENCES document_version(id),
    profile_fingerprint text NOT NULL REFERENCES processing_profile(fingerprint),
    chunk_index integer NOT NULL CHECK (chunk_index >= 0),
    page_number integer NOT NULL CHECK (page_number BETWEEN 1 AND 500),
    full_text text NOT NULL CHECK (full_text <> ''),
    snippet text NOT NULL,
    embedding vector(384) NOT NULL,
    staged_by_run_id uuid NOT NULL REFERENCES processing_run(id),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (version_id, profile_fingerprint, chunk_index)
);
CREATE INDEX search_chunk_version_idx ON search_chunk (version_id, profile_fingerprint);

CREATE TABLE change_log (
    sequence_id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    document_id uuid NOT NULL REFERENCES document(id),
    version_id uuid REFERENCES document_version(id),
    run_id uuid REFERENCES processing_run(id),
    event_type text NOT NULL CHECK (event_type <> ''),
    safe_detail jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX change_log_document_idx ON change_log (document_id, sequence_id);

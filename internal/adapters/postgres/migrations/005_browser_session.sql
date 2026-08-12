CREATE TABLE browser_session (
    token_hash bytea PRIMARY KEY CHECK (octet_length(token_hash) = 32),
    csrf_token text NOT NULL CHECK (char_length(csrf_token) BETWEEN 32 AND 256),
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX browser_session_expiry_idx ON browser_session (expires_at);

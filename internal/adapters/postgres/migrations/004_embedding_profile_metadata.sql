ALTER TABLE syncbase.processing_profile
    ADD COLUMN provider text NOT NULL DEFAULT 'local-onnx' CHECK (provider <> ''),
    ADD COLUMN chunk_size_tokens integer NOT NULL DEFAULT 384
        CHECK (chunk_size_tokens BETWEEN 1 AND 512),
    ADD COLUMN chunk_overlap_tokens integer NOT NULL DEFAULT 64
        CHECK (chunk_overlap_tokens BETWEEN 0 AND chunk_size_tokens);

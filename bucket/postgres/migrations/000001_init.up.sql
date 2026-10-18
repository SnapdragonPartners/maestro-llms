-- bucket schema v1 (ADR-0025). Apply with your migration tool before
-- deploying a release that requires it. The package never runs DDL.
-- Requires PostgreSQL 13+ (gen_random_uuid).

CREATE SCHEMA IF NOT EXISTS bucket;

CREATE TABLE bucket.bucket (
    id                bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name              text NOT NULL UNIQUE,
    tokens_per_minute integer NOT NULL CHECK (tokens_per_minute >= 0),
    max_concurrency   integer NOT NULL CHECK (max_concurrency >= 0),
    buffer_factor     double precision NOT NULL CHECK (buffer_factor > 0),
    token_balance     double precision NOT NULL,
    last_refill       timestamptz NOT NULL
);

COMMENT ON COLUMN bucket.bucket.tokens_per_minute IS
    'Change only with postgres.Ensure(overwrite=true): a raw UPDATE applies the new rate retroactively to unsettled refill.';
COMMENT ON COLUMN bucket.bucket.max_concurrency IS
    'Change only with postgres.Ensure(overwrite=true).';
COMMENT ON COLUMN bucket.bucket.buffer_factor IS
    'Change only with postgres.Ensure(overwrite=true): a raw UPDATE applies the new capacity retroactively to unsettled refill.';

CREATE TABLE bucket.lease (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    bucket_id   bigint NOT NULL REFERENCES bucket.bucket (id) ON DELETE CASCADE,
    acquired_at timestamptz NOT NULL,
    expires_at  timestamptz NOT NULL
);

CREATE INDEX lease_bucket_expires_idx ON bucket.lease (bucket_id, expires_at);

CREATE TABLE bucket.schema_version (
    version    integer PRIMARY KEY,
    applied_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO bucket.schema_version (version) VALUES (1);

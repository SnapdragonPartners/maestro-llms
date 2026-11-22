-- bucket schema v2 (ADR-0052): leases held by ID from another process.
-- Apply with your migration tool before deploying a release that requires
-- it. Every new column has a default, so binaries that require v1 keep
-- working during the rollout: their rows are in-process leases
-- (detached = false), which by-ID operations never touch. On PostgreSQL 11+
-- adding a column with a constant default does not rewrite the table.

ALTER TABLE bucket.lease
    ADD COLUMN detached       boolean NOT NULL DEFAULT false,
    ADD COLUMN tokens         bigint NOT NULL DEFAULT 0 CHECK (tokens >= 0),
    ADD COLUMN token_charged  boolean NOT NULL DEFAULT false,
    ADD COLUMN adjusted       boolean NOT NULL DEFAULT false,
    ADD COLUMN ttl            interval,
    ADD COLUMN max_expires_at timestamptz,
    ADD CONSTRAINT lease_detached_has_terms
        CHECK (NOT detached OR (ttl IS NOT NULL AND max_expires_at IS NOT NULL));

COMMENT ON COLUMN bucket.lease.detached IS
    'True for a lease held by ID (TryAcquireDetached). Its tokens, token_charged, adjusted, ttl and max_expires_at are the only record of its terms; in-process leases keep them in memory and leave the defaults.';

-- Refresh the v1 limit-column comments, which named only Ensure (ADR-0046
-- deferred this to the next real schema change).
COMMENT ON COLUMN bucket.bucket.tokens_per_minute IS
    'Change only with postgres.Ensure or EnsureTx (overwrite=true): a raw UPDATE applies the new rate retroactively to unsettled refill.';
COMMENT ON COLUMN bucket.bucket.max_concurrency IS
    'Change only with postgres.Ensure or EnsureTx (overwrite=true).';
COMMENT ON COLUMN bucket.bucket.buffer_factor IS
    'Change only with postgres.Ensure or EnsureTx (overwrite=true): a raw UPDATE applies the new capacity retroactively to unsettled refill.';

INSERT INTO bucket.schema_version (version) VALUES (2);

-- Reverts bucket schema v2. Detached leases become ordinary rows that
-- nothing renews, so their slots free within one LeaseTTL; their pending
-- adjustments are lost (the estimates stay charged).
DELETE FROM bucket.schema_version WHERE version = 2;

COMMENT ON COLUMN bucket.bucket.tokens_per_minute IS
    'Change only with postgres.Ensure(overwrite=true): a raw UPDATE applies the new rate retroactively to unsettled refill.';
COMMENT ON COLUMN bucket.bucket.max_concurrency IS
    'Change only with postgres.Ensure(overwrite=true).';
COMMENT ON COLUMN bucket.bucket.buffer_factor IS
    'Change only with postgres.Ensure(overwrite=true): a raw UPDATE applies the new capacity retroactively to unsettled refill.';

ALTER TABLE bucket.lease
    DROP CONSTRAINT IF EXISTS lease_detached_has_terms,
    DROP COLUMN IF EXISTS max_expires_at,
    DROP COLUMN IF EXISTS ttl,
    DROP COLUMN IF EXISTS adjusted,
    DROP COLUMN IF EXISTS token_charged,
    DROP COLUMN IF EXISTS tokens,
    DROP COLUMN IF EXISTS detached;

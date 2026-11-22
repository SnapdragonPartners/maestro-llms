-- Reverts bucket schema v2. Detached leases become ordinary rows that
-- nothing renews, so their slots free within one LeaseTTL; their pending
-- adjustments are lost (the estimates stay charged).
DELETE FROM bucket.schema_version WHERE version = 2;

ALTER TABLE bucket.lease
    DROP CONSTRAINT IF EXISTS lease_detached_has_terms,
    DROP COLUMN IF EXISTS max_expires_at,
    DROP COLUMN IF EXISTS ttl,
    DROP COLUMN IF EXISTS adjusted,
    DROP COLUMN IF EXISTS token_charged,
    DROP COLUMN IF EXISTS tokens,
    DROP COLUMN IF EXISTS detached;

package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/SnapdragonPartners/maestro-llms/bucket"
	"github.com/SnapdragonPartners/maestro-llms/bucket/internal/core"
)

// Ensure creates the bucket name with limits if it is missing; a new bucket
// starts with a full token balance. With overwrite it also installs limits
// on an existing bucket, and this is the only supported way to change
// limits: in one transaction it settles the refill accrued so far under the
// OLD limits, installs the new ones, and caps the balance at the new
// capacity (debt is kept). Leases are untouched; lowering MaxConcurrency
// preempts no one.
//
// The policy is the app's: overwrite=false at startup makes the database
// the source of truth (recommended in production); overwrite=true makes
// config win, and during a rolling deploy the last instance to start wins.
func (l *Limiter) Ensure(ctx context.Context, name string, limits bucket.Limits, overwrite bool) error {
	if err := limits.Validate(); err != nil {
		return fmt.Errorf("postgres: Ensure %q: %w", name, err)
	}
	if name == "" {
		return fmt.Errorf("postgres: Ensure: empty bucket name")
	}
	return l.inTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
INSERT INTO bucket.bucket (name, tokens_per_minute, max_concurrency, buffer_factor, token_balance, last_refill)
VALUES ($1, $2, $3, $4, $5, clock_timestamp())
ON CONFLICT (name) DO NOTHING`,
			name, limits.TokensPerMinute, limits.MaxConcurrency, limits.EffectiveBufferFactor(), float64(limits.Capacity()))
		if err != nil {
			return fmt.Errorf("bucket %q: ensure: %w", name, schemaErr(err))
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("bucket %q: ensure: %w", name, err)
		}
		if n == 1 || !overwrite {
			return nil
		}
		old, now, err := lockBucket(ctx, tx, name)
		if err != nil {
			return err
		}
		balance := newBalance(old.limits, limits, core.Settle(old.limits, old.balance, old.lastRefill, now))
		if _, err := tx.ExecContext(ctx, `
UPDATE bucket.bucket
SET tokens_per_minute = $1, max_concurrency = $2, buffer_factor = $3, token_balance = $4, last_refill = $5
WHERE id = $6`,
			limits.TokensPerMinute, limits.MaxConcurrency, limits.EffectiveBufferFactor(), balance, now, old.id); err != nil {
			return fmt.Errorf("bucket %q: ensure: %w", name, schemaErr(err))
		}
		return nil
	})
}

// newBalance is the balance after a limit change, given the balance already
// settled under the old limits. Turning token limiting on starts the bucket
// full, as a new bucket would; otherwise the balance is capped at the new
// capacity and debt is kept.
func newBalance(old, next bucket.Limits, settled float64) float64 {
	if !next.TokenLimited() {
		return 0
	}
	if !old.TokenLimited() {
		return float64(next.Capacity())
	}
	return min(settled, float64(next.Capacity()))
}

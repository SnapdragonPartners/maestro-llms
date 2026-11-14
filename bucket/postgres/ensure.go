package postgres

import (
	"context"
	"fmt"

	"github.com/SnapdragonPartners/maestro-llms/bucket"
	"github.com/SnapdragonPartners/maestro-llms/bucket/internal/core"
)

// Ensure creates the bucket name with limits if it is missing; a new bucket
// starts with a full token balance. With overwrite it also installs limits
// on an existing bucket; Ensure and EnsureTx are the only supported ways to
// change limits. In one transaction it settles the refill accrued so far under the
// OLD limits, installs the new ones, and caps the balance at the new
// capacity (debt is kept). Leases are untouched; lowering MaxConcurrency
// preempts no one.
//
// The policy is the app's: overwrite=false at startup makes the database
// the source of truth (recommended in production); overwrite=true makes
// config win, and during a rolling deploy the last instance to start wins.
//
// Ensure runs in its own transaction, bounded by OpTimeout. To change limits
// atomically with the app's own writes (a stale-save check, an audit
// record), use LockLimits and EnsureTx in the app's transaction instead.
func (l *Limiter) Ensure(ctx context.Context, name string, limits bucket.Limits, overwrite bool) error {
	if err := checkEnsure("Ensure", name, limits); err != nil {
		return err
	}
	return l.inTx(ctx, func(ctx context.Context, tx Tx) error {
		return ensureIn(ctx, tx, name, limits, overwrite)
	})
}

// checkEnsure validates Ensure's and EnsureTx's arguments before any
// database work; op names the method in the error.
func checkEnsure(op, name string, limits bucket.Limits) error {
	if err := limits.Validate(); err != nil {
		return fmt.Errorf("postgres: %s %q: %w", op, name, err)
	}
	if name == "" {
		return fmt.Errorf("postgres: %s: empty bucket name", op)
	}
	return nil
}

// ensureIn is the one implementation of Ensure's rules, run in tx (the
// limiter's own for Ensure, the caller's for EnsureTx).
func ensureIn(ctx context.Context, tx Tx, name string, limits bucket.Limits, overwrite bool) error {
	n, err := tx.Exec(ctx, `
INSERT INTO bucket.bucket (name, tokens_per_minute, max_concurrency, buffer_factor, token_balance, last_refill)
VALUES ($1, $2, $3, $4, $5, clock_timestamp())
ON CONFLICT (name) DO NOTHING`,
		name, limits.TokensPerMinute, limits.MaxConcurrency, limits.EffectiveBufferFactor(), float64(limits.Capacity()))
	if err != nil {
		return fmt.Errorf("bucket %q: ensure: %w", name, schemaErr(err))
	}
	if n == 1 || !overwrite {
		return nil
	}
	old, now, err := lockBucket(ctx, tx, name)
	if err != nil {
		return err
	}
	balance := newBalance(old.limits, limits, core.Settle(old.limits, old.balance, old.lastRefill, now))
	if _, err := tx.Exec(ctx, `
UPDATE bucket.bucket
SET tokens_per_minute = $1, max_concurrency = $2, buffer_factor = $3, token_balance = $4, last_refill = $5
WHERE id = $6`,
		limits.TokensPerMinute, limits.MaxConcurrency, limits.EffectiveBufferFactor(), balance, now, old.id); err != nil {
		return fmt.Errorf("bucket %q: ensure: %w", name, schemaErr(err))
	}
	return nil
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

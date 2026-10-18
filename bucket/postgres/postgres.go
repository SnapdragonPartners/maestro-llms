// Package postgres is a bucket.Limiter shared across processes through
// PostgreSQL 13+ (ADR-0025).
//
// Limits live in the database (bucket.bucket), so every instance sees the
// same limits and a change applies everywhere on the next acquire. Each held
// concurrency slot is a row in bucket.lease with an expiry that the lease
// renews in the background; a crashed holder's slot frees within one
// LeaseTTL. All timestamps come from the database clock, sampled after the
// bucket row lock is taken.
//
// The package imports no database driver: pass a *sql.DB (pgx users:
// stdlib.OpenDBFromPool). It never runs DDL: apply Migrations() with your
// own migration tool. New fails with *SchemaError when the schema is behind.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/bucket"
	"github.com/SnapdragonPartners/maestro-llms/bucket/internal/core"
)

// Defaults for Config.
const (
	DefaultLeaseTTL         = 30 * time.Second
	DefaultMaxLeaseDuration = 15 * time.Minute
	DefaultOpTimeout        = 5 * time.Second
	DefaultPollInterval     = 100 * time.Millisecond
	DefaultMaxPollInterval  = 2 * time.Second
)

// Config configures a Limiter. The zero value uses every default.
type Config struct {
	// LeaseTTL is how long a slot outlives its last renewal; renewal runs
	// every LeaseTTL/3. A crashed holder's slot frees within one LeaseTTL.
	LeaseTTL time.Duration
	// MaxLeaseDuration caps how long a lease is renewed, so a holder that
	// never releases cannot keep a slot forever. It must exceed the longest
	// legitimate call.
	MaxLeaseDuration time.Duration
	// OpTimeout bounds every database operation (each acquire attempt,
	// Adjust, renewal, Release, Ensure), including lock waits.
	OpTimeout time.Duration
	// MaxWait bounds how long Acquire blocks; 0 waits until ctx is done.
	MaxWait time.Duration
	// PollInterval is the first re-check delay while blocked; it doubles
	// up to MaxPollInterval.
	PollInterval    time.Duration
	MaxPollInterval time.Duration
}

func (c Config) withDefaults() Config {
	if c.LeaseTTL <= 0 {
		c.LeaseTTL = DefaultLeaseTTL
	}
	if c.MaxLeaseDuration <= 0 {
		c.MaxLeaseDuration = DefaultMaxLeaseDuration
	}
	if c.OpTimeout <= 0 {
		c.OpTimeout = DefaultOpTimeout
	}
	if c.PollInterval <= 0 {
		c.PollInterval = DefaultPollInterval
	}
	if c.MaxPollInterval <= 0 {
		c.MaxPollInterval = DefaultMaxPollInterval
	}
	c.MaxPollInterval = max(c.MaxPollInterval, c.PollInterval)
	return c
}

func (c Config) validate() error {
	if c.MaxLeaseDuration <= c.LeaseTTL {
		return fmt.Errorf("postgres: MaxLeaseDuration %v must exceed LeaseTTL %v", c.MaxLeaseDuration, c.LeaseTTL)
	}
	if c.LeaseTTL/3 <= 0 {
		return fmt.Errorf("postgres: LeaseTTL %v is too short to renew", c.LeaseTTL)
	}
	return nil
}

// Limiter is a bucket.Limiter backed by PostgreSQL. It is safe for
// concurrent use; any number of Limiters across processes may share one
// database.
type Limiter struct {
	db  *sql.DB
	cfg Config
}

// New builds a Limiter over db. It checks the schema first and returns
// *SchemaError when it is missing or older than SchemaVersion, so a missed
// migration fails at startup rather than on the first acquire.
func New(ctx context.Context, db *sql.DB, cfg Config) (*Limiter, error) {
	if db == nil {
		return nil, errors.New("postgres: nil *sql.DB")
	}
	cfg = cfg.withDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	opCtx, cancel := context.WithTimeout(ctx, cfg.OpTimeout)
	defer cancel()
	if err := CheckSchema(opCtx, db); err != nil {
		return nil, err
	}
	return &Limiter{db: db, cfg: cfg}, nil
}

// micros converts d to whole microseconds for SQL interval arithmetic.
func micros(d time.Duration) int64 { return d.Microseconds() }

// inTx runs fn in a transaction bounded by OpTimeout, with lock waits
// bounded the same way. fn's error rolls back; a nil error commits.
func (l *Limiter) inTx(ctx context.Context, fn func(context.Context, *sql.Tx) error) error {
	opCtx, cancel := context.WithTimeout(ctx, l.cfg.OpTimeout)
	defer cancel()
	tx, err := l.db.BeginTx(opCtx, nil)
	if err != nil {
		return fmt.Errorf("bucket: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// SET LOCAL takes no bind parameters; the value is an integer we format.
	if _, err := tx.ExecContext(opCtx, fmt.Sprintf("SET LOCAL lock_timeout = %d", l.cfg.OpTimeout.Milliseconds())); err != nil {
		return fmt.Errorf("bucket: set lock_timeout: %w", err)
	}
	if err := fn(opCtx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("bucket: commit: %w", err)
	}
	return nil
}

// row is a locked bucket row.
type row struct {
	lastRefill time.Time
	limits     bucket.Limits
	balance    float64
	id         int64
}

// lockBucket locks key's row and returns it with the database time sampled
// after the lock was granted (a separate statement, so a long lock wait can
// never yield a stale timestamp). A missing row is an unknown-key error.
func lockBucket(ctx context.Context, tx *sql.Tx, key string) (row, time.Time, error) {
	return lockRow(ctx, tx, key, `WHERE name = $1`, key)
}

// lockBucketByID is lockBucket for a lease's bucket, by its stable id, so a
// renamed bucket keeps reconciling.
func lockBucketByID(ctx context.Context, tx *sql.Tx, key string, id int64) (row, time.Time, error) {
	return lockRow(ctx, tx, key, `WHERE id = $1`, id)
}

func lockRow(ctx context.Context, tx *sql.Tx, key, where string, arg any) (row, time.Time, error) {
	var r row
	err := tx.QueryRowContext(ctx, `
SELECT id, tokens_per_minute, max_concurrency, buffer_factor, token_balance, last_refill
FROM bucket.bucket `+where+` FOR UPDATE`, arg).Scan(
		&r.id, &r.limits.TokensPerMinute, &r.limits.MaxConcurrency, &r.limits.BufferFactor, &r.balance, &r.lastRefill)
	if errors.Is(err, sql.ErrNoRows) {
		return row{}, time.Time{}, bucket.UnknownKey(key)
	}
	if err != nil {
		return row{}, time.Time{}, fmt.Errorf("bucket %q: lock: %w", key, schemaErr(err))
	}
	var now time.Time
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return row{}, time.Time{}, fmt.Errorf("bucket %q: clock: %w", key, err)
	}
	return r, now, nil
}

// Acquire implements bucket.Limiter.
func (l *Limiter) Acquire(ctx context.Context, key string, tokens int) (bucket.Lease, error) {
	tokens = max(tokens, 0)
	w := core.Waiter{MaxWait: l.cfg.MaxWait, Initial: l.cfg.PollInterval, Max: l.cfg.MaxPollInterval}
	return core.Wait(ctx, key, w, func(ctx context.Context) (core.Result, error) {
		return l.tryAcquire(ctx, key, tokens)
	})
}

// tryAcquire is one admission attempt: lock, refill, reap expired leases,
// count live ones, and admit if tokens and a slot fit. It commits either
// way, so the reap and refill persist.
func (l *Limiter) tryAcquire(ctx context.Context, key string, tokens int) (core.Result, error) {
	var res core.Result
	var leaseID string
	var bucketID int64
	var tokenCharged bool
	err := l.inTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		r, now, err := lockBucket(ctx, tx, key)
		if err != nil {
			return err
		}
		if reason := core.Impossible(r.limits, tokens); reason != "" {
			res.Impossible = reason
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM bucket.lease WHERE bucket_id = $1 AND expires_at <= $2`, r.id, now); err != nil {
			return fmt.Errorf("bucket %q: reap: %w", key, schemaErr(err))
		}
		var live int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM bucket.lease WHERE bucket_id = $1`, r.id).Scan(&live); err != nil {
			return fmt.Errorf("bucket %q: count: %w", key, schemaErr(err))
		}
		balance := core.Settle(r.limits, r.balance, r.lastRefill, now)
		fits := core.Fits(r.limits, balance, tokens, live)
		if fits && r.limits.TokenLimited() {
			balance -= float64(tokens)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE bucket.bucket SET token_balance = $1, last_refill = $2 WHERE id = $3`, balance, now, r.id); err != nil {
			return fmt.Errorf("bucket %q: refill: %w", key, schemaErr(err))
		}
		if !fits {
			res.RetryAfter = core.RetryAfter(r.limits, balance, tokens)
			return nil
		}
		// Fresh database time for both stamps: the lease is live for a full
		// TTL from the moment it is written.
		if err := tx.QueryRowContext(ctx, `
INSERT INTO bucket.lease (bucket_id, acquired_at, expires_at)
VALUES ($1, clock_timestamp(), clock_timestamp() + $2::float8 * interval '1 microsecond')
RETURNING id`, r.id, micros(l.cfg.LeaseTTL)).Scan(&leaseID); err != nil {
			return fmt.Errorf("bucket %q: insert lease: %w", key, schemaErr(err))
		}
		bucketID = r.id
		tokenCharged = r.limits.TokenLimited()
		return nil
	})
	if err != nil {
		return core.Result{}, err
	}
	if leaseID != "" {
		res.Lease = l.newLease(ctx, key, leaseID, bucketID, tokens, tokenCharged)
	}
	return res, nil
}

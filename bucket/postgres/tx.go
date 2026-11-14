package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/SnapdragonPartners/maestro-llms/bucket"
)

// Tx is the part of a transaction the package needs, so an app can run
// EnsureTx and LockLimits in a transaction it owns, alongside its own writes
// (ADR-0046). It is deliberately neither *sql.Tx nor pgx.Tx, so the package
// takes on no driver: wrap a *sql.Tx with SQLTx, and a pgx.Tx with a few
// lines of your own:
//
//	type pgxTx struct{ tx pgx.Tx }
//
//	func (t pgxTx) Exec(ctx context.Context, q string, args ...any) (int64, error) {
//		tag, err := t.tx.Exec(ctx, q, args...)
//		return tag.RowsAffected(), err
//	}
//
//	func (t pgxTx) QueryRow(ctx context.Context, q string, args ...any) postgres.Row {
//		return t.tx.QueryRow(ctx, q, args...)
//	}
//
// Queries use $n placeholders and pass int, float64, string and time.Time
// arguments.
type Tx interface {
	// Exec runs a statement and returns the number of rows it affected.
	Exec(ctx context.Context, query string, args ...any) (rowsAffected int64, err error)
	// QueryRow runs a query expected to return at most one row.
	QueryRow(ctx context.Context, query string, args ...any) Row
}

// Row is one result row. When the query returned no row, Scan must return an
// error that wraps sql.ErrNoRows; database/sql does, and so does pgx from
// v5.6 (pgx.ErrNoRows wraps it). Database errors that implement
// SQLState() string (pgx and lib/pq errors do) let a missing schema surface
// as *SchemaError.
type Row interface {
	Scan(dest ...any) error
}

// SQLTx adapts a *sql.Tx to Tx.
func SQLTx(tx *sql.Tx) Tx { return sqlTx{tx} }

type sqlTx struct{ tx *sql.Tx }

func (t sqlTx) Exec(ctx context.Context, query string, args ...any) (int64, error) {
	res, err := t.tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err //nolint:wrapcheck // a pass-through adapter; callers add context
	}
	return res.RowsAffected() //nolint:wrapcheck // a pass-through adapter; callers add context
}

func (t sqlTx) QueryRow(ctx context.Context, query string, args ...any) Row {
	return t.tx.QueryRowContext(ctx, query, args...)
}

// Limits returns name's limits as stored, without a lock: what an admin page
// draws its form from. BufferFactor is the effective factor Ensure stored, so
// it is never 0; compare stored limits only with values read back (Limits or
// LockLimits), not with the Limits the app passed to Ensure. A missing
// bucket returns an error wrapping bucket.ErrUnknownKey. It is bounded by
// OpTimeout.
func (l *Limiter) Limits(ctx context.Context, name string) (bucket.Limits, error) {
	opCtx, cancel := context.WithTimeout(ctx, l.cfg.OpTimeout)
	defer cancel()
	return selectLimits(l.db.QueryRowContext(opCtx, limitsQuery, name), name)
}

// LockLimits locks name's bucket row in tx (SELECT ... FOR UPDATE) and
// returns its limits as Limits does, for a compare-then-EnsureTx save: the
// lock holds until tx ends, so no other save, and no Acquire or Adjust on
// the bucket, can interleave. A missing bucket returns an error wrapping
// bucket.ErrUnknownKey.
//
// The caller owns tx: this neither begins, commits nor rolls it back, and
// sets no lock timeout (bound the wait with ctx, or SET LOCAL lock_timeout
// in your transaction). Keep the transaction short once the lock is held:
// every Acquire and Adjust on the bucket waits behind it, and fails after
// OpTimeout, which for Adjust means the call's actual usage goes
// unrecorded.
func (l *Limiter) LockLimits(ctx context.Context, tx Tx, name string) (bucket.Limits, error) {
	if tx == nil {
		return bucket.Limits{}, errors.New("postgres: LockLimits: nil Tx")
	}
	return selectLimits(tx.QueryRow(ctx, limitsQuery+` FOR UPDATE`, name), name)
}

// EnsureTx is Ensure in a transaction the caller owns, with the same rules:
// it validates first and touches tx only when name and limits are valid;
// it creates a missing bucket full; with overwrite it locks the row, settles
// the refill accrued under the old limits, installs the new ones and caps
// the balance at the new capacity (debt is kept). Rolling tx back undoes it.
//
// The caller owns tx as for LockLimits: no begin, commit, rollback or lock
// timeout here, and the row lock it takes holds until tx ends. Under
// REPEATABLE READ or SERIALIZABLE, a row changed since tx's snapshot fails
// with a serialization error the caller retries; READ COMMITTED (the
// default) waits for the lock instead.
func (l *Limiter) EnsureTx(ctx context.Context, tx Tx, name string, limits bucket.Limits, overwrite bool) error {
	if err := checkEnsure(name, limits); err != nil {
		return err
	}
	if tx == nil {
		return errors.New("postgres: EnsureTx: nil Tx")
	}
	return ensureIn(ctx, tx, name, limits, overwrite)
}

const limitsQuery = `SELECT tokens_per_minute, max_concurrency, buffer_factor FROM bucket.bucket WHERE name = $1`

func selectLimits(r Row, name string) (bucket.Limits, error) {
	var lim bucket.Limits
	err := r.Scan(&lim.TokensPerMinute, &lim.MaxConcurrency, &lim.BufferFactor)
	if errors.Is(err, sql.ErrNoRows) {
		return bucket.Limits{}, bucket.UnknownKey(name)
	}
	if err != nil {
		return bucket.Limits{}, fmt.Errorf("bucket %q: read limits: %w", name, schemaErr(err))
	}
	return lim, nil
}

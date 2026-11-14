# 0046. `bucket/postgres`: limit changes in a caller's transaction (`EnsureTx`, `LockLimits`, `Limits`)

- **Status:** Accepted; amends [ADR-0025](0025-bucket-distributed-limiter.md)
- **Date:** 2026-11-14

## Context

ADR-0025 puts a bucket's limits in the database (`bucket.bucket`) and makes
`Ensure(ctx, name, limits, overwrite=true)` the only supported way to change
them, because the change must settle refill under the old rate first.
`Ensure` always runs in a transaction the limiter opens itself.

Morris is adding an admin page that edits those limits (#170, Morris ADR
0058). The bucket row is the only record of them. In Morris, every settings
save is one transaction that:

1. locks the value and refuses the save if another admin changed it after
   the form was drawn;
2. writes the new value;
3. records an audit event (who, old and new limits).

One "Save All" can cover several settings, still in one transaction.
`Ensure` can't take part: the stale check and the write can't happen under
one lock, so two admins can overwrite each other silently, and the limit
change and its audit record can commit separately. Nothing exported reads a
bucket's limits either, locked or not. Copying the balance rules into
Morris to write the row itself would fork the one implementation ADR-0025
relies on.

## Decision

Three methods on `*postgres.Limiter`, and a two-method transaction
interface. `bucket.Limiter` and `bucket/memory` are unchanged.

```go
type Tx interface {
    Exec(ctx context.Context, query string, args ...any) (rowsAffected int64, err error)
    QueryRow(ctx context.Context, query string, args ...any) Row
}
type Row interface{ Scan(dest ...any) error }
func SQLTx(tx *sql.Tx) Tx

func (l *Limiter) Limits(ctx context.Context, name string) (bucket.Limits, error)
func (l *Limiter) LockLimits(ctx context.Context, tx Tx, name string) (bucket.Limits, error)
func (l *Limiter) EnsureTx(ctx context.Context, tx Tx, name string, limits bucket.Limits, overwrite bool) error
```

- **`Tx` is ours, not a driver's.** The package imports no driver
  (ADR-0025), and Morris's settings transaction is a `pgx.Tx`, which can't
  become a `*sql.Tx`. So the package declares the minimum it needs. `Exec`
  returns rows affected rather than `sql.Result` because a pgx adapter can't
  produce a `sql.Result`; with an `int64` the adapter is two short methods,
  shown in `Tx`'s doc comment and tested over a native `pgx.Tx`. `SQLTx`
  adapts `*sql.Tx`; Go has no covariant returns, so `*sql.Tx` can't satisfy
  `Tx` directly. Internally, every limiter transaction now runs through the
  same interface, so `Ensure` and `EnsureTx` share one code path.
- **`Row`'s contract is written down.** Scanning no row must return an
  error wrapping `sql.ErrNoRows` (database/sql does; pgx does from v5.6),
  which becomes `bucket.ErrUnknownKey`. Errors exposing `SQLState()` keep
  mapping a missing schema to `*SchemaError`.
- **`EnsureTx` has `Ensure`'s rules exactly.** It validates first and
  touches the transaction only for valid arguments; it inserts a missing
  bucket full; with overwrite it locks the row, settles refill under the old
  limits, installs the new ones and caps the balance (debt kept). `Ensure` is
  now `inTx` plus that same core.
- **The caller owns the transaction.** `EnsureTx` and `LockLimits` never
  begin, commit or roll back, and set no `lock_timeout`: `SET LOCAL` would
  change the caller's transaction for its remaining statements. The caller
  bounds lock waits with `ctx` or its own `lock_timeout`. Under REPEATABLE
  READ or SERIALIZABLE a concurrently changed row surfaces as a
  serialization failure the caller retries.
- **Reads return the stored limits.** `Ensure` stores the effective buffer
  factor, so a stored `BufferFactor` is never 0. `Limits` (unlocked, bounded
  by `OpTimeout`, for drawing the form) and `LockLimits` return the stored
  values, and a stale check compares stored values with stored values,
  never with the `Limits` the app passed to `Ensure`. It compares the fields
  one by one: `bucket.Limits` is not on the README's named comparability
  list, so `==` on it is not promised to keep compiling. The row has no version column; comparing values is enough,
  because an A→B→A change leaves nothing to lose.
- **Methods, not package functions.** None of the three needs a second
  database handle, but holding a `*Limiter` means `New` already ran
  `CheckSchema`, so the schema-version guarantee covers them too.

## Consequences

- An app can lock, compare, change a bucket's limits and write its own
  audit record in one transaction, and roll all of it back together.
- **The row lock blocks the bucket's traffic.** While the caller's
  transaction holds the lock, every `Acquire` and `Adjust` on that bucket
  waits and fails after `OpTimeout` (5s by default). A failed `Adjust` loses
  the actual usage of a call that already ran. Renewal and `Release` don't
  touch the bucket row and are unaffected. Callers lock last, write their
  audit record, commit, and don't wait on anything while holding the lock.
  The doc comments say so.
- No schema change, so no `SchemaVersion` bump. Additive under the v1
  stability contract.
- No `MAESTRO_DIVERGENCES.md` row: `bucket` has no Maestro counterpart.

## References

- [ADR-0025](0025-bucket-distributed-limiter.md) (limits in the database,
  `Ensure`, lock scope, `OpTimeout`)
- #170 (the Morris request)

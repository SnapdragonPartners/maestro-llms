# 0025. Generic `bucket` limiter with a Postgres backend (lease rows, renewal, FIFO queue planned)

- **Status:** Accepted; amended by [ADR-0033](0033-limit-error-classification.md) (`bucket.LimitError` gains `Permanent` and `Cause`; `FromBucket` maps both) and [ADR-0034](0034-limiter-config-and-charging.md) (`Limits.Validate` rejects non-finite and overflowing values; `InMemoryLimiter` sizes through `Limits`)
- **Date:** 2026-10-18

## Context

The spec defines the rate-limit reservation protocol (`ratelimit.Limiter`:
`Reserve` → `Commit` → always `Release`) and ships only a process-local
`InMemoryLimiter`. It deliberately left distributed limiting to the apps:

> Morris can implement it in its own repo using PostgreSQL; later, a generic
> SQL/Redis implementation can be added if it remains app-neutral.

and, in the v0.1 scope: "Do not include Morris distributed rate limiting in
the shared package yet."

That never happened, and three consumers now need a shared limiter at once,
for different reasons (#100):

- **buggrit** runs as a GitHub app. Every use shares LunaRoute's limit of 2
  concurrent requests and there is no token limit, so concurrency is the
  whole problem.
- **Morris** is preparing to ship on multi-instance Cloud Run, where a
  per-process limiter hands every instance the full budget.
- **Maestro** is building its cloud version.

All three run Postgres (Alloy is a possible later move). Building the same
limiter three times, or building it in Morris first and promoting it later,
costs more than building it once here.

The interface an app needs from a token/concurrency limiter is small and is
not LLM-specific: "block until key K has room for N tokens and one slot, then
give me a handle to give the slot back." All the hard parts are in the
backend. So the limiter should be generic, and usable for any API with token
or concurrency limits, not only LLM providers.

### Why concurrency is the hard part

Tokens use a refill-over-time bucket: they are taken at acquire and refill on
the clock. A holder that dies loses nothing that matters; at worst an
overestimate is never refunded, which errs safe.

Concurrency is a count of held slots, and a slot is freed only by `Release`.
In process that is safe for two reasons:

1. The middleware always defers `Release`.
2. A dead process takes its counter with it.

A shared counter loses the second property. An instance killed mid-call (an
out-of-memory kill or a Cloud Run scale-down) would hold its slot forever.
With buggrit's limit of 2, two such crashes block LunaRoute for every
instance.

## Decision

### 1. A top-level `bucket` package that does not import `llms`

```go
package bucket

type Limiter interface {
    // Acquire blocks until key has capacity for tokens and one concurrency
    // slot, or until ctx is done or the backend's MaxWait elapses.
    Acquire(ctx context.Context, key string, tokens int) (Lease, error)
}

type Lease interface {
    // Adjust reconciles the actual token count against the acquired one:
    // it refunds a surplus and charges a shortfall (the balance may go
    // negative and is repaid by refill). It is a no-op after the first
    // call. It does not free the slot.
    Adjust(ctx context.Context, actualTokens int) error
    // Release frees the slot. It is idempotent, safe after Adjust, and
    // safe on a context with no deadline: backends bound it internally.
    Release(ctx context.Context) error
}

// Limits are what every user of a key must agree on. Backends store
// them per key; a shared backend stores them in the shared store.
type Limits struct {
    TokensPerMinute int     // 0 = no token limit
    MaxConcurrency  int     // 0 = no concurrency limit
    BufferFactor    float64 // capacity = TokensPerMinute * BufferFactor; default 0.9
}
```

- **Per-key limits, and who owns them.** `Limits` holds only what all
  instances must agree on. Caller policy is not in `Limits`: `MaxWait`, how
  long a caller will block (0 = until ctx is done), is a backend option, and
  `ctx` can shorten it per call. The in-memory backend takes its limits from
  a map from key to `Limits`. The Postgres backend reads them from the
  database (Decision 3), so changing one takes effect on every instance at
  once and instances can't disagree. A key with no limits configured is an
  error (fail closed: it's a configuration bug, not a reason to run
  unlimited). A lease always holds exactly one slot.
- **Rates are tokens per minute.** Providers set quotas over different
  windows: per second, per minute, sometimes per day. `bucket` standardizes
  on tokens per minute, and converting is the app's job. The conversion isn't
  exact. Refill is continuous, at `TokensPerMinute/60` per second, but the
  burst capacity is a full minute's worth
  (`TokensPerMinute * BufferFactor`). So a provider limit of X tokens per
  second, entered as 60·X per minute, admits up to about 60·X tokens in one
  burst, which a per-second provider can reject. Apps facing per-second
  quotas should lower the rate or the buffer factor accordingly, and the
  provider's own 429 stays the backstop. Day-long quotas are not modeled. A
  separate burst-capacity field, independent of the rate, is the likely
  extension if a consumer needs it; adding it is additive.
- **Errors.** `*bucket.LimitError{Key, Reason, RetryAfter}` means "not
  admitted" (timed out, or the request can never fit). Any other error is a
  backend failure, for example the database is unreachable. Backend
  failures **fail closed**: no lease, so no provider call. An app that
  prefers to fail open can wrap the limiter itself.
- **Import direction.** `bucket` must not import `llms`, and a test enforces
  it. The dependency runs the other way: `llms/ratelimit` imports `bucket`.
- **Packages:** `bucket` (the interfaces, `Limits`, `LimitError`),
  `bucket/memory` (process-local) and `bucket/postgres`.

It lives in this module, not a separate one. Go compiles only the packages a
consumer imports, so a non-LLM consumer pays nothing beyond `go.sum`
entries, and one module keeps a single release and Dependabot stream.
Splitting it out later would change the import path; we accept that because
all current consumers use it with LLMs.

### 2. The adapter to `llms/ratelimit`

```go
func ratelimit.FromBucket(l bucket.Limiter, key func(ReservationRequest) string) Limiter
```

The adapter maps the calls one to one:

- `Reserve` calls `Acquire` with `key(req)` and `req.EstimatedUnits.Tokens()`.
- `Commit` calls `Adjust` with the same effective-token count
  `InMemoryLimiter` uses.
- `Release` calls `Release`.

**A lease is always exactly one slot**, while `UsageUnits.Slots()` can be
greater than 1, and `InMemoryLimiter` honours that weight. Acquiring one slot
for a heavier reservation would silently undercount capacity, so the adapter
rejects `Slots() > 1` **before** acquiring. It returns a non-retryable error
that names the unsupported weight. This is not a `LimitError`: retrying
would not help. Every estimator in this package sets `Requests: 1`, and no
current consumer sets more. If a consumer needs weighted slots, the addition
is an optional interface
(`bucket.WeightedLimiter.AcquireN(ctx, key, tokens, slots)`), discovered by
type assertion, so `Limiter` doesn't change.

A `*bucket.LimitError` becomes a `*llms.LimitError` carrying the request's
Provider and Model and the same `RetryAfter`, so retry and circuit
middleware treat it exactly as they treat today's local rejections.
The `key` function is the app's policy. buggrit would map every request to
`"lunaroute"`; Morris might use provider plus model. The middleware does not
change. `InMemoryLimiter` stays as it is; reimplementing it on
`bucket/memory` is left to the pre-v1 cleanup (#82) and is not required.

### 3. Postgres backend: token bucket and slots in one transaction

`bucket/postgres.New(ctx, db *sql.DB, cfg)` takes an app-supplied `*sql.DB`
and imports no database driver. pgx users pass
`stdlib.OpenDBFromPool(pool)`. It requires PostgreSQL 13 or later, for
`gen_random_uuid()`.

Two tables, in the dedicated `bucket` Postgres schema (see "Schema delivery
and versioning" below), plus its version table:

- `bucket.bucket`: one row per key, holding both its limits and its state:
  `id` (bigint identity primary key), `name` (unique),
  `tokens_per_minute`, `max_concurrency`, `buffer_factor`, `token_balance`
  and `last_refill`;
- `bucket.lease`: one row per held slot, holding the lease ID, a
  `bucket_id` that references `bucket.bucket(id)` with `ON DELETE CASCADE`,
  `expires_at` and `acquired_at`.

**The database owns the limits.** Every acquire reads them from the row it
already locks, so storing them costs nothing extra.
- *Changing a limit* means calling `Ensure` with `overwrite=true`. It takes
  effect on the next acquire everywhere. Lowering `max_concurrency` below
  the slots currently held preempts no one; new acquires wait until the
  count falls under the new limit. **Raw `UPDATE`s of the limit columns are
  unsupported** (the schema's column comments say so). Refill is computed
  lazily, from `last_refill` to now at the row's *current* rate. Changing
  the rate without first settling the refill would apply the new rate
  retroactively. For example, a balance of -600 at 60 TPM, raised to 600 TPM
  sixty seconds later, would come back at 0 instead of -540, erasing usage
  debt.
- *Deleting a bucket* (cleanup, or a full reset) cascades to its leases.
  Holders still in flight find their lease gone on their next renewal
  (`ErrLeaseLost`, Decision 4). Acquiring on a deleted key fails closed
  until it is ensured again.
- *The surrogate `id`* lets a bucket be renamed without touching its leases
  and keeps lease rows small.

Buckets are created or updated with
`postgres.Ensure(ctx, name string, limits bucket.Limits, overwrite bool)`.
With `overwrite` false it inserts only when the bucket is missing
(`ON CONFLICT DO NOTHING`). With `overwrite` true it changes the limits in
one transaction:

1. lock the row;
2. settle the refill accrued so far under the **old** limits, capped at the
   old capacity;
3. set `last_refill` to the current time;
4. install the new limits;
5. cap the balance at the new capacity, so a lower limit takes effect
   immediately. A negative balance (debt) is kept as is.

Leases are untouched. A new bucket starts with a full balance. The package
builds no policy into
this. The docs recommend `overwrite=false` at startup in production, so the
database is the source of truth and is changed deliberately. With
`overwrite=true` config wins, and during a rolling deploy whichever instance
starts last wins, so old and new values may alternate briefly until the
rollout finishes.

Each attempt to acquire is one short transaction:

1. Lock the bucket's row by name with `SELECT … FOR UPDATE`. If there is no
   row, return the unknown-key error. The lock serializes callers per key,
   never across keys.
2. Refill the token balance from the elapsed time, using the limits in the
   row.
3. Delete that key's expired leases.
4. Count the leases still live.
5. If the tokens and a slot both fit, debit the tokens, insert the lease,
   and commit. Otherwise commit only the reap and the refill, and return
   the `RetryAfter` hint.

**All time comes from the database, sampled after the lock is acquired.**
No instance's clock enters the calculation, so clock skew across instances
does not matter. Every timestamp the backend computes uses
`clock_timestamp()`, not `now()`. `now()` is the *transaction start* time,
and an acquire that waited on the bucket lock for longer than `LeaseTTL`
would insert a lease that had already expired. Another acquire could then
reclaim it immediately while the admitted call runs, breaking the cap with
no renewal failure to report it. This applies to refill, expiry checks,
`expires_at` on insert, renewal and `Ensure`.

**Lock scope.** Only acquire and `Ensure` take the bucket row lock.
Renewal and `Release` touch only their own lease row. Renewal changes
`expires_at`, which is not a key column, so Postgres does not recheck the
foreign key, and deleting a child row takes no lock on its parent. So
renewal and release never queue behind contention among acquirers. If a
renewal races an acquire that is deleting the same expired lease, the
renewal waits for that row, finds it deleted, updates 0 rows and reports
the lease lost, which is the correct outcome.

A request larger than the key's capacity fails immediately with
`LimitError`, since it can never fit.

#### Schema delivery and versioning

**The package never runs DDL.** The app applies the schema with its own
migration tool and its own migration credentials, so a least-privilege app
role keeps working and the app's tooling sees every table. Creating tables
on the fly, and shipping a `postgres.Migrate` runner, were both considered
and set aside for now; see the alternatives.

- **The schema has its own version, separate from the app's.** The bucket
  schema lives in a dedicated Postgres schema, `bucket`: `bucket.bucket`,
  `bucket.lease` and `bucket.schema_version`. Its names can't collide with
  app tables, and an app upgrading its own schema never touches it. The
  name `bucket` is fixed, not configurable. A configurable name would mean
  templated SQL, and the files could no longer be copied verbatim into a
  migration tool.
- **Versioned migration files, no runner.** `postgres.Migrations()` returns
  an embedded `fs.FS` of numbered files in golang-migrate naming
  (`000001_init.up.sql` / `.down.sql`). Apps copy them into their own
  migrations or read them from the `fs.FS`. Each file's last statement
  records its version in `bucket.schema_version`, so the version a database
  reports is whatever was actually applied, whichever tool applied it.
- **The code declares the version it needs:** `postgres.SchemaVersion`, a
  constant. A unit test fails if it isn't equal to the highest migration
  file number, so the constant and the files can't drift apart.
- **Fail fast on a schema that's behind.** `postgres.New` reads
  `bucket.schema_version`. If the table is missing or its version is below
  `SchemaVersion`, `New` returns `*postgres.SchemaError`, naming the
  required and found versions and the migration files to apply, and no
  limiter is built. The app finds out at startup, not on its first provider
  call.
  - The same check is exported as `postgres.CheckSchema(ctx, db)` for
    readiness probes and integration tests. That matters because unit tests
    that use fakes or the in-memory backend pass whether or not the
    migration was applied.
  - Acquire also re-checks lazily: if a statement fails because a table or
    column is missing (SQLSTATE `42P01` / `42703`), it returns the same
    `SchemaError` instead of a raw driver error. That covers a database
    restored or rolled back underneath a running instance.
  - Until the migration is applied, every attempt keeps failing closed with
    that error. Nothing degrades silently.
- **A newer schema is accepted.** A version above `SchemaVersion` is fine.
  The migrations are expand-only: they add tables and columns, and never
  rename or drop anything an older release reads. So during a rolling
  deploy an old binary keeps working against a migrated database. The
  rollout order is therefore: apply the migration first, then deploy. A
  migration that has to contract (drop or rename) is a breaking release, and
  its notes say so.
- **Release notes are loud.** Any release that raises `SchemaVersion` opens
  its notes with a **"Database migration required"** section: the new
  version, the files to apply, the rollout order above, and the
  `SchemaError` an upgrader will see if they skip the migration. The
  GitHub release title carries the same flag. This rule is also recorded in
  the CLAUDE.md "Versioning" section, so it doesn't depend on someone
  remembering this ADR.

### 4. Slot leases: renewal, lost leases and a hard cap

- **Renewal.** A lease row expires at `clock_timestamp() + LeaseTTL`, 30 seconds by
  default, configurable per backend. While the lease is held, one goroutine
  per lease extends it every `LeaseTTL/3`. If the process dies, renewal
  stops and the slot frees within one TTL, however long calls run. Without
  renewal the TTL would have to exceed the longest possible call (minutes,
  with reasoning models), and a crashed slot would stay held that long.
- **The renewal goroutine's context.** It derives from
  `context.WithoutCancel(acquireCtx)` plus the lease's own cancel func: it
  keeps the context's values and ignores its cancellation. `Release`
  cancels it, waits for it to exit, then deletes the row. The renewal is
  owned by the backend's `Lease`; callers never write renewal code, and
  this package's implementation is the reference.
- **A transient renewal failure** (database unreachable) is retried on the
  next tick. Renewing at TTL/3 leaves two retries before the lease expires.
- **A lost lease.** Renewal runs
  `UPDATE … SET expires_at = clock_timestamp() + ttl WHERE id = $1 AND expires_at > clock_timestamp()`.
  If no row updates, the lease has expired and may already have been
  reclaimed. Renewal then stops. `Release` returns `bucket.ErrLeaseLost`
  (informational; the middleware ignores Release errors), and `Lease` also
  exposes `Lost() <-chan struct{}` for callers that want to abort work. The
  harm is bounded: at most one call runs over the limit, for the rest of that
  call.
- **A hard cap: `MaxLeaseDuration`,** default 15 minutes. Renewal turns a
  leaked `Release` (a holder that never releases) into a slot held forever,
  where TTL alone would have recovered it. After `MaxLeaseDuration` from
  acquire, renewal stops and the lease is marked lost (`Lost()` closes), since
  the slot may now be reassigned. The database enforces the same cap: renewal sets `expires_at` to the earlier
  of `clock_timestamp() + LeaseTTL` and `acquired_at + MaxLeaseDuration`.
  The cap must exceed the longest legitimate call; the per-attempt Timeout
  middleware normally bounds that.
- **Every database operation has an internal deadline.** That covers each
  acquire attempt, `Adjust`, each renewal, `Release` and `Ensure`. The
  deadline is `OpTimeout`, 5 seconds by default. Acquire attempts and
  `Ensure` also stop if the caller's context ends first. `Adjust`, renewal
  and `Release` ignore the caller's cancellation (see below). Inside each
  transaction, `SET LOCAL lock_timeout` stops a lock wait from outlasting
  it. The middleware calls `Commit` (so `Adjust`) and `Release` on
  `context.WithoutCancel(ctx)`, which has no deadline. Without these limits
  a hung database would hang `Complete` in `Adjust`, before its deferred
  `Release` ever runs, and a blocked renewal would never get to retry.
  - The internal deadline derives from the context it was given, so it keeps
    that context's values and can't be cut short by the request being
    canceled.
  - When it fires: an acquire attempt returns a backend error (fail
    closed). `Adjust` returns the error, as the spec's reconciliation
    contract requires. `Release` returns it, and the slot is reclaimed one
    TTL later. A renewal counts it as a transient failure and retries on
    the next tick.

### 5. Waiting: polling in the MVP, a FIFO queue planned

When a request doesn't fit, `Acquire` sleeps for the shorter of the
`RetryAfter` hint and a backoff with jitter, then tries again, until `ctx`
ends or `MaxWait` elapses. `MaxWait` bounds the whole wait, including an
attempt that is itself blocked on the bucket lock: each attempt runs under a
context that ends with the wait budget, and each backend checks that
context after taking its lock and before charging capacity. `MaxWait` is
therefore a hard deadline on the whole `Acquire`: nothing is admitted after
it, even when the wait was spent on lock contention rather than polling. When it gives up it returns `LimitError` with
`RetryAfter`.

Polling is not fair:

- When a slot frees, the waiter whose poll lands first wins. A long-waiting
  waiter can lose any number of times to newer arrivals, and its wait has no
  bound short of `MaxWait`.
- On the token side, small requests keep fitting and draining the bucket, so
  a large request may never see enough capacity.

On average this is fine; the worst case is one unlucky caller.

**A fair (FIFO) waiter queue is planned, not part of the MVP.** The planned
design:

- A `bucket_waiter` table with a sequence number per arrival.
- Only the oldest live waiters may acquire: with N free slots, the N oldest.
- Waiter rows expire and renew the same way leases do, so a dead waiter
  can't block the queue.
- Optionally, `LISTEN/NOTIFY` wakes waiters on release instead of polling.

One question stays open: **when may a small request jump the queue?** Strict
FIFO lets one large request at the head block small ones that would fit now.
One candidate rule: strict FIFO until the queue exceeds a size threshold,
then let requests that fit go ahead of a head waiter that can't fit. That
gets decided when the queue is built.

The queue changes only the Postgres backend's internals. `Acquire`, `Lease`
and the `ratelimit` adapter stay the same, so adding it is not a breaking
change.

### 6. Testing

- Unit tests run the in-memory backend with an injected clock.
- The Postgres backend is tested against a real Postgres: a service
  container in CI, gated locally on `BUCKET_POSTGRES_DSN`. The tests include
  many goroutines across several `*sql.DB` handles, standing in for
  instances, asserting the concurrency cap is never exceeded; crash
  recovery, where a lease whose renewal is stopped is reclaimed after its
  TTL; lost-lease detection; the `MaxLeaseDuration` cap; an acquire that
  waits on the bucket lock longer than `LeaseTTL` still returns a live lease
  (lock contention held open by a second transaction); `OpTimeout` firing
  on a blocked `Adjust`, renewal and `Release`; `Ensure` with and without
  overwrite, including a rate change over existing debt (the -600 example
  in Decision 3); the adapter rejecting `Slots() > 1` before acquiring; a limit change seen by every handle on its next acquire; `New` and `CheckSchema` returning `SchemaError` on a missing or older
  schema and accepting a newer one; `SchemaVersion` equal to the highest
  migration file;
  delete-cascade while leases are held; and refill and
  `Adjust` arithmetic using database time.
- pgx is a test-only dependency (its stdlib driver). Consumers that don't
  import the test packages don't compile it.

## Consequences

- **Additive.** No existing API changes. Apps opt in by building a
  `bucket.Limiter` and wrapping it with `ratelimit.FromBucket`.
- **The spec changes.** The "Distributed limiter implementation" section and
  the v0.1 scope sentence are amended in the implementation PR to point
  here.
- **Each held slot costs one background goroutine** and one write every
  `LeaseTTL/3`. Each acquire attempt is one short transaction that locks a
  single row. At LLM call rates (tens per second per key) that is
  negligible. The per-key row lock serializes acquirers for that key, and
  that is the intended behavior.
- **Some upgrades need a database migration.** It is the app's job to apply
  it, before deploying. A missed migration fails fast at `New` with
  `SchemaError`, never silently. Release notes flag every one (see "Schema
  delivery and versioning").
- **Fail closed.** If Postgres is unreachable, admission fails and no
  provider call is made.
- **A lost lease can briefly exceed the limit**, by at most one call, bounded
  as above. The provider's own limit, such as a LunaRoute 429, remains the
  backstop.
- **Limits are operational data** in the Postgres backend: they can change
  with no redeploy, and `Ensure` is the only write path the package
  provides. Apps that keep limits in config choose `overwrite` deliberately.
- **Alloy:** it speaks the Postgres protocol and SQL, so it's expected to
  work, but it's untested.
- **No `MAESTRO_DIVERGENCES.md` row.** Maestro has no distributed limiter to
  diverge from, and the in-memory limiter is unchanged.

## Alternatives considered

- **Build it in Morris first, then promote it** (the spec's plan). Rejected:
  three consumers need it now, and the interface is generic.
- **A separate Go module.** Rejected for now; see Decision 1.
- **Postgres advisory locks for slots.** A session lock is released when its
  connection dies, so crash recovery comes free. Rejected because each lease
  pins a pooled connection for the whole call, it doesn't extend to the
  token bucket, and it is Postgres-specific in a way that blocks other
  backends later.
- **A counter column instead of lease rows.** Rejected: a crashed holder's
  slot is unrecoverable.
- **A long TTL with no renewal.** Rejected: a crashed slot stays held for the
  whole TTL, minutes, which at a limit of 2 is an outage.
- **Limits only in each instance's config** (the first draft). Rejected:
  instances could disagree, and the bucket row is locked on every acquire
  anyway, so reading the limits from it is free.
- **A separate table for limits beside the state table.** Rejected: both are
  one row per key. Merging them gives one row to lock and one foreign-key
  target.
- **A fixed config-vs-database rule** (always seed-only, or always upsert).
  Rejected in favor of `Ensure(…, overwrite)`. It is barely more code and
  leaves the policy to the app.
- **Per-second or per-day rate units.** Deferred: tokens per minute matches
  the current limiter and most LLM quotas, and conversion is the app's job
  (Decision 1).
- **Generic SQL dialects (SQLite, MySQL) or Redis.** Deferred: every current
  consumer is on Postgres. Each would be a new `bucket/<backend>` package
  behind the same interface.
- **Creating the tables on the fly** (`CREATE TABLE IF NOT EXISTS` from
  `New`). Rejected:
  - production app roles often have no DDL privilege;
  - concurrent `IF NOT EXISTS` across instances can still fail on a
    catalog unique violation;
  - it never alters an existing table, so the first schema change (the
    FIFO queue's waiter table) would need hidden versioned migrations
    anyway;
  - the app's migration tool never sees the tables.
- **A `postgres.Migrate(ctx, db)` runner** (advisory-locked, idempotent).
  Deferred: the migration files plus `SchemaError` cover the need, and
  every consumer already has a migration tool. Adding a runner later is
  additive.
- **A configurable table prefix or schema name.** Rejected: it forces
  templated SQL, and the files can no longer be copied verbatim into a
  migration tool.
- **Building the FIFO queue in the MVP.** Deferred, see Decision 5: about
  30–40% more backend work, and the queue-jumping rule is still open.

## References

- #100 (this work).
- `docs/specification.md`: "Rate Limiting", "Distributed limiter implementation".
- `llms/ratelimit/ratelimit.go` (the reservation protocol);
  `llms/ratelimit/memory.go` (refill and Adjust semantics, ported);
  `llms/middleware/ratelimit.go` (Release on `context.WithoutCancel`).
- ADR-0003: middleware is Complete/Embed-only; streaming is unaffected.

Note (2026-10-29): the specification was refreshed as the v1 contract in #84 step 4 (PR #133). Its "Distributed limiter implementation" section is now the "Shared limiter" part of "Rate Limiting"; the pre-v1 text is [`docs/specification.md` at 30b2b93](https://github.com/SnapdragonPartners/maestro-llms/blob/30b2b930370c50593d09eef4c5303682da25e231/docs/specification.md).

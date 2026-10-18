# 0025. Generic `bucket` limiter with a Postgres backend (lease rows, renewal, FIFO queue planned)

- **Status:** Proposed
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

A `*bucket.LimitError` becomes a `*llms.LimitError` carrying the request's
Provider and Model and the same `RetryAfter`, so retry and circuit
middleware treat it exactly as they treat today's local rejections.
The `key` function is the app's policy. buggrit would map every request to
`"lunaroute"`; Morris might use provider plus model. The middleware does not
change. `InMemoryLimiter` stays as it is; reimplementing it on
`bucket/memory` is left to the pre-v1 cleanup (#82) and is not required.

### 3. Postgres backend: token bucket and slots in one transaction

`bucket/postgres.New(db *sql.DB, cfg)` takes an app-supplied `*sql.DB` and
imports no database driver. pgx users pass `stdlib.OpenDBFromPool(pool)`.

Two tables:

- `bucket`: one row per key, holding both its limits and its state:
  `id` (bigint identity primary key), `name` (unique),
  `tokens_per_minute`, `max_concurrency`, `buffer_factor`, `token_balance`
  and `last_refill`;
- `bucket_lease`: one row per held slot, holding the lease ID, a
  `bucket_id` that references `bucket(id)` with `ON DELETE CASCADE`,
  `expires_at` and `acquired_at`.

**The database owns the limits.** Every acquire reads them from the row it
already locks, so storing them costs nothing extra.
- *Changing a limit* is an `UPDATE`, which takes effect on the next acquire
  everywhere. Lowering `max_concurrency` below the slots currently held
  preempts no one; new acquires wait until the count falls under the new
  limit.
- *Deleting a bucket* (cleanup, or a full reset) cascades to its leases.
  Holders still in flight find their lease gone on their next renewal
  (`ErrLeaseLost`, Decision 4). Acquiring on a deleted key fails closed
  until it is ensured again.
- *The surrogate `id`* lets a bucket be renamed without touching its leases
  and keeps lease rows small.

Buckets are created or updated with
`postgres.Ensure(ctx, name string, limits bucket.Limits, overwrite bool)`.
With `overwrite` false it inserts only when the bucket is missing
(`ON CONFLICT DO NOTHING`); with `overwrite` true it upserts the limits and
leaves the token balance and leases alone. The package builds no policy into
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
   and commit. Otherwise roll back and return the `RetryAfter` hint.

**All time comes from the database (`now()`).** No instance's clock enters
the calculation, so clock skew across instances does not matter.

A request larger than the key's capacity fails immediately with
`LimitError`, since it can never fit.

The schema ships as SQL text (`postgres.Schema`) for the app to apply with
its own migration tool. The package never runs DDL. The table-name prefix is
configurable and validated as an identifier.

### 4. Slot leases: renewal, lost leases and a hard cap

- **Renewal.** A lease row expires at `now() + LeaseTTL`, 30 seconds by
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
  `UPDATE … SET expires_at = now() + ttl WHERE id = $1 AND expires_at > now()`.
  If no row updates, the lease has expired and may already have been
  reclaimed. Renewal then stops. `Release` returns `bucket.ErrLeaseLost`
  (informational; the middleware ignores Release errors), and `Lease` also
  exposes `Lost() <-chan struct{}` for callers that want to abort work. The
  harm is bounded: at most one call runs over the limit, for the rest of that
  call.
- **A hard cap: `MaxLeaseDuration`,** default 15 minutes. Renewal turns a
  leaked `Release` (a holder that never releases) into a slot held forever,
  where TTL alone would have recovered it. After `MaxLeaseDuration` from
  acquire, renewal stops and the slot is reclaimed one TTL later. The cap
  must exceed the longest legitimate call; the per-attempt Timeout
  middleware normally bounds that.
- **`Release` bounds itself** with a short internal timeout. The middleware
  calls it on `context.WithoutCancel(ctx)`, which has no deadline, so a hung
  database would otherwise hang the caller.

### 5. Waiting: polling in the MVP, a FIFO queue planned

When a request doesn't fit, `Acquire` sleeps for the shorter of the
`RetryAfter` hint and a backoff with jitter, then tries again, until `ctx`
ends or `MaxWait` elapses. When it gives up it returns `LimitError` with
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
  TTL; lost-lease detection; the `MaxLeaseDuration` cap; `Ensure` with and
  without overwrite; a limit change seen by every handle on its next acquire;
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
- **Building the FIFO queue in the MVP.** Deferred, see Decision 5: about
  30–40% more backend work, and the queue-jumping rule is still open.

## References

- #100 (this work).
- `docs/specification.md`: "Rate Limiting", "Distributed limiter implementation".
- `llms/ratelimit/ratelimit.go` (the reservation protocol);
  `llms/ratelimit/memory.go` (refill and Adjust semantics, ported);
  `llms/middleware/ratelimit.go` (Release on `context.WithoutCancel`).
- ADR-0003: middleware is Complete/Embed-only; streaming is unaffected.

# 0052. `bucket`: leases held by ID from another process (detached leases, `bucket/remote`)

- **Status:** Accepted; amends [ADR-0025](0025-bucket-distributed-limiter.md)
- **Date:** 2026-11-22

## Context

A `bucket/postgres` lease lives in the process that acquired it. That
process renews the lease row in the background, and only that process can
`Adjust` or `Release` it, because the charge (`tokens`, `tokenCharged`,
`adjusted`) is held in memory (ADR-0025, Decision 4).

Morris is splitting into a web service, which holds the database
credentials, and a private worker service, which holds none and reaches
Morris's state only through an internal HTTP API the web service serves
(Morris ADR 0068). Decision 2 of that ADR requires the worker's provider
calls to draw from the same per-client buckets as the web service, and to
fail closed when the API can't be reached. Cloud Run sends each request to
any web instance, and its session affinity is best effort. So the `acquire`
can reach instance A while the later `adjust` and `release` reach instance B,
which has no handle on A's lease (#189).

Ownership stays where ADR-0025 put it: this package owns admission and lease
accounting, and the app owns the authenticated transport. What's missing is
lease operations that any instance can run, by lease ID, using only what is
stored in the database.

## Decision

### 1. Detached leases: a lease row that records its own terms

Schema v2 adds six columns to `bucket.lease`, all defaulted:

| Column | Default | Meaning for a detached lease |
|---|---|---|
| `detached` | `false` | the lease is held by ID |
| `tokens` | `0` | the estimate debited at acquire |
| `token_charged` | `false` | whether acquire actually debited it (the bucket was token-limited) |
| `adjusted` | `false` | whether an adjustment has been applied |
| `ttl` | null | the renewal TTL fixed at acquire |
| `max_expires_at` | null | the hard deadline (`acquired_at + MaxLeaseDuration` of the acquiring instance) |

A check constraint requires `ttl` and `max_expires_at` on detached rows.

**Each lease has exactly one authority for whether it has been adjusted.**
An in-process lease keeps it in memory, exactly as before. It writes none
of the new columns, and its `Adjust` still reconciles after its row is
reaped. A detached lease keeps it in the row, so any instance can reconcile
it. By-ID operations accept only `detached` rows; an in-process lease's ID
gets the same answer as an unknown one. That makes it impossible for the
local path and the by-ID path to both reconcile the same lease.

**The terms are fixed at acquire, in the row.** The in-process renewal SQL
passes the renewing instance's own `LeaseTTL` and `MaxLeaseDuration`. That
was harmless while the acquiring instance always did the renewing. With
detached leases, an instance configured with a fifteen-minute cap could
extend a lease that another instance granted for two minutes. Detached
renewal therefore reads `ttl` and `max_expires_at` from the row:
`expires_at = LEAST(clock_timestamp() + ttl, max_expires_at)`.

### 2. Operations on `*postgres.Limiter`

```go
func (l *Limiter) TryAcquireDetached(ctx context.Context, key string, tokens int) (bucket.DetachedLease, error)
func (l *Limiter) RenewLease(ctx context.Context, key, id string) (bucket.Validity, error)
func (l *Limiter) AdjustLease(ctx context.Context, key, id string, actualTokens int) error
func (l *Limiter) ReleaseLease(ctx context.Context, key, id string) error
```

`bucket.DetachedLease{ID; Validity}` and
`bucket.Validity{TTL, ExpiresIn, CapIn}` live in `bucket`, so
`*postgres.Limiter` is itself a `remote.Backend` (Decision 4) and a host's
handlers pass straight through. `bucket.Limiter` and `bucket.Lease` are
unchanged.

- **`TryAcquireDetached` makes one admission attempt.** It shares the
  admission transaction (`admit`) with `Acquire`: lock, refill, reap expired
  leases, count, debit. A request that doesn't fit returns a temporary
  `*bucket.LimitError` with `RetryAfter` (zero when it is waiting on a slot);
  one that can never fit returns a `Permanent` one. It never waits: waiting
  across an HTTP hop would hold the web request open for up to `MaxWait`,
  polling the database the whole time, and a client-side timeout would then
  leak a slot. The holder waits instead (Decision 4).
- **Validity is returned as durations,** measured on the database clock
  from a single reading taken after the row is written (`CapIn` is
  `ExpiresIn` plus the stored gap to the cap, so it is never below
  `ExpiresIn`). Durations rather than timestamps keep clock skew between
  machines out of it.
- **`RenewLease`** returns the validity after the renewal. Once the cap is
  reached a renewal succeeds without extending the lease
  (`ExpiresIn == CapIn`); after the cap it returns `bucket.ErrLeaseLost`.
- **`AdjustLease` only adjusts a live lease.** It locks the bucket row and
  then the lease row (acquire's lock order), reads `clock_timestamp()` after
  both locks are held, and marks the lease adjusted in the same transaction
  as the balance change. A repeated call returns nil while the lease is
  live. After the lease expired or was released, it returns
  `bucket.ErrLeaseLost`, even when an earlier call committed and its
  response was lost. **The balance changes at most once**, and the answer is
  the same whether or not a later acquire has reaped the row.
- **`ReleaseLease`** deletes the row and returns `ErrLeaseLost` when it had
  expired or was gone. A retried release can therefore report
  `ErrLeaseLost` after the first one succeeded.
- **Every by-ID call takes the key the caller authorized** and matches the
  lease on its ID and on the current name of the lease's bucket. A wrong
  key, an unknown or malformed ID (anything but the canonical UUID form, so
  it never reaches the database as a type error), an in-process lease's ID,
  or a different bucket's lease all get `bucket.ErrLeaseLost` and change
  nothing. A lease is bound to its bucket's stable `id`. After a rename,
  callers pass the new name; a new bucket that takes the old name never
  gains an earlier lease.
- `AdjustLease` and `ReleaseLease` run on cancellation-surviving contexts
  bounded by `OpTimeout`, like their in-process counterparts. `RenewLease`
  and `TryAcquireDetached` follow the caller's context.

### 3. A gone lease's estimate stands (a deliberate relaxation)

The in-process `Adjust` reconciles even after its lease is lost, because
the charge is in memory. A detached adjustment needs a live row, so an
adjustment that arrives after the lease expired leaves the acquire-time
estimate charged. An overestimate then stays charged, which is the safe
direction. An underestimate's shortfall goes unbilled. The middleware
discards `Commit` errors, so nothing surfaces this.

We accept it for this release. A rate limit refills, and each provider
attempt takes its own lease, so a lease lasts one attempt, not a whole tool
loop. Morris's first consumer, ingestion embeddings, uses
`MaxConcurrency: 8` with no token limit, so it can't lose anything this way.
Keeping expired rows in `bucket.lease` for late adjustment would not have
worked anyway: during a rolling deploy, binaries that require v1 reap them
on acquire. If late reconciliation is ever needed, it belongs in a separate,
bounded accounting record.

### 4. `bucket/remote`: the holder's side

A leaf package (it imports `bucket` and `bucket/internal/core` only; never a
database driver or `llms`). `remote.New(backend, Config)` returns a
`bucket.Limiter` over an app-supplied `Backend` with the four methods above.
The worker uses it with `ratelimit.FromBucket` like any other limiter.

- **Acquire keeps `Acquire`'s semantics on the holder's side.** It uses
  `core.Wait`, the same loop as the backends: jittered backoff that never
  sleeps past `RetryAfter`, and `MaxWait` as a hard deadline across every
  backend call. Only a temporary refusal is retried, meaning a
  `*bucket.LimitError` with `Permanent` false and no `Cause`. A `Permanent`
  refusal returns after one attempt. Caller cancellation ends the wait. Any
  other error, including a transport failure whose outcome is unknown, is
  returned after one attempt and fails closed. Acquire is not idempotent: a
  server can commit the acquire and lose the response, and replaying it
  would debit twice. A lost response leaks the slot for at most one TTL, with
  the estimate charged.
- **Validity is measured from when the request was sent,** so a slow
  response only shortens the lease. A grant that arrives after that
  validity, or after `MaxWait`, is released (best effort) and not returned,
  so no provider call starts on a lease that may already be gone. A grant
  with no ID or a TTL too short to renew also fails closed.
- **Renewal runs every `TTL/3`.** Each attempt is bounded by `OpTimeout`,
  the interval and the remaining validity. `Lost()` closes when a renewal
  reports `ErrLeaseLost`, when the last confirmed validity runs out without
  a successful renewal (the backend is unreachable or hanging), or at the
  hard cap. A renewal can tighten the cap but never extend it.
- **`Adjust`** is sent on a cancellation-surviving context. After a
  definitive answer (nil or `ErrLeaseLost`), later calls are no-ops. After a
  transport error, a later call sends it again, which the backend applies at
  most once. **`Release`** stops renewal and releases once.

`remote.Backend` joins the README's frozen-interface list: apps implement
it over their own API, so adding a method would be a v2 change.

### 5. Schema v2 rollout

`SchemaVersion` becomes 2 with `000002_detached_leases.{up,down}.sql`. The
release notes open with "Database migration required", addressed to
`bucket/postgres` adopters, with the rollout order migrate, then deploy.
All columns have constant defaults, so on PostgreSQL 11+ the migration does
not rewrite the table. Binaries that require v1 keep working on a v2
database: their inserts are in-process leases, their renewals touch only
`expires_at`, and their acquires count and reap detached leases like any
others. A binary that requires v2 fails at `New` with `*SchemaError`
(found v1) on an unmigrated database. The down migration removes the
columns. Detached leases then become rows that nothing renews, so they free
within one TTL.

## Consequences

- A process with no database access can share a bucket through any
  transport the app chooses, without this package knowing about HTTP or
  authentication. A host's handlers are thin pass-throughs, and the worker
  gets a normal `bucket.Limiter`.
- Detached leases cost the holder one renewal round trip every `TTL/3`
  (every 10s at the default TTL) through the host, plus one database
  statement per renewal on the host.
- A detached adjustment can miss a shortfall once its lease is gone
  (Decision 3), and a lost acquire response leaks a slot for at most one TTL
  (Decision 4). Both are documented, and both are bounded.
- In-process leases are untouched: same SQL, same behavior, same in-memory
  reconciliation.

## Alternatives considered

- **Blocking `AcquireDetached` with `Acquire`'s waiting**, as #189 first
  sketched. Rejected: it holds the web request open for `MaxWait`, runs into
  request timeouts, and makes a client-side timeout leak a slot. Single
  attempts keep every call short and stateless.
- **The row as the authority for every lease's adjustment.** Rejected: an
  in-process lease could then no longer reconcile after its row is reaped,
  which changes existing behavior. The `detached` flag gives each lease one
  authority without changing in-process leases.
- **A combined `SettleLease` (adjust and release in one call).** Deferred.
  `FromBucket` calls `Adjust` in `Commit` and `Release` separately, and the
  `Lease` contract keeps the slot held after `Adjust`. On the integration
  path it saves no round trip, and it couldn't recover an estimate after
  reaping either.
- **Keeping expired, unadjusted rows for late reconciliation.** Rejected:
  binaries that require v1 reap them during a rolling deploy, and it
  complicates the slot count.
- **ID-only operations (no key).** Rejected: the key lets the host
  authorize per bucket and have the database enforce that the lease belongs
  to that bucket.
- **Idempotent acquire (a client-supplied request key).** Deferred: the
  failure it prevents is bounded by one TTL and charges in the safe
  direction.

## References

- #189; Morris ADR 0068 (worker service), morris#570, morris#707.
- [ADR-0025](0025-bucket-distributed-limiter.md) (the limiter, Decision 4
  lease renewal), [ADR-0033](0033-limit-error-classification.md)
  (`LimitError.Permanent` and `Cause`).
- `bucket/postgres/detached.go`, `bucket/remote/remote.go`,
  `bucket/postgres/migrations/000002_detached_leases.up.sql`.

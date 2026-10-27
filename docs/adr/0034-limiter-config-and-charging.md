# 0034. Limiter configuration is validated; usage is charged from the normalized fields

- **Status:** Accepted
- **Date:** 2026-10-26

## Context

The #84 step 2 whole-API audit found four problems with how the toolkit's
limiters are configured and how they charge actual usage. All four are
documented or observable behavior, so they have to be settled before v1
(#84 step 3, v0.15 PR 2).

1. **Invalid limits pass silently.** `NewInMemoryLimiter` returns only a
   limiter. A negative `TokensPerMinute` or `MaxConcurrency` disables that
   dimension, because both checks are `> 0`. A negative `BufferFactor`
   falls back to the default. A NaN or infinite `BufferFactor`, or a
   `TokensPerMinute * BufferFactor` product of 2^63 or more, reaches an
   `int(float64)` conversion, which Go leaves implementation-defined.
   `bucket.Limits.Validate` already rejected the negatives, but not NaN,
   infinities or overflow.
2. **A nil estimator panics.** `RecommendedConfig.Estimator` documents
   "nil => DefaultEstimator", but `RateLimitChat(lim, nil)` and
   `RateLimitEmbeddings(lim, nil)` store the nil and panic on the first
   call.
3. **The charge ignores the ADR-0016 output split.** `effectiveTokens`
   charged `TotalTokens` when it was set, and otherwise `InputTokens +
   OutputTokens + EmbeddingTokens`.
   - Without a total, reasoning tokens were never charged, because
     `OutputTokens` is visible-only (ADR-0016). They are real metered
     output.
   - With a total, the charge depended on what each provider puts in it.
     `TotalTokens` is passed through from Google, OpenAI and
     OpenAI-compatible servers, and derived by the Anthropic and Ollama
     adapters, so it is not one definition.
   - `usageReported`, which decides whether to commit at all, ignored
     `ReasoningTokens` and `BillableOutputTokens`. A response reporting only
     those kept the full estimate.
4. **Input and embedding counts had no defined relation.** The charge
   summed `InputTokens` and `EmbeddingTokens`. Nothing said whether an
   adapter may set both for the same tokens. If one did, the input would be
   charged twice.

## Decision

1. **`NewInMemoryLimiter(cfg) (*InMemoryLimiter, error)`.** It validates its
   limits by building a `bucket.Limits` and calling `Validate`, and returns
   `(nil, err)` when they fail. It also takes its capacity, refill rate and
   enabled dimensions from `bucket.Limits`. That makes `bucket.Limits` the
   single definition of how a bucket is sized and what makes limits valid,
   for `InMemoryLimiter`, `bucket/memory` and `bucket/postgres` alike.
   - A negative `BufferFactor` is now an error, not the default. Zero still
     selects `bucket.DefaultBufferFactor` (0.9).
   - `MaxWait` and `PollInterval` are not limits and are not validated here.
     `bucket.Limits` doesn't have them either; they are caller policy
     (ADR-0025).
2. **`bucket.Limits.Validate` also rejects** a NaN or infinite
   `BufferFactor` (even when token limiting is off) and a
   `TokensPerMinute * BufferFactor` of `float64(math.MaxInt)` or more.
   `Capacity` is documented as defined only for limits that pass `Validate`.
   Capacity bounds refill too: `InMemoryLimiter`'s refill clamps the
   accrued carry at the room left below capacity before converting it to
   an int, so a long gap at a rate near the bound stays defined. The
   bucket backends refill in float64 and clamp to `Capacity` already.
3. **A nil estimator selects `DefaultEstimator`** in `RateLimitChat` and
   `RateLimitEmbeddings`, matching `RecommendedConfig`.
4. **`effectiveTokens` charges from the normalized fields**, on both limiter
   paths (`InMemoryLimiter.Commit` and `FromBucket`'s `Adjust`):
   - **input** = `max(InputTokens, EmbeddingTokens)`. Both count the
     request's input tokens: `InputTokens` for chat, `EmbeddingTokens` for
     an embedding call. They overlap and are never summed. Cache reads and
     writes stay subsets of `InputTokens` and are not added (ADR-0023).
   - **output** = `BillableOutputTokens`, or `OutputTokens +
     ReasoningTokens` when it is unset. Reasoning is charged either way.
   - **charge** = `max(input + output, TotalTokens)`. `TotalTokens` is a
     floor, not the charge. Usage that reports only a total charges the
     total instead of refunding the whole estimate. A total that counts
     tokens the normalized fields don't is not undercounted: Gemini's total
     includes the tool-use prompt, which `InputTokens` doesn't.
   - Negative fields count as zero.
5. **`usageReported` counts every `Usage` token field**, including
   `ReasoningTokens` and `BillableOutputTokens`. Usage reporting only those
   is committed.
6. **`Usage` docs:** `TotalTokens` is documented as a provider-specific
   aggregate. The Anthropic and Ollama derivations are described, and
   equality across providers is not promised. `EmbeddingTokens` is
   documented as the input count of an embedding call, which overlaps
   `InputTokens`.

## Alternatives considered

- **Charge `TotalTokens` only when every normalized field is zero.** This
  fixes the total-only case, but it charges a Gemini call with tool-use
  prompt tokens less than Gemini meters. Overcounting is the safe
  direction for a limiter, so the larger of the two wins.
- **Sum `InputTokens` and `EmbeddingTokens`, and require adapters to set
  only one.** That is a rule no type enforces. Taking the larger is correct
  under either reading.
- **Keep `NewInMemoryLimiter` infallible and clamp bad values.** Clamping
  hides a config error behind a limiter that doesn't do what was asked.
  Since `NewInMemoryLimiter` is the place config errors surface, an error
  return is the honest signature, and v0.15 is the last point it can change
  before v1.
- **Panic on invalid config.** Limits often come from app config at
  runtime. An error lets the app report it; the toolkit doesn't panic on
  input (only on programmer errors such as a nil key func in
  `FromBucket`).

## Consequences

- **Breaking change**, for the release notes: `NewInMemoryLimiter` returns
  `(*InMemoryLimiter, error)`. Known call sites: Maestro `factory.go:85`,
  Morris `maestrochat.go:47`, buggrit `llm.go:95`.
- **Behavior changes**, for the release notes:
  - Configs that used to be accepted are rejected: negative limits or
    `BufferFactor`, a non-finite `BufferFactor`, an overflowing capacity.
    That applies to `InMemoryLimiter`, `bucket/memory` and
    `bucket/postgres.Ensure`.
  - Reasoning tokens are now charged when `BillableOutputTokens` is unset,
    and a response reporting only reasoning or billable output is
    reconciled.
  - A provider whose `TotalTokens` is smaller than input plus billable
    output is charged the larger amount. A provider whose total is larger
    (Gemini with tool use) is charged the total, as before.
  - Every adapter in this repo reports a `TotalTokens` at least as large
    as input plus billable output when the server's counts are
    consistent. So charges are unchanged where a total is reported; only
    usage without a total, or with an inconsistent one, changes.
- `RateLimitChat(lim, nil)` no longer panics.
- No `bucket/postgres` schema change. `Validate` runs in `Ensure` before
  any write, so existing rows are unaffected.

## Amends

- [ADR-0016](0016-usage-reasoning-tokens.md): `TotalTokens` is no longer
  "the provider's authoritative grand total". It is a provider-specific
  aggregate (decision 6), and the limiters use it only as a floor.
- [ADR-0023](0023-usage-input-includes-cache.md): decision 4 (the in-memory
  limiter charges `TotalTokens`, else input plus output) is replaced by
  decision 4 here, for both limiter paths. Cache fields are still not
  added.
- [ADR-0025](0025-bucket-distributed-limiter.md): `Limits.Validate` also
  rejects a non-finite `BufferFactor` and an overflowing capacity, and
  `InMemoryLimiter` now sizes its bucket through `bucket.Limits`.

## References

- `bucket/bucket.go`: `Limits.Validate`, `Limits.Capacity`
- `llms/ratelimit/memory.go`: `NewInMemoryLimiter`, `effectiveTokens`
- `llms/ratelimit/bucket.go`: `FromBucket` (`Commit` calls `Adjust` with
  `effectiveTokens`)
- `llms/middleware/ratelimit.go`: `RateLimitChat`, `RateLimitEmbeddings`,
  `usageReported`
- `llms/usage.go`: the `TotalTokens` and `EmbeddingTokens` docs
- Tests: `bucket/bucket_test.go` (`TestLimitsValidate`),
  `llms/ratelimit/charging_test.go` (the charging table on both paths),
  `llms/middleware/middleware_test.go`
  (`TestRateLimitNilEstimatorUsesDefault`,
  `TestRateLimitCommitsOutputOnlyUsage`)

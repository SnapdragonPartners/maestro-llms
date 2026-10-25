# 0031. Per-attempt correlation: `ContextObserver` and the retry attempt number in ctx

- **Status:** Accepted
- **Date:** 2026-10-24

## Context

A consumer can already capture whole logical calls with its own
`ChatClient` decorator around `RecommendedChat`: the request, the
response, the call's context, and end-to-end latency. `toolloop`'s
`OnIteration`/`OnToolCall` cover turns and tools, and
`Usage.ProviderRequestID` covers request IDs.

What it can't see is the individual provider attempt inside `RetryChat`.
The only per-attempt hook is the metrics `Observer`, and its `Event` has
no context. With concurrent callers (buggrit's finder and verifier run at
once), an `Event` can't be tied to its logical call, so retries,
failures and backoff can't be separated from model time. buggrit's run
telemetry (buggrit#26) needs that separation for its latency work.

#114 proposed a `RecommendedConfig.Attempt` middleware slot, and an
attempt index derived by counting the attempts the slot sees. The
engineering review on #114 found two problems:

- **Counting undercounts.** `*llms.LimitError` is retryable, so a
  limiter rejection consumes a retry attempt that never reaches anything
  inside the rate-limit layer. With `MaxAttempts=3` an innermost hook can
  see attempts 1 and 3 and call them 1 and 2.
- **A middleware slot is more than observation needs.** The request is
  the same on every attempt, and only the final attempt's response is
  trusted, so a per-attempt hook adds the context, attempt number,
  latency, error and usage: an `Event` plus its context. A slot inside
  metrics could also change requests, responses or errors, and metrics
  would report the changed values. The consumer (buggrit) only observes.

## Decision

1. **`RetryChat` and `RetryEmbeddings` tag each attempt's context** with
   the retry loop's own 1-based attempt counter, read by
   `middleware.AttemptFromContext(ctx) (int, bool)`. It reports false
   when no retry layer sits outside the reader. With nested retry layers
   it reports the innermost one. The setter is unexported: the number is
   the retry loop's, not something callers assign. Because it is the
   loop's counter, it stays exact across attempts rejected before an
   inner layer, which shows up as a gap in the numbers an inner observer
   sees.

2. **An optional `ContextObserver` capability on `Observer`**, discovered
   by type assertion (the capability-growth rule; `Observer` is
   unchanged):

   ```go
   type ContextObserver interface {
       Observer
       ObserveContext(ctx context.Context, ev Event)
   }
   ```

   When the `Observer` given to `MetricsChat`/`MetricsEmbeddings`
   implements it, each `Event` goes to `ObserveContext` instead of
   `Observe`, never both, with the context of the call `MetricsChat`
   wrapped. The choice is made once, at construction. In
   `RecommendedChat`, where metrics is innermost, that is the attempt's
   context: the caller's values, the attempt number, and the per-attempt
   timeout's deadline.

3. **The context is for reading values.** It may already be done (a
   per-attempt timeout that fired, or the caller's cancellation), so an
   implementation that does I/O with it detaches it first. The `Observer`
   contract applies unchanged: safe for concurrent use, non-blocking.

4. **No content and no IDs from the library.** `Event` is unchanged and
   carries no request or response. The consumer owns the logical-call key
   it puts in ctx; the toolkit generates no correlation IDs.
   Metadata-only consumers (Morris) get correlation without any content
   passing through the library.

5. **The chain order is unchanged.** `RecommendedConfig` gains no field;
   `RecommendedConfig.Observer` accepts a `ContextObserver` as it is. As
   before, the innermost observer sees only attempts that pass
   validation, circuit and rate limit.

With both pieces, a consumer that wraps the recommended chain can
compute, per logical call: each attempt's provider time and outcome
(`Event.Latency`, `Event.Err`, `Event.Usage`), which attempt it was, and
the rest of the logical latency (backoff, limiter waits, overhead) by
subtraction. A decorated `ratelimit.Limiter` can split out the limiter
wait. `ExampleContextObserver` shows the records.

## Rejected alternatives

- **`RecommendedConfig.Attempt` middleware slot (#114 as filed).** It
  adds a position to the load-bearing chain and the power to change
  results, for no data an observer can't get. Revisit it only if a
  consumer needs to act per attempt, not just observe.
- **An attempt index counted by the hook.** Wrong under limiter
  rejections (above).
- **`Event.Ctx` or `Event.Attempt` fields.** Storing a context in a
  struct goes against Go convention, and a context-free `Attempt` field
  doesn't solve correlation. `AttemptFromContext` serves both
  `ContextObserver`s and any other inner layer.
- **An exported `WithAttempt` setter.** Letting callers forge the number
  would make it unreliable for the observers that read it.

## Consequences

- Additive. An existing `Observer` behaves exactly as before, and a nil
  `Observer` still adds no wrapper.
- `RetryChat`/`RetryEmbeddings` now pass each attempt a derived context
  (`context.WithValue`). Cancellation and deadlines are unchanged.
- **Known limit:** an attempt's `Latency` is one number covering network,
  provider-side queueing and generation. This separates retries and
  failures from the core flow, but not congestion from model time
  within a successful attempt. That would need provider response
  headers or time-to-first-token (streaming, deferred per ADR-0003);
  both stay with #112.
- A failed attempt has no provider request ID: `Usage` (which carries
  `ProviderRequestID`) is reported on success only, and
  `*llms.ProviderError` has no request ID field. It stays unknown; adding
  one is a separate change.
- Divergence M5 records the addition.

## References

- #114 (request and the engineering review comment), #112 (the wider
  capture scope), buggrit#26 (first consumer).
- ADR-0003 (middleware is `Complete`/`Embed` only), ADR-0004 (retry
  classifies via `llms.Retryable`, which makes `LimitError` retryable).
- Spec: "Logging and metrics should use narrow callback interfaces, not
  a specific logging package."

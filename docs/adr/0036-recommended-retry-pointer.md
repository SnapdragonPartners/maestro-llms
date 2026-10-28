# 0036. `RecommendedConfig.Retry` is a pointer: nil selects the jittered default

- **Status:** Accepted
- **Date:** 2026-10-28

## Context

The #84 step 2 whole-API audit (item B4) found that the zero
`RecommendedConfig` runs without jitter (#84 step 3, v0.15 PR 4).

`RetryConfig` fills each zero or invalid numeric field with its default,
except `Jitter`: 0 means "jitter off", because a caller must be able to
ask for deterministic backoff. That rule is right for `RetryChat`, where
the caller always passes a config. But `RecommendedConfig.Retry` was a
`RetryConfig` value, so leaving it out and asking for zero jitter were
the same thing. `RecommendedConfig{}` is documented as usable ("defaults
when their configs are zero"); only the README example's explicit
`Retry: DefaultRetryConfig()` line avoided the trap. An app that relied on
the zero value got the default schedule with jitter off. Several instances
that start together then retry in step, the thundering herd that
`DefaultRetryConfig`'s ±10% jitter is there to prevent.

At v1 the field type is frozen, so the two meanings have to be told apart
now.

## Decision

1. **`RecommendedConfig.Retry` is `*RetryConfig`.** `nil` selects
   `DefaultRetryConfig()` (5 attempts, 1s backoff doubling to 30s, ±10%
   jitter) for both `RecommendedChat` and `RecommendedEmbeddings`.
2. **A non-nil config is used as given.** It goes through the usual
   `normalized()` rules, so zero or invalid numeric fields still take their
   defaults, and `Jitter: 0` still means off. An explicit
   `&middleware.RetryConfig{}` therefore keeps today's behavior: the
   default schedule with jitter off.
3. **The config is copied when the chain is built.** Changing the
   pointed-to value afterwards does not change a built chain, as with
   every other `RecommendedConfig` field.
4. **`RetryConfig` and `RetryChat` are unchanged.** `RetryConfig{}` still
   means the default schedule with jitter off. Only the convenience
   config, where omission is the common case, gets the pointer.
5. **`Circuit` stays a value.** `CircuitConfig` has no field whose zero is
   a meaningful setting, so its zero value already selects the defaults.

## Alternatives considered

- **Treat `Jitter: 0` as "unset" and add a `NoJitter` flag or a negative
  sentinel.** That changes `RetryConfig` for every `RetryChat` caller, and
  a sentinel value is easier to misread than a nil pointer.
- **Keep the value field and document the trap.** The audit flagged it
  because the documented zero value does the wrong thing for most
  deployments; at v1 it would be permanent.
- **A `RetryConfig.Set bool` or an `Option` type for the field.** More API
  for the same distinction; a pointer is the usual Go spelling for
  "omitted means default".

## Consequences

- Breaking field type. A site that sets `Retry: middleware.DefaultRetryConfig()`
  can drop the line (nil gives the same policy) or write
  `Retry: new(middleware.DefaultRetryConfig())` (Go 1.26 `new(expr)`, which
  the module's `go 1.26` minimum allows). A site with a custom config takes
  its address: `Retry: &cfg` or `new(middleware.RetryConfig{...})`.
- Behavior change for apps that left `Retry` unset: they now get ±10%
  jitter. The schedule is otherwise the same.
- Known sites: Morris `internal/llm/maestrochat/maestrochat.go:48` and
  `internal/ai/embed.go:69`; buggrit `internal/llm/llm.go:131` and
  `internal/telemetry/telemetry_test.go:159`. Maestro composes `RetryChat`
  itself and maestro-cms does not use `RecommendedConfig`.

## References

- `llms/middleware/recommended.go`: `RecommendedConfig.Retry`,
  `retryOrDefault`
- `llms/middleware/retry.go`: `RetryConfig`, `normalized`
- Tests: `llms/middleware/recommended_test.go`
  (`TestRecommendedRetryNilSelectsDefault`,
  `TestRecommendedRetryExplicitZeroJitterKept`,
  `TestRecommendedRetryCopiedAtBuild`)

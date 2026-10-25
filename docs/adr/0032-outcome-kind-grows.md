# 0032. `toolloop.OutcomeKind` may grow after v1

- **Status:** Accepted
- **Date:** 2026-10-25

## Context

The v1 stability contract (README "Stability contract (v1)", #83, #84) lists
the enum-like types that may gain values in minor releases. Callers switching
over them need a `default` case. Dan's v1 plan put `toolloop.OutcomeKind` in
that list.

The record said the opposite. The `OutcomeKind` doc comment says "The set is
closed; new kinds require a superseding ADR". Each ADR that added a kind
repeats it: [ADR-0017](0017-toolloop-terminal-tools.md),
[ADR-0019](0019-toolloop-deadline-and-no-iteration-limit.md),
[ADR-0020](0020-toolloop-empty-response.md),
[ADR-0026](0026-context-length-error-kind.md) and
[ADR-0030](0030-toolloop-compaction.md). ADR-0020, ADR-0026 and ADR-0030 also
say "v1 freezes the outcome set" as part of their reasoning for adding a kind
before v1. Read together, a caller may rely on an exhaustive
`switch out.Kind`, and any new kind after v1 would need `/v2`.

That freeze is costly. The loop has gained a kind in five of its releases
(terminal tools, deadline, empty response, context overflow, compaction
errors). Every one of those reported a stop that callers had been misreading
as something else. A frozen set forces the next such case either into
`OutcomeLLMError` with a sentinel error, which is the misclassification those
ADRs existed to remove, or into a major version.

## Decision

1. **`OutcomeKind` is an enum-like type that may grow.** New values may be
   added in minor releases after v1.0.0, like `llms.ErrorKind` and the other
   types the stability contract lists. A `switch` over `Outcome.Kind` needs a
   `default` case. The contract's grow list includes it.
2. **Adding a kind still needs an ADR.** "Closed" now means that a kind is
   added only by a decision of record, never as an implementation detail. It
   no longer means that callers may switch exhaustively. The ADR states what
   the new kind reports, when it is chosen over the existing kinds, and what
   `Response`, `Err` and the transcript hold. The release notes say how a
   caller's `default` branch should treat it.
3. **A new kind must not take over an existing kind's cases silently.** A
   caller with only a `default` branch must still see every stop it saw
   before, under the same kind. A new kind may split out a case that used to
   be reported under another kind only when the ADR adding it says so, and the
   release notes call it out as a behavior change.
4. This amends the "the set stays closed" and "v1 freezes the outcome set"
   sentences in ADR-0017, ADR-0019, ADR-0020, ADR-0026 and ADR-0030. Their
   decisions about each kind are unchanged.

## Consequences

- Callers must write a `default` case. Code that switches exhaustively today
  still compiles. It only misses kinds added later, which Decision 3 keeps
  rare and announced.
- Future stops (for example a tool-budget or policy stop) can get their own
  kind in a minor release, instead of being forced into `OutcomeLLMError` or
  held for `/v2`.
- The `OutcomeKind` doc comment says the type may grow and that switches need
  a `default` case.

## References

- README "Stability contract (v1)"; #83; #84
- ADR-0017, ADR-0019, ADR-0020, ADR-0026, ADR-0030 (each amended)

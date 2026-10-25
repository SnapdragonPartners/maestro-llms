# 0032. Enum-like types at v1: which may grow, and how

- **Status:** Accepted
- **Date:** 2026-10-25

## Context

The v1 stability contract (README "Stability contract (v1)", #83, #84) has to
say, for each exported enum-like type, whether new values may arrive in a
minor release. Go can't close a string type, so this is a promise about
intent: may a caller switch exhaustively, or must it handle values it doesn't
know?

The record disagreed with Dan's v1 plan, which made these types growable:

- **`toolloop.OutcomeKind`.** The doc comment said "The set is closed; new
  kinds require a superseding ADR". Each ADR that added a kind repeats it:
  [ADR-0017](0017-toolloop-terminal-tools.md),
  [ADR-0019](0019-toolloop-deadline-and-no-iteration-limit.md),
  [ADR-0020](0020-toolloop-empty-response.md),
  [ADR-0026](0026-context-length-error-kind.md) and
  [ADR-0030](0030-toolloop-compaction.md). ADR-0020, ADR-0026 and ADR-0030
  also say "v1 freezes the outcome set" as part of their reasoning for adding
  a kind before v1. Read together, a caller may switch exhaustively, and any
  new kind after v1 would need `/v2`.
- **`llms.ReasoningEffort`.** [ADR-0022](0022-reasoning-effort.md) Decision 1
  calls it "a closed set of constants".
- **`toolloop.CompactReason`** has no recorded growth decision at all.
  ADR-0030 defines its two values as the hook's two call points.

A frozen `OutcomeKind` is costly. The loop has gained a kind in five of its
releases (terminal tools, deadline, empty response, context overflow,
compaction errors). Every one of those reported a stop that callers had been
misreading as something else. A frozen set forces the next such case either
into `OutcomeLLMError` with a sentinel error, which is the misclassification
those ADRs existed to remove, or into a major version.

## Decision

1. **These types may gain values in minor releases after v1.0.0:**
   - `llms`: `ErrorKind`, `ContentPartType`, `Role`, `ToolChoiceType`,
     `ReasoningEffort`, `EmbeddingTask`, `Purpose`, `ProviderID`;
   - `ratelimit.Operation`;
   - `toolloop.OutcomeKind`.

   A `switch` over any of them needs a `default` case. Each new value comes
   with a documented meaning. Where it changes what a caller sees, the
   release notes say how a `default` branch should treat it. `Purpose` and
   `ProviderID` stay open sets, as documented: applications may define their
   own values.
2. **A new `OutcomeKind` still needs its own ADR.** "Closed" now means that a
   kind is added only by a decision of record, never as an implementation
   detail. It no longer means that callers may switch exhaustively. The ADR
   states what the new kind reports, when it is chosen over the existing
   kinds, and what `Response`, `Err` and the transcript hold.
3. **No silent reclassification.** For any type in Decision 1, a caller that
   handles only the values it knows, plus a `default` branch, must still see
   every case it saw before under the same value. A new value may split out a
   case that used to be reported under another value only when the decision
   adding it says so, and the release notes call it out as a behavior change.
4. **`ReasoningEffort`: only the constants have meaning.** That part of
   ADR-0022 Decision 1 stands: a value outside the constants is unsupported
   and omitted. New constants may be added; each adapter maps a new level
   1:1 or omits it, per ADR-0022 Decision 2.
5. **`toolloop.CompactReason` is fixed for v1.** Its two values name the
   `Compact` hook's two call points (before a turn, and on overflow), and it
   is an input to application code: a hook, like the shipped `compact`
   policies, may test `Reason == CompactOverflow` and treat everything else as
   before-turn. A third value would silently run with before-turn semantics in
   every such hook. So a new call point is a v2 change, or arrives through a
   separate, opt-in hook.
6. This amends the "the set stays closed" and "v1 freezes the outcome set"
   sentences in ADR-0017, ADR-0019, ADR-0020, ADR-0026 and ADR-0030, the
   "closed set of constants" sentence in ADR-0022, and adds Decision 5 to
   ADR-0030's design. Their other decisions are unchanged.

## Consequences

- Callers must write a `default` case for every type in Decision 1. Code that
  switches exhaustively today still compiles; it only misses values added
  later, which Decision 3 keeps announced.
- Future stops (for example a tool-budget or policy stop) can get their own
  `OutcomeKind` in a minor release, instead of being forced into
  `OutcomeLLMError` or held for `/v2`.
- Compaction hooks can rely on exactly two reasons for the life of v1.
- The doc comments of `OutcomeKind` and `CompactReason` state their rule.
  This ADR is the single authority for the README grow list.

## References

- README "Stability contract (v1)"; #83; #84
- ADR-0017, ADR-0019, ADR-0020, ADR-0022, ADR-0026, ADR-0030 (each amended)

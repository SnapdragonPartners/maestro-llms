# 0030. Tool loop compaction: one `Compact` hook, `OutcomeCompactionError`, and a `compact` leaf

- **Status:** Accepted; amended by [ADR-0032](0032-outcome-kind-grows.md) (`OutcomeKind` may grow after v1; switches need a `default` case)
- **Date:** 2026-10-20

## Context

A long or unbounded tool loop (`NoIterationLimit`, ADR-0019) grows its
transcript until it no longer fits the model's context window. ADR-0026
made that overflow visible as `OutcomeContextExceeded`, with a well-formed
`Messages` the caller can shorten and run again. Every caller that wants to
keep going writes the same restart loop around `Run`, and the restart
discards the loop's position: iteration counts, usage totals, and the
`MaxIterations` budget start over.

ADR-0011 lists "no prompt management or context compaction" among its
binding non-goals. This ADR amends that one entry. What to keep is still
app policy, so the toolkit adds a hook (the primitive) and two optional
compactors for the common cases (#80). The design decisions are recorded
on #80.

## Decision

Amend ADR-0011's non-goals and outcome set. Everything else in ADR-0011,
ADR-0017, ADR-0019, ADR-0020 and ADR-0026 stands.

1. **One optional hook, `Config.Compact`** (type `toolloop.Compactor`:
   `func(ctx, CompactInput) (CompactResult, error)`). With no hook, `Run`
   behaves exactly as before. The hook is called at two points:
   - **Before every provider call** (`CompactBeforeTurn`), with the
     transcript about to be sent and the previous response's `Usage` as
     `LastUsage` (zero before the first call). The toolkit does not know
     the model's window (ADR-0026), so the threshold is the hook's
     decision; `LastUsage.InputTokens + LastUsage.OutputTokens` is the
     provider's own count of the transcript up to the last response.
   - **On overflow** (`CompactOverflow`): when `Complete` fails with
     `ErrorKindContextLength` and the loop's context is live, the hook gets
     the transcript that was sent and the error. If it returns a compacted
     transcript, `Run` retries the call once with it. A second overflow
     with no successful call in between returns `OutcomeContextExceeded`,
     with `Messages` the compacted transcript that was sent. The retry is
     not preceded by a `CompactBeforeTurn` call.

   A result with nil `Messages` means "no change": before a turn the loop
   sends the transcript as is; on overflow it returns
   `OutcomeContextExceeded` as it would without a hook.

2. **The compacted transcript is validated** before it is used, with the
   Validation middleware's structural rules (non-empty messages, legal
   parts per role, tool-call/result pairing, no trailing unanswered calls;
   now exported as `middleware.ValidateMessages`), plus one loop rule: it
   must not end with an assistant message, since the next call continues
   from it. `Run` copies it, so the hook may return a sub-slice of its
   input. `Run` does not strip reasoning from a hook's result; the hook
   contract says to (ADR-0021: an edited history can invalidate the
   reasoning after the edit, and removing all of it is always valid), and
   both shipped compactors do.

3. **`OutcomeCompactionError`** (`"compaction_error"`) is returned when the
   hook fails or its transcript is invalid. `Err` is a `*CompactionError`
   wrapping the cause (`errors.As` reaches a `*middleware.ValidationError`
   for an invalid transcript). It goes through `classifyStop`, so a done
   loop context still gives `OutcomeCanceled` / `OutcomeDeadline`.
   `Messages` is the transcript before the failed compaction.

4. **Usage is reported separately.** `CompactResult.Usage` (a summarizer's
   provider calls) is summed into a new `Outcome.CompactionUsage`, using
   the same rules as `TotalUsage` (`ProviderRequestID` kept only for a
   single contributor). `TotalUsage` stays the loop model's calls. The
   summarizer's client is app-supplied and may be a different, cheaper
   model, so one total would mix prices irreversibly; a caller who wants
   the cost of the run adds the two. Usage is counted even when the hook
   also returns an error, since the tokens were spent.
   `Outcome.Compactions` counts the compactions applied (non-nil results
   that passed validation).

5. **Convenience compactors live in the leaf `llms/toolloop/compact`**,
   since `Summarize` makes provider calls and neither belongs next to
   `Run`'s executor-facing types. Both share one cut rule:
   - The **head** (every message through the first user message, the task
     statement) is always kept.
   - A **turn** starts at an assistant message and includes its tool-result
     message, plus any user messages immediately before it (its prompt).
     Cuts fall only between turns, so a tool call is never separated from
     its result.
   - The result is passed through `llms.WithoutReasoning`.
   - `AtTokens` sets the before-turn threshold against `LastUsage`
     (`InputTokens + OutputTokens`); zero means "only on overflow". On
     overflow they always act.
   - With nothing to drop they return nil (no change).

   `KeepLast(KeepLastConfig{Turns, AtTokens})` keeps the head and the last
   `Turns` turns. `Summarize(SummarizeConfig{Client, Prompt, KeepTurns,
   AtTokens, MaxTokens, TruncateRunes})` keeps the head and the last
   `KeepTurns` turns, and replaces the dropped turns with one user message
   that carries a summary produced by `Client`. The dropped turns are
   rendered as plain text (roles, tool names and arguments, results), not
   sent as structured messages, so the summarizer needs no tool
   definitions and providers that bind tool-call IDs or signatures to
   their own turns are not involved. Each rendered part is truncated to
   `TruncateRunes` runes (default 4000; negative means no limit), because
   the turns being dropped are typically the oversized ones and would
   otherwise overflow the summarizer too. Summaries roll: on the next
   compaction the previous summary is among the dropped messages, so it is
   sent in full as the existing summary to update, apart from the new
   turns, and the default prompt asks for an updated summary (found live:
   gpt-4o-mini otherwise dropped a value from the first summary). Re-summarizing,
   rather than appending, keeps the summary bounded on an endless run. Constructors return an error
   for a nil client or negative counts.

## Rejected alternatives

- **Overflow-only trigger.** Simplest, but every compaction costs a
  rejected request first, and only some providers signal overflow
  deterministically (ADR-0026; Ollama truncates silently).
- **A `Config.CompactAtTokens` field.** Puts a budget number in the loop
  and still cannot know the window. The threshold lives with the compactor
  that acts on it.
- **Sum compaction usage into `TotalUsage`.** One number is convenient for
  cost, but the summarizer may be a different model, and the split cannot
  be recovered afterwards.
- **Report a failing hook as `OutcomeToolError`.** Its documented meaning
  is a config or executor failure; a compaction failure needs different
  handling (fall back to a cruder compactor, or stop), and v1 freezes the
  outcome set, so the distinction belongs in `Kind`.
- **Send the dropped turns to the summarizer as structured messages.** The
  request would need the loop's tool definitions, and Gemini signatures
  and Anthropic reasoning bind to their original turns.
- **Keep the compactors in `toolloop`.** Smaller import graph, but
  `toolloop` would then make provider calls of its own.

## Consequences

- Additive for callers without a hook: no behavior change.
- `OutcomeKind` grows one value, so exhaustive `switch out.Kind` statements
  need a case. The set stays closed.
- `Outcome` grows `CompactionUsage` and `Compactions`; `Config` grows
  `Compact`. `middleware.ValidateMessages` is new exported surface.
- `toolloop` now imports `llms/middleware` (for `ValidateMessages`); both
  are leaf packages of `llms` and neither imports a provider.
- Neutrality: the hook receives only provider-neutral facts (transcript,
  usage, the overflow error). No app policy enters `Run`.
- No `MAESTRO_DIVERGENCES.md` row: `toolloop` has no pre-extraction Maestro
  equivalent.

## References

- ADR-0011: tool loop helper. Its "no context compaction" non-goal is
  amended here; the other non-goals stand.
- ADR-0019: `NoIterationLimit`, the unbounded runs this exists for.
- ADR-0021: `llms.WithoutReasoning` before editing history.
- ADR-0026: `ErrorKindContextLength` / `OutcomeContextExceeded`.
- Issue #80 and its decision comment.

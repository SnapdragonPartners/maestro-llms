# 0039. The compaction summary message is exported, and its format is fixed for v1

- **Status:** Accepted
- **Date:** 2026-11-06

## Context

`compact.Summarize` (ADR-0030) replaces dropped turns with one user message
that starts with an unexported header. On the next compaction it recognizes
that header, so a previous summary is sent in full as the existing summary
to update, apart from the new turns, which are each truncated to
`TruncateRunes`.

Morris (#144, Morris#511) runs `Summarize` on stored chat history before a
turn, outside `toolloop.Run`. The summary has to live in its database
between turns. When Morris rebuilds the transcript, it can't produce the
header, so its stored summary comes back as an ordinary user message: it is
truncated, and the summary stops rolling. Morris also can't reliably pull
the summary text out of a `CompactResult`.

## Decision

1. **Export the constructor and parser.**
   `compact.SummaryMessage(summary string) llms.Message` builds the message
   `Summarize` inserts. `compact.SummaryText(msg llms.Message) (string, bool)`
   reports whether a message is one (a user message whose only part is text
   in that format) and returns the summary. `Summarize` itself uses them,
   both to insert the summary and to recognize a previous one (for rolling,
   and for skipping it when it finds the in-flight prompt, ADR-0038). The
   format stays the library's alone: apps store the summary text and
   rebuild the message, or store the message as it is.
2. **The format is fixed for v1.** Apps persist summary messages, so a
   message that `SummaryMessage` built in one v1 release must be recognized
   by `SummaryText` and `Summarize` in every later v1 release. The header
   text may change only if the old header is still recognized. This is
   added to the README stability contract, next to the
   `EstimateTextTokens` formula, for the same reason.
3. **No new entry point for "compact now".** Outside `Run`, a caller passes
   `CompactInput{Reason: toolloop.CompactOverflow}`, which both shipped
   compactors already treat as "always act". A `CompactNow` reason would
   grow `CompactReason`, which ADR-0032 fixes for v1. A second function form
   beside the `Compactor` would duplicate the API to rename one field. The
   package doc says how.
4. **Document the head.** The package doc now states that the head (through
   the first user message) is kept verbatim, so in a long chat the first
   message is never dropped or summarized. That is unchanged behavior.

## Rejected alternatives

- **Export the header constant.** Apps would then build and parse the
  format themselves, and the library could never change it, even
  compatibly.
- **A `Summary` field on `CompactResult`.** It would help only with reading
  the summary, not with putting it back, and `CompactResult` belongs to
  `toolloop`, which has no notion of summaries.

## Consequences

- Additive: two new functions, and no behavior change.
- Morris can store and roll summaries across turns without depending on
  unexported details.
- One more stability promise: the summary message format.
- No `MAESTRO_DIVERGENCES.md` row: `toolloop` has no Maestro equivalent.

## References

- ADR-0030: compaction and `Summarize`'s rolling summary.
- ADR-0032: `CompactReason` is fixed for v1.
- ADR-0038: summaries are never the in-flight prompt.
- Issue #144, Morris#511.

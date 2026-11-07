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
3. **A summary in the head is rolled in place.** An app may rebuild the
   transcript as the stored summary plus the newer turns, without the
   original first message. The summary is then the head (ADR-0030: through
   the first user message), which is never dropped, so `Summarize` would
   leave it and add a second summary beside it, and it would stop rolling.
   Instead, when the head's first user message is a summary and `Summarize`
   compacts (there are turns to drop), it rolls that summary with the
   dropped turns and puts the new summary in its place. `KeepLast` keeps it
   as the head. The head itself is unchanged, so a compacted transcript
   still opens as the input did and is never empty. (Ending the head
   before a leading summary was tried in review and rejected: an empty
   head let `KeepLast` return an empty transcript, or one opening with an
   assistant message.) This amends ADR-0030's cut rule for `Summarize`
   only; it changes nothing for transcripts the shipped compactors produce,
   which always put the summary after a non-summary head.
4. **No new entry point for "compact now".** Outside `Run`, a caller passes
   `CompactInput{Reason: toolloop.CompactOverflow}`, which both shipped
   compactors already treat as "always act". The `CompactReason` and
   `CompactInput` docs say so: such a call has a nil `Err` and usually a
   zero `LastUsage`, so a hook must not assume a rejected request. A
   `CompactNow` reason would grow `CompactReason`, which ADR-0032 fixes for
   v1. A second function form beside the `Compactor` would duplicate the API
   to rename one field. The package doc says how.
5. **Document the head.** The package doc now states that the head (through
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

- Additive: two new functions. The only behavior change is decision 3, for
  `Summarize` on transcripts that open with a summary message, which the
  shipped compactors never produce.
- Morris can store and roll summaries across turns without depending on
  unexported details.
- One more stability promise: the summary message format.
- No `MAESTRO_DIVERGENCES.md` row: `toolloop` has no Maestro equivalent.

## References

- ADR-0030: compaction and `Summarize`'s rolling summary.
- ADR-0032: `CompactReason` is fixed for v1.
- ADR-0038: summaries are never the in-flight prompt.
- Issue #144, Morris#511.

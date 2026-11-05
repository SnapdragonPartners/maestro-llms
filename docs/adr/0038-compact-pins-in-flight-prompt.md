# 0038. Compaction pins the in-flight prompt

- **Status:** Accepted
- **Date:** 2026-11-05

## Context

ADR-0030 gave `KeepLast` and `Summarize` one cut rule: keep the **head**
(every message through the first user message), keep the last N **turns**
(an assistant message, its tool results, and the user messages just before
it as its prompt), and keep any user messages after the last turn (a prompt
not yet answered). That rule assumes an agent loop, where the first user
message is the task and the loop appends only assistant and tool messages.

A chat app runs `toolloop.Run` once per user message and sends the
conversation so far, then the new question:

```
u:q1  a:a1  u:q2  a:a2  u:q3  a:a3  u:NOW  a:call1  t:res1  a:call2  t:res2  a:call3  t:res3
```

The head is `q1`, and `NOW` is only the prompt of the `call1` turn. After a
few tool rounds an overflow makes `KeepLast{Turns: 2}` keep `q1` plus the
`call2` and `call3` rounds, so the retry answers with no question in front
of it. `Summarize` shares the rule and summarizes `NOW` away (#123, found in
Morris).

## Decision

Amend ADR-0030 item 5's cut rule; the rest of ADR-0030 stands.

1. **The in-flight prompt is always kept.** It is the last run of
   consecutive user messages after the head, when the transcript does not
   end with an assistant message (so the turns after that prompt are still
   answering it). When it falls among the messages a compactor would drop,
   it is lifted out and kept in place, between the head and the kept turns.
   The messages between the head and the in-flight prompt (the history) and
   the older turns answering it are what gets dropped or summarized.
2. **Summary messages are not prompts.** `Summarize` inserts its summary as
   a user message, so the search for the in-flight prompt skips messages
   that are a previous `Summarize` result (one text part starting with the
   summary header), the same test `render` uses to roll a summary.
3. **The summary goes right after the head**, before the in-flight prompt,
   as before. It stays among the messages the next compaction drops, so it
   keeps rolling, and it is never pinned.

The prompt is found from the transcript alone, not from
`len(Config.Request.Messages)`: compaction moves messages, so an index into
the original request goes stale after the first compaction, and the
compactors also run on stored history outside `Run` (#144). Nothing in `Run`
appends user messages, so after the request the last user run is the
caller's latest prompt.

## Rejected alternatives

- **A caller-chosen head** (`HeadAt func([]llms.Message) int`, or a mode
  that makes the in-flight prompt the head). New API for something every
  chat caller would need; the default would still drop the prompt. The
  head stays the first user message, which is the task in an agent loop.
- **Pass the request length in `CompactInput`.** Stale after the first
  compaction (above), and absent outside `Run`.

## Consequences

- Behavior change, as a bug fix in a v1 minor: compaction results differ
  only when the in-flight prompt is not the head and would have been
  dropped. A single-prompt agent loop, where the in-flight prompt is the
  head, compacts exactly as before.
- A transcript where the app inserted a user message mid-run (for example
  a nudge, then a restart of `Run`) has that message pinned, since it is
  the prompt the following turns answer. `KeepLast{Turns: 0}` keeps it.
- A compacted transcript can contain two consecutive user messages (the
  summary, then the prompt), as it already could; providers accept them.
- No `MAESTRO_DIVERGENCES.md` row: `toolloop` has no pre-extraction Maestro
  equivalent.

## References

- ADR-0030: tool loop compaction and the original cut rule.
- Issue #123 (this fix), Morris#510 (where it was found); issue #144
  (compaction on stored history outside `Run`).

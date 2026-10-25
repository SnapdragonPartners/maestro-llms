# 0026. Context-window overflow: `ErrorKindContextLength` and toolloop `OutcomeContextExceeded`

- **Status:** Accepted; amended by [ADR-0030](0030-toolloop-compaction.md) (with a `Compact` hook, overflow is offered to it and retried once), and by [ADR-0032](0032-outcome-kind-grows.md) (`OutcomeKind` may grow after v1; switches need a `default` case)
- **Date:** 2026-10-18

## Context

When a conversation outgrows the model's context window, every adapter
returned a generic `bad_request` `*llms.ProviderError`. Callers could not
tell "too long, so trim or compact" from "malformed request", and
`toolloop.Run` (ADR-0011), whose unbounded runs (ADR-0019
`NoIterationLimit`) will eventually overflow, reported it as an ordinary
`OutcomeLLMError` (#79).

Most providers only say "overflow" in the message text, so a detector that
guesses would misclassify. The live behavior was captured on 2026-10-18
with an over-long prompt to each provider (rejected requests are not
billed):

| Provider | Status | Signal |
|---|---|---|
| Anthropic (direct and Vertex) | 400 `invalid_request_error` | message `prompt is too long: 260024 tokens > 200000 maximum` |
| OpenAI Responses | 400 | `code: context_length_exceeded` |
| OpenAI embeddings | 400 | `code: null`, message `Invalid 'input': maximum context length is 8192 tokens.` |
| Gemini API | 400 `INVALID_ARGUMENT` | message `The input token count exceeds the maximum number of tokens allowed (1048576).` |
| Gemini on Vertex | 400 `INVALID_ARGUMENT` | message `... maximum number of tokens allowed 1048576.` (no parentheses) |
| LunaRoute `deepseek-4.1-flash` | 400 | `code: context_length_exceeded`, message `This model's maximum context length is ...` |
| LunaRoute `glm-5.3` | 400 | `code: UPSTREAM_ERROR`, message `The input (N tokens) is longer than the model's context length (M tokens).` |
| Ollama 0.33.2 | 200 | none: the prompt is silently truncated to `num_ctx` |

An input that fits but leaves too little room for the requested output is
not an error on Anthropic, OpenAI or Gemini; they lower or clamp the output
budget instead.

## Decision

- **A new error kind, `llms.ErrorKindContextLength` (`"context_length"`)**,
  is not retryable: the same request fails the same way, so the Retry
  middleware passes it through after one attempt, and the circuit treats
  it as neutral, as it does every non-retryable kind (ADR-0005).
- **A deterministic signal or nothing.** An adapter reports the new kind
  only for a signal in the table above: a structured code where the
  provider sends one, and otherwise a documented message fragment. Each
  fragment is a constant next to its matcher, with the captured wording in
  a comment and the real body in `testdata/`. Anything else stays
  `bad_request`. False negatives are acceptable; false positives are not.
- **One choke point.** `apierr.ClassifyOverflow` takes each adapter's
  `Overflow` detector and consults it only when the status already maps to
  `bad_request`. A detector can narrow `bad_request` to `context_length`
  but can never reclassify an auth, throttling or server error.
  `apierr.Classify` is `ClassifyOverflow` with no detector.
- **Matchers by provider:**
  - `anthropic` (and `anthropicvertex`, which uses the same adapter):
    error type `invalid_request_error` and a message starting with
    `prompt is too long`.
  - `openai`: code `context_length_exceeded`, or a message containing
    `maximum context length is`. The embeddings endpoint sends no code, so
    `Embed` gets the new kind too.
  - `vllm`: the same code and fragment, plus `longer than the model's
    context length` for gateways that send neither. The first fragment is
    also vLLM's own long-standing wording.
  - `google`: status `INVALID_ARGUMENT` and a message containing both
    `input token count` and `exceeds the maximum number of tokens
    allowed`. This covers both backends' wording and the older documented
    form with the count in parentheses.
  - `ollama`: no detector, because Ollama gives no signal. Callers must
    budget Ollama context themselves.
- **toolloop: a new outcome, `OutcomeContextExceeded`
  (`"context_exceeded"`).** It is returned when `Complete` fails with a
  `context_length` `*llms.ProviderError`. It reaches the outcome through
  `classifyStop` as that call's fallback kind, so the loop's own context
  stays authoritative (ADR-0019): an overflow reported after the caller
  canceled is still `OutcomeCanceled`. `Messages` is the transcript that
  was sent, unchanged and well-formed, so the caller can shorten it (strip
  reasoning with `llms.WithoutReasoning` per ADR-0021, then trim or
  compact) and run again. `Err` is the provider error, and `Response` is
  the last successful response.
- **Executor errors are not mapped.** A tool whose own model call
  overflows is a tool failure (`OutcomeToolError`), not this loop's
  transcript overflowing.

## Rejected alternatives

- **Match a generic "too long" / "context" substring.** This would catch
  more gateways, but it is exactly the false-positive risk the issue
  rules out. A 400 about, say, a tool description being too long must not
  trigger compaction.
- **Count tokens locally and reject before sending.** This would work for
  Ollama, but the toolkit does not know each model's window, and estimates
  are deliberately biased (ADR-0013). Budgeting is app policy, as it is in
  Maestro (`MaxContextTokens`).
- **Map 413 (request too large).** That is a byte limit, not the context
  window. It stays `unknown`.
- **Report it as `OutcomeLLMError` and let callers inspect `Err`.** This
  works, but compact-and-resume is the main reason to run an unbounded
  loop, and v1 freezes the outcome set, so the distinction belongs in
  `Kind`, like `OutcomeDeadline` and `OutcomeEmptyResponse`.

## Consequences

- **Behavior change (pre-1.0 minor):** an overflow that was `bad_request`
  is now `context_length` on `anthropic`, `openai`, `google` and `vllm`
  (`MAESTRO_DIVERGENCES.md` X11). Code that matched `bad_request` to catch
  overflow must match the new kind. Code that treats every non-retryable
  kind alike is unaffected.
- `OutcomeKind` grows one value, so exhaustive `switch out.Kind`
  statements need a case. The set stays closed.
- Wording drift is a false negative (the error falls back to
  `bad_request`), never a misclassification. The shared live trigger
  `livetest.ContextOverflow` logs each provider's current message, and the
  Ollama live test fails if Ollama starts rejecting over-long prompts.
- Neutrality is unchanged: no app policy, no new `Config` fields. A
  compaction hook is #80.

## References

- ADR-0011: tool loop helper. Amended here for the outcome set.
- ADR-0019: `classifyStop`, the choke point for context-driven stops.
- ADR-0004 / ADR-0005: `llms.Retryable` drives retry and the circuit.
- ADR-0021: `llms.WithoutReasoning` before editing history.
- Issue #79; #80 (compaction) builds on this.

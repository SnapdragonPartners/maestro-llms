# 0043. An input bound beside the estimate (`InputBounder`, `ReservationRequest.BoundUnits`)

- **Status:** Accepted
- **Date:** 2026-11-10

## Context

maestro-agents enforces hard per-run token budgets (#149). A hard budget
admits an attempt only when the attempt's worst case fits, so it needs an
upper bound on the request's usage, not an estimate. `DefaultEstimator` is
approximate by design: its bias toward over-reserving suits a rate limiter,
but it isn't a bound. With 1,000 tokens left, an input estimated at 200
permits 800 output tokens; if the input really costs 400, the budget is
overshot.

ADR-0042 shipped the composition (`ratelimit.All`) that puts a budget beside
the host's rate limiter. The two limiters want different numbers. The
budget wants the bound. The rate limiter should keep the estimate, because
the text bound runs several times the estimate and would throttle for no
reason.

The #149 thread settled what counts as a bound:

- **Text:** tokens are at most UTF-8 bytes for byte-level BPE and
  byte-fallback SentencePiece tokenizers, since each token covers at least one
  byte. Providers don't document their framing, so the bound adds generous
  margins. Loose, but real.
- **Tools:** providers add hidden tokens when tools are present. Anthropic
  lists its tool-use system prompt per model on its pricing page; OpenAI and
  Google document none.
- **Images:** formulas vary by provider and model, so image bounds stay with
  the application: keeping them correct needs a per-model registry, which
  this package avoids until a reliable source exists.
- **PDF page figures and `count_tokens` are estimates** by their own
  documentation, so they are not bounds.

`TokenEstimator` and `ReservationRequest` are frozen (README "Stability
contract (v1)"): neither can gain a method, though the struct can gain a
field.

## Decision

1. **`ratelimit.ReservationRequest.BoundUnits *UsageUnits`**, beside
   `EstimatedUnits`. Non-nil is an upper bound: input tokens bounded from the
   request, and output by `MaxTokens`. nil means no bound was established,
   and what nil means for admission is the limiter's policy (a hard budget
   refuses; a rate limiter ignores the field). `EstimatedUnits` is always an
   estimate, so the two are labeled by where they sit.
2. **`middleware.InputBounder`**, an optional capability a `TokenEstimator`
   may implement, discovered by type assertion in `RateLimitChat` and
   `RateLimitEmbeddings`:
   `BoundChat(model, req)` and `BoundEmbeddings(model, req)`, each returning
   `(units, ok, err)`. The model is the wrapped client's, so a bound can
   depend on the provider. ok false means no bound, not a failure. An error
   is a real failure: the middleware returns it unwrapped before reserving,
   and the provider is not called.
3. **`DefaultEstimator` implements `InputBounder`.** `BoundChat` gives no
   bound when:
   - `MaxTokens` is unset;
   - the provider isn't `anthropic`, `openai` or `google` (Ollama and
     OpenAI-compatible servers apply chat templates that can add any amount
     of hidden text; Vertex reports `anthropic`/`google` and is bounded);
   - the request has `ProviderParams`;
   - any part, including a tool result part, is an image or a document;
   - the request has tools and the provider isn't Anthropic.

   Otherwise the input bound is the UTF-8 byte count of every text field the
   adapters can send (system; message text; tool call IDs, names, arguments
   and signatures; tool results; reasoning text and opaque provider data;
   tool definitions and a forced tool's name). To that it adds 64 per
   request, 16 per message, 16 per part and 32 per tool definition, plus
   1,024 for an Anthropic request with tools. Anthropic lists at most 804
   tool-use system-prompt tokens per model (Opus 4.7, `any`/`tool`); current
   models list 286. `BoundEmbeddings` gives no bound: embedding task and
   title framing is undocumented, and no consumer budgets embeddings.
4. **Replayed reasoning counts at its byte length.** Whether billed input
   for a replayed thinking block is at most its bytes isn't documented, so
   the live tests check it on a thinking model (below).
5. **Live checks guard the margins.**
   - `livetest.InputBound`: minimal and multi-turn text, on Anthropic, OpenAI
     and Google.
   - `livetest.InputBoundToolLoop`: the notes tool loop, on the default
     Anthropic model and on a thinking model with replayed reasoning.
   - Both wrap the client and fail if billed `InputTokens` ever exceeds the
     bound.
   - First run (2026-11-10): billed input was 2.5× under the bound for
     Haiku 4.5 with tools (the tightest), 6–12× under for text, and 3.5–6.5×
     under for Opus 5.5 replaying thinking.

## Consequences

- Additive: one struct field, one optional interface, and two methods on
  `DefaultEstimator`. Existing limiters ignore `BoundUnits`.
- Every default reservation now carries a bound when one can be
  established, at the cost of one more pass over the request.
- **An estimator that embeds `DefaultEstimator` inherits its bound.** That's
  usually right: the bound doesn't depend on the estimate, and it declines
  anything it can't bound. An app that wants no bound defines `BoundChat`
  returning ok false.
- An app with better knowledge supplies its own bounds by implementing
  `InputBounder`: image formulas for the models it uses, OpenAI or Google
  tool overheads it has measured, or a tokenizer-exact count. It can
  delegate the text-only case to `DefaultEstimator`.
- The bound rests on documented and measured facts that providers can
  change: a tokenizer that emits sub-byte tokens, or an Anthropic model
  listed above 1,024 tool tokens, would break it. The live tests are the
  tripwire; a breach means raising the margin or narrowing what is bounded.
- Hard budgets also rely on `MaxTokens` capping billable output, reasoning
  included. That is verified per provider separately (#149 PR 3); the bound
  assumes it.

## References

- #149 and its engineering-review thread
- [ADR-0042](0042-limiter-composition-and-rollback.md) (limiter composition)
- [ADR-0013](0013-text-token-estimator.md) and [ADR-0040](0040-image-and-document-parts.md) (the estimate)
- Anthropic pricing page, "Tool use system prompt tokens" table (fetched 2026-11-10)
- `llms/middleware/bound.go`, `llms/internal/livetest/bound.go`

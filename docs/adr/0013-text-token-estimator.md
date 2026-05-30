# 0013. Text-level token estimator (`EstimateTextTokens`)

- **Status:** Accepted
- **Date:** 2026-05-30

## Context

`maestro-llms` ships a request-shaped `TokenEstimator` in
`llms/middleware` for the rate-limit reservation flow. Its char-based
core (`tokensForText`) is unexported and intentionally **biases high**
(~3 chars/token, byte-counted) because over-reservation is the safe
error at a limiter: if you reserve too much you slow yourself down; if
you reserve too little you bypass the cap.

A third consumer — `maestro-cms`, which does budget-aware text chunking
for embeddings — needs to estimate the token count of a *standalone
string*. It does not need the request-shaped API, the limiter
plumbing, or any `ratelimit.UsageUnits` machinery; it needs
`func(string) int`. Today there is no exported `text → int` entry
point, so `maestro-cms` falls back to its own local char/N estimate
and drifts from whatever `maestro-llms` uses. That drift means the
count at chunking time will not match what the rate-limit middleware
reserves at send time.

The request that drove this (filed by `maestro-cms`, non-blocking) is
recorded at `../maestro-llms-text-token-estimator.md`.

## Decision

Add a small, exported, free-function helper at the core level:

```go
// llms/estimate.go
func EstimateTextTokens(s string) int
```

Properties:

- **Neutral bias** (~4 chars/token), **rune-counted**, ceiling-divided.
- Provider-neutral and zero-dependency (`unicode/utf8` only).
- Free function, not a method on `DefaultEstimator` — chunking callers
  should not have to construct an estimator or supply a `ModelRef` for
  the common case.
- Returns 0 for the empty string.

**The bias is intentionally different from the middleware estimator.**

| Estimator | Purpose | Bias | Counting | Why |
|---|---|---|---|---|
| Middleware `TokenEstimator` (existing) | Limiter reservation | High (~3 chars/token) | Bytes (`len(s)`) | Over-reservation is safe at a limiter; under-reservation bypasses caps |
| `EstimateTextTokens` (new) | Chunking budget | Neutral (~4 chars/token) | Runes (`utf8.RuneCountInString`) | Over-estimation makes smaller-than-necessary chunks → more API calls; consumers add their own safety margin if they want |

Rune-counting matters for non-ASCII text: byte-counting non-Latin
scripts (CJK, Greek, Arabic, etc.) systematically overestimates,
which is the opposite of the limiter's intentional byte-based
overestimate — for chunking, that would compound waste.

## Non-goals

- **Not consolidating with the middleware estimator.** The two serve
  different purposes with different correct biases; a single helper
  would force the wrong tradeoff on at least one consumer. This ADR
  locks that in before a future contributor tries to deduplicate.
- **Not a tokenizer-backed variant.** A future opt-in `TextEstimator`
  interface (e.g. `NewTextEstimator(model llms.ModelRef) TextEstimator`,
  tokenizer-backed where available, char-based fallback elsewhere) is
  deferred. That is additive, does not need to land with this ADR, and
  requires a tokenizer dependency decision the v1 helper deliberately
  sidesteps. If/when a consumer needs that fidelity, it gets its own
  ADR.
- **Not truncation / budget helpers.** `maestro-cms` `chunk` owns
  splitting; the helper only produces the count.
- **Not provider-specific estimation.** Same char/token ratio for all
  models — that is the price of being a zero-dependency primitive.
  Provider-aware estimation belongs to the future tokenizer-backed
  variant.

## Consequences

- `maestro-cms` (and any consumer doing budget-aware text work) gets a
  documented, public `func(string) int` so their chunk-time counts no
  longer drift from `maestro-llms`'s view of token size.
- The existing `TokenEstimator` interface and rate-limiter behavior are
  unchanged. No new required dependency.
- The two-estimator split is **binding**: future PRs that try to fold
  one into the other require a superseding ADR. The bias-direction
  comment block in `EstimateTextTokens`'s godoc names the middleware
  estimator explicitly so the relationship is discoverable from the
  code, not just this ADR.
- This is the first ADR to formally acknowledge `maestro-cms` as a
  third consumer of the toolkit alongside Maestro and Morris. The
  neutrality discipline (no consumer-specific assumptions in this
  package) applies symmetrically across all three.

## References

- `llms/estimate.go` — `EstimateTextTokens`.
- `llms/middleware/estimator.go` — request-shaped `TokenEstimator` +
  unexported high-biased `tokensForText`.
- `docs/specification.md` §8 (Token Estimation) — updated with the
  text-level helper.
- `../maestro-llms-text-token-estimator.md` — the request that drove
  this (recorded relative to this repo).
- ADR-0001 (process), ADR-0008 (similar minimal-public-helper pattern:
  `ContentPart.CacheBreakpoint` — opt-in, advisory, honored-where-
  supported), ADR-0012 (recent neutral-data + per-consumer-policy
  split).

## Amendment 2026-05-30 — future-variant framing clarified

The "Non-goals" section above defers a future opt-in `TextEstimator`
interface, framed as **"tokenizer-backed where available, char-based
fallback elsewhere."** That phrasing was implicitly narrow — it read
as "embedded offline tokenizer or nothing" (e.g. tiktoken-go). In
practice the design space is wider, and a future ADR-0014 should plan
for the fuller shape:

| Provider | Accuracy path | Network? | New dep at toolkit level? |
|---|---|---|---|
| Anthropic | `messages/count_tokens` (anthropic-sdk-go wrapper) | yes | **none** — already imported |
| Google (Gemini, mldev + Vertex) | `genai.Models.CountTokens` | yes | **none** — already imported |
| OpenAI | tiktoken-go (offline, embedded BPE tables) | no | yes — opt-in subpackage |
| Ollama | char-based fallback (no tokenization API) | n/a | none |

Two of the four exact-tokenizer paths cost the toolkit nothing in
dependencies — the SDKs are already imported for chat. Wrapping
`count_tokens` / `CountTokens` into a `TextEstimator` is a ~10-line
method per provider. Network round-trip per call is comparable to the
embedding/chat round-trip the consumer is already paying for in the
chunk-time use case, so the "network is too expensive" objection that
originally narrowed the framing does not hold for online/incremental
workloads.

This is a **framing correction, not a decision change**. The original
v1 decision (ship char-based `EstimateTextTokens` now; defer the
opt-in variant to a future ADR-0014) stands. The amendment exists so
ADR-0014, when written, inherits the correct design space and is not
constrained to "embed a tokenizer or nothing."

Mixed-fidelity reality applies either way: even with the fuller shape,
OpenAI requires either a new opt-in dep or a network call elsewhere,
and Ollama has no high-fidelity option. The toolkit-style answer is
the same as for `ModelLister` — type-assertion over a per-provider
optional capability, missing implementations are honest rather than
stubbed.

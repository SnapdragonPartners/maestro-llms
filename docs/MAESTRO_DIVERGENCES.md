# Maestro Divergences

Living checklist of every place the extracted `maestro-llms` provider/behavior
**intentionally differs** from Maestro's original tested implementation. This
is the acceptance aid for the clean cut-over (extraction-plan step 11):
whoever switches Maestro onto `maestro-llms` must review each row and confirm
the new behavior is acceptable or adjust Maestro accordingly.

Append a row in the PR that introduces the divergence. Do not delete rows;
mark them `Resolved at cut-over` once validated.

Categories: **Behavioral** (observable output/behavior changes — these need
cut-over validation) vs **Internal** (no observable change — informational).

## Cross-cutting

| # | Kind | Maestro | maestro-llms | Why | Cut-over action |
|---|---|---|---|---|---|
| X1 | Behavioral | SDK-level retries on by default in some clients | SDK retries default **0**; retry is middleware's job | Single, consistent retry/backoff policy in `llms/middleware`, not per-SDK | Ensure Maestro wraps clients in the retry middleware (it relied on SDK retries implicitly) |
| X2 | Internal | Clients hold mutable per-call state | All clients **stateless & concurrency-safe** (spec requirement) | Safe for middleware fan-out / shared use | None (strict improvement) |
| X3 | Behavioral | Errors stringly-classified per client | Unified `*llms.ProviderError` via shared `apierr` (typed SDK error → kind/status/Retry-After) | One classification, `errors.As`-able | Maestro error-type switches must move to `llms.ErrorKind` / `errors.As(*llms.ProviderError)` |
| X4 | Behavioral | Usage often dropped | `Usage` populated incl. cache/embedding tokens where the SDK exposes it | Cost/limit accounting | Maestro can now rely on usage; previously it could not |

## Anthropic chat (PR #5)

| # | Kind | Maestro | maestro-llms | Why | Cut-over action |
|---|---|---|---|---|---|
| A1 | Behavioral | HTTP status parsed out of error **string** | Typed `*anthropic.Error` (real status + `Retry-After`) | Robust classification | Validate retry/limit behavior unchanged or better |
| A2 | Behavioral | Tool-call params round-tripped through `map[string]any` | `ToolCall.Parameters` preserved as **raw JSON** | No lossy precision/number coercion | Maestro tool-arg parsing must accept raw JSON (unmarshal itself) |
| A3 | Behavioral | System extracted from in-band system messages | Driven by `ChatRequest.System`, **text-only validated** | Spec model; deterministic | Maestro must pass system via `System`, not a system-role message |
| A4 | Behavioral | Malformed parts silently dropped | Empty/nil/unknown parts → `bad_request` | Fail loud, not silent corruption | Maestro must not send empty placeholder parts |

## OpenAI embeddings (PR #6)

| # | Kind | Maestro | maestro-llms | Why | Cut-over action |
|---|---|---|---|---|---|
| OE1 | Behavioral | (n/a — Maestro had no embeddings client) | Vectors placed by provider `Index`, ID-tagged; duplicate/missing index → error | Order/id correctness contract | New capability; no Maestro behavior to preserve |
| OE2 | Behavioral | (n/a) | >2048 inputs → `bad_request` (app owns chunking) | Predictable batch contract | Maestro ingestion must chunk |

## OpenAI chat — Responses API (PR #10)

| # | Kind | Maestro | maestro-llms | Why | Cut-over action |
|---|---|---|---|---|---|
| OC1 | **Behavioral (significant)** | Entire conversation **string-flattened** into one text blob (`System: …\n[Tool Call: …]\n[Tool Result …]`) | **Structured Responses input items** (message / function_call / function_call_output) | Faithful tool round-trips; not a lossy text hack | Validate Maestro tool-using flows against the structured path; output quality should improve but behavior differs |
| OC2 | **Behavioral (significant)** | Hard-codes `tool_choice = required` whenever tools are present | Honors caller `ToolChoice` (auto default) | Caller controls tool use | Maestro flows that relied on forced tool use must set `ToolChoice{Type: tool}`/`required` explicitly |
| OC3 | Internal | Output text via `OutputText()` only | Output items iterated in order; `Message` preserves interleaving | Round-trip source-of-truth ordering | None observable beyond ordering fidelity |

## Google chat — genai (PR #11, pending)

| # | Kind | Maestro | maestro-llms | Why | Cut-over action |
|---|---|---|---|---|---|
| G1 | **Behavioral (significant)** | Caches assistant responses (`responseCache`) to replay Gemini **thought signatures** across turns | **Dropped** — clients are stateless/concurrency-safe; conversation round-trips via the app-neutral message history only | Mutable per-client turn-indexed cache violates the stateless + concurrent-safe contract | Validate multi-turn Gemini tool loops; thinking-model quality may differ without thought-signature replay. Revisit if a stateless encoding is needed. |
| G2 | Behavioral | Forces `FunctionCallingConfigMode = ANY` when tools present | Honors caller `ToolChoice` | Caller controls tool use | Same as OC2 for Gemini |
| G3 | Behavioral | `StopReason` hard-coded `"end_turn"`; usage dropped | Real finish reason + usage populated | Accurate stop/accounting | Maestro can now read real finish reasons |

> Rows for Ollama chat (PR #12) to be appended in that PR.

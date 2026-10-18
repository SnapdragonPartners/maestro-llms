# 0023. `Usage.InputTokens` includes cached input; cache reads and writes are subsets

- **Status:** Accepted
- **Date:** 2026-10-17

## Context

Before this change, `Usage` had one input field and two cache fields with no
stated relationship, and the adapters disagreed (#95):

| Adapter | `InputTokens` | Cache fields |
|---|---|---|
| Anthropic | wire `input_tokens`, which **excludes** cache reads and writes | mapped |
| OpenAI (Responses) | `input_tokens`, which **includes** cached tokens | not mapped |
| vllm (Chat Completions) | `prompt_tokens`, which **includes** cached tokens | not mapped |
| Google | `promptTokenCount`, which **includes** `cachedContentTokenCount` | not mapped |
| Ollama | `prompt_eval_count` | none reported |

buggrit saw `CacheReadTokens` stay 0 on LunaRoute while the raw usage showed
16–17k cached tokens per call. It runs 30+ turn tool loops and wants to report
cache hit rates. The `Usage` doc's budget math also says a length cut-off fires
as `InputTokens + OutputTokens + ReasoningTokens` nears the context limit.
Cached tokens still fill the context, so that sum was wrong for Anthropic
whenever caching was on.

## Decision

1. **`InputTokens` is all input processed, including cached tokens, on every
   provider.** `CacheReadTokens` and `CacheWriteTokens` are subsets of it.
   `InputTokens − CacheReadTokens − CacheWriteTokens` is the uncached
   remainder (Dan, recorded on #95).
2. **The invariant is enforced at one place:** `llms.CacheSplit(input, read,
   write)` clamps reads to `[0, input]` and writes to `[0, input − read]`, so
   the remainder is never negative even if a server reports inconsistent
   counts. The OpenAI, vllm and Google adapters use it.
3. **Per adapter:**
   - **Anthropic:** `InputTokens` and `TotalTokens` add `cache_read_input_tokens`
     and `cache_creation_input_tokens` to the wire `input_tokens`.
   - **OpenAI (Responses):** `input_tokens_details.cached_tokens` and
     `cache_write_tokens`.
   - **vllm (Chat Completions):** `prompt_tokens_details.cached_tokens` and
     `cache_write_tokens`. When `cache_write_tokens` is absent, the adapter
     reads the `created_cache_tokens` extension that LunaRoute
     (DeepSeek-style servers) reports instead. buggrit reaches LunaRoute
     through this adapter, so this is the path #95 needed.
   - **Google:** `cachedContentTokenCount` maps to `CacheReadTokens`. Gemini
     reports no cache writes.
   - **Ollama:** unchanged; it reports no cache accounting.
4. **The in-memory limiter** charges `TotalTokens` (or input + output) and no
   longer adds the cache fields on top. Under (1) they're already included,
   so adding them would count cached input twice.

## Consequences

- One input convention everywhere, and cost math is the same on every
  provider: price the uncached remainder, the cache reads and the cache
  writes separately.
- **Breaking for Anthropic consumers.** `InputTokens` and `TotalTokens` grow by
  the cached amount. Code that treated Anthropic `InputTokens` as uncached
  must subtract the cache fields. This goes in the release notes and is
  divergence X9.
- Limiter reconciliation for Anthropic now counts cached input, which is the
  conservative direction. It's no longer double-counted for the other
  providers.
- Verified live on 2026-10-17 (input / cache read):
  - Anthropic Haiku 4.5 with a breakpoint: 9621 / 9604.
  - OpenAI gpt-4o-mini: 8025 / 7808.
  - Gemini 2.5 Flash: 9558 / 9195.
  - LunaRoute DeepSeek via vllm: 8044 / 7936.

## References

- Issue #95 (the evidence and the decision comment). `llms/usage.go`
  (`Usage` docs, `CacheSplit`), each adapter's usage builder,
  `llms/ratelimit/memory.go` (`effectiveTokens`).
- Fixtures: `openai/testdata/lunaroute_responses_cached.json`,
  `vllm/testdata/lunaroute_chat_cached.json`, `vllm/testdata/lunaroute_chat_cache_write.json`,
  `google/testdata/gemini25_flash_implicit_cache.json`.
- Live: `livetest.CachedInput`.
- ADR-0016 / ADR-0018 (the output-side split this mirrors).

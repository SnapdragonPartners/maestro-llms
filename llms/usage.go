package llms

// Usage is normalized token accounting across chat and embeddings. Fields a
// provider does not report are left zero. Middleware may estimate usage before
// a call; provider clients fill actual usage when available.
//
// Cross-provider semantics for the output split (ADR-0016):
//
//   - OutputTokens is the VISIBLE assistant output only — the tokens
//     that a consumer rendering the message would count.
//   - ReasoningTokens is the count of "thinking" tokens consumed by
//     reasoning models — separately metered output that does NOT
//     appear in the visible response (Gemini 3 thoughts, OpenAI
//     o-series reasoning content, Anthropic thinking). Where the
//     provider exposes this split, the toolkit reports it; where it
//     doesn't, ReasoningTokens is zero.
//   - BillableOutputTokens is what the provider charges you as "output
//     tokens." It equals OutputTokens + ReasoningTokens for every
//     adapter. Since ADR-0018, Anthropic's OutputTokens is visible-only
//     too; an Anthropic response that omits output_tokens_details
//     reports the combined count in OutputTokens with ReasoningTokens
//     zero.
//
// Cross-provider semantics for input and caching (ADR-0023):
//
//   - InputTokens is ALL input the provider processed, including tokens
//     read from or written to a prompt cache. Cached tokens still occupy
//     the context window.
//   - CacheReadTokens and CacheWriteTokens are subsets of InputTokens, so
//     InputTokens - CacheReadTokens - CacheWriteTokens is the uncached
//     remainder (never negative). Cost math prices the three separately.
//
// TotalTokens is a provider-specific aggregate (ADR-0034), not a field
// with one cross-provider definition. Google, OpenAI and OpenAI-compatible
// servers report it as sent; Gemini's includes tool-use prompt tokens that
// InputTokens doesn't. The Anthropic adapter derives it as InputTokens +
// BillableOutputTokens, and Ollama as prompt plus generated tokens. Don't
// assume it equals the sum of the other fields; cost and budget math should
// use those.
//
// EmbeddingTokens is the input token count of an embedding call. It counts
// the same tokens InputTokens would, so the two overlap and are never summed;
// the toolkit's limiters charge the larger.
//
// Budget math: a length-truncation fires when
// InputTokens + OutputTokens + ReasoningTokens approaches the cap, not
// when OutputTokens alone does. Callers that see a small OutputTokens
// paired with a length stop reason should consult ReasoningTokens to
// understand where the budget went. Callers doing cost/billing math
// should read BillableOutputTokens.
type Usage struct {
	InputTokens          int
	OutputTokens         int
	TotalTokens          int
	ReasoningTokens      int
	BillableOutputTokens int
	EmbeddingTokens      int
	CacheReadTokens      int
	CacheWriteTokens     int
	ProviderRequestID    string
}

// CacheSplit clamps provider-reported cache reads and writes so they are
// subsets of input: read in [0, input], write in [0, input-read]. Adapters
// use it so InputTokens - CacheReadTokens - CacheWriteTokens is never
// negative even when a server reports inconsistent counts (ADR-0023).
func CacheSplit(input, read, write int) (cacheRead, cacheWrite int) {
	input = max(input, 0)
	cacheRead = min(max(read, 0), input)
	cacheWrite = min(max(write, 0), input-cacheRead)
	return cacheRead, cacheWrite
}

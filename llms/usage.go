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
//     adapter (ADR-0018 retired the Anthropic exception). An Anthropic
//     response that omits output_tokens_details reports the combined
//     count in OutputTokens with ReasoningTokens zero.
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

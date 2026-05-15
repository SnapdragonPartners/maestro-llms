package llms

// Usage is normalized token accounting across chat and embeddings. Fields a
// provider does not report are left zero. Middleware may estimate usage before
// a call; provider clients fill actual usage when available.
type Usage struct {
	InputTokens       int
	OutputTokens      int
	TotalTokens       int
	EmbeddingTokens   int
	CacheReadTokens   int
	CacheWriteTokens  int
	ProviderRequestID string
}

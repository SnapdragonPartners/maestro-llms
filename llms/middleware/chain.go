package middleware

import "github.com/SnapdragonPartners/maestro-llms/llms"

// ChatMiddleware wraps a ChatClient, returning a decorated ChatClient.
type ChatMiddleware func(llms.ChatClient) llms.ChatClient

// EmbeddingMiddleware wraps an EmbeddingClient.
type EmbeddingMiddleware func(llms.EmbeddingClient) llms.EmbeddingClient

// ChainChat composes middleware around base. The first argument is the
// outermost wrapper: ChainChat(base, A, B, C) yields A(B(C(base))), so a call
// flows A -> B -> C -> base.
func ChainChat(base llms.ChatClient, mws ...ChatMiddleware) llms.ChatClient {
	for i := len(mws) - 1; i >= 0; i-- {
		base = mws[i](base)
	}
	return base
}

// ChainEmbeddings composes middleware around base with the same
// first-argument-outermost ordering as ChainChat.
func ChainEmbeddings(base llms.EmbeddingClient, mws ...EmbeddingMiddleware) llms.EmbeddingClient {
	for i := len(mws) - 1; i >= 0; i-- {
		base = mws[i](base)
	}
	return base
}

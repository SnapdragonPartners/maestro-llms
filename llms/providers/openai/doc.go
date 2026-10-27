// Package openai implements the maestro-llms clients for OpenAI: New builds
// the chat Client (the Responses API) and NewEmbeddings the EmbeddingClient,
// matching the constructor names of the other providers (ADR-0035). The
// Option set is shared; an embeddings-only option passed to New is a config
// error.
package openai

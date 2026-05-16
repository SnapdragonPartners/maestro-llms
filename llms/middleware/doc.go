// Package middleware provides provider-neutral chat and embedding middleware
// (timeout, retry, circuit breaker, rate limiting, metrics, validation) and
// the ChainChat and ChainEmbeddings composition helpers. The first middleware
// argument to a chain is outermost.
//
// Limitations: every middleware in this package wraps Complete (and Embed)
// only. The returned wrappers implement ChatClient/EmbeddingClient but not the
// optional llms.StreamingChatClient capability, so composing them around a
// future streaming-capable client makes that capability undiscoverable
// through the chain (a type assertion for StreamingChatClient fails on the
// wrapper). This is deliberate: no consumer needs streaming yet and
// streaming-aware retry/timeout/circuit semantics are intentionally not
// designed. See docs/adr/0003-middleware-complete-only-defer-streaming.md.
package middleware

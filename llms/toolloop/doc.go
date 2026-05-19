// Package toolloop is a small, app-neutral helper that wraps the
// provider-protocol tool round-trip over an llms.ChatClient: send a request,
// execute every tool call the model emits, append the matching tool results,
// and repeat until the model returns a response with no tool calls (final
// answer), the configured iteration limit is reached, the provider or an
// executor errors, or the caller cancels the context.
//
// The full design and binding non-goals are in docs/toolloop-proposal.md
// (accepted as ADR-0011). This is deliberately a tool loop, not an agent
// loop: it has no agent state, no terminal/state-transition tools, no
// built-in persistence, audit taxonomy, authorization, tool adapters,
// schema-generation, or moderation hooks. Applications layer those around
// the loop.
//
// Streaming tool loops are deferred per ADR-0003 — Run is synchronous; a
// streaming-aware tool loop requires a separate ADR before it can be added.
package toolloop

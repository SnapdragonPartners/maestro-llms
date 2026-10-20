// Package toolloop is a small, app-neutral helper that wraps the
// provider-protocol tool round-trip over an llms.ChatClient: send a request,
// execute every tool call the model emits, append the matching tool results,
// and repeat until the model returns a response with no tool calls (a final
// answer, or an empty response with no text either), an executor marks its result terminal (ToolResult.Stop), the
// configured iteration limit is reached (if any; see NoIterationLimit), the
// provider or an executor errors, or the loop's context is canceled or
// reaches its deadline.
//
// The full design and binding non-goals are in docs/toolloop-proposal.md
// (accepted as ADR-0011). This is deliberately a tool loop, not an agent
// loop: it has no agent state, no terminal/state-transition tools, no
// built-in persistence, audit taxonomy, authorization, tool adapters,
// schema-generation, or moderation hooks. Applications layer those around
// the loop.
//
// The one transcript-shaping hook is Config.Compact (ADR-0030), which lets
// the application compact the transcript before a turn or after a
// context-window overflow; what to keep stays app policy, and
// llms/toolloop/compact ships two ready-made policies.
//
// Streaming tool loops are deferred per ADR-0003 — Run is synchronous; a
// streaming-aware tool loop requires a separate ADR before it can be added.
package toolloop

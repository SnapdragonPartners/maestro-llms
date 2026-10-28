// Package toolloop is a small, app-neutral helper that wraps the
// provider-protocol tool round-trip over an llms.ChatClient: send a request,
// execute every tool call the model emits, append the matching tool results,
// and repeat. Run stops when the model answers with no tool calls (a final
// answer, or an empty response), an executor marks its result terminal
// (ToolResult.Stop), the configured iteration limit is reached (if any; see
// NoIterationLimit), the provider, an executor or the compaction hook errors,
// the context window overflows and compaction can't recover, or the loop's
// context is canceled or reaches its deadline. Outcome.Kind names which; the
// OutcomeKind constants document each one.
//
// The full design and binding non-goals are in docs/toolloop-proposal.md
// (accepted as ADR-0011). This is deliberately a tool loop, not an agent
// loop: it has no agent state, no state-transition tools (a terminal tool
// only ends the loop, ADR-0017), no built-in persistence, audit taxonomy,
// authorization, tool adapters, schema-generation, or moderation hooks.
// Applications layer those around the loop.
//
// The one transcript-shaping hook is Config.Compact (ADR-0030), which lets
// the application compact the transcript before a turn or after a
// context-window overflow; what to keep stays app policy, and
// llms/toolloop/compact ships two ready-made policies.
//
// Streaming tool loops are deferred per ADR-0003 — Run is synchronous; a
// streaming-aware tool loop requires a separate ADR before it can be added.
package toolloop

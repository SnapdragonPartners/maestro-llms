# Tool Loop Proposal

Status: Accepted (ADR-0011)  
Date: 2026-05-19

## Purpose

`maestro-llms` may eventually include a small, app-neutral tool loop helper
for applications that want to let a model call ordinary tools, execute those
tools in-process, and continue the conversation until the model produces a
final assistant response or the loop reaches a configured stop condition.

The intended first consumer is Morris-style tool use: relatively generic,
request-scoped tools whose execution functions are supplied by the
application. This is not intended to absorb Maestro's agent/tool
infrastructure, terminal state-machine tools, persistence model, watchdogs,
or escalation policy.

The package should make the provider-neutral tool-call round trip easy and
hard to misuse, while keeping all side effects and authorization decisions in
the application.

This is a tool loop, not a full agent loop. Applications may use it inside a
larger agent or chat service, but the abstraction here is only:

```text
model -> tool calls -> execute tools -> feed results back -> repeat
```

## Goals

1. Provide a reusable synchronous loop over `llms.ChatClient`.
2. Execute every model-requested tool call before the next provider request.
3. Preserve the existing `llms.Message` / `ContentPart` transcript model.
4. Preserve `ToolCall.ProviderSignature` by appending the exact assistant
   message returned by the provider.
5. Let applications supply tool definitions and execution functions.
6. Return a complete transcript and a typed loop outcome.
7. Keep the API small, deterministic, and easy to test with `llms/testllm`.
8. Avoid importing application concepts from Maestro or Morris.
9. Let an application evolve a `Complete` call site into a tool loop without
   rewriting its chat handler, UI contract, or persistence boundary.

## Non-Goals

- No Maestro agent concepts.
- No claim that this package is a general-purpose agent framework.
- No terminal tool or state-transition abstraction.
- No `ProcessEffect`, story IDs, request IDs, or workflow signals.
- No built-in persistence, audit taxonomy, or database schema.
- No built-in authorization, tenant isolation, or secret resolution.
- No built-in filesystem, shell, browser, retrieval, or MCP tool adapters.
- No typed argument binding, schema-generation helper, or tool registry
  framework.
- No human escalation policy.
- No built-in moderation, policy enforcement, or "police output" hook.
- No automatic streaming support in the initial version.
- No prompt management or context compaction.

Applications may build those behaviors around the tool loop, but they should
not be part of the reusable package.

## Proposed Package Shape

The helper should live outside the core `llms` package so callers that do not
need it do not inherit extra API surface:

```text
llms/toolloop/
```

The package depends only on the core `llms` API and the Go standard library.

## Core Types

### Tool

```go
type Tool struct {
    Definition llms.ToolDefinition
    Execute    func(context.Context, llms.ToolCall) (ToolResult, error)
}
```

`Definition` is sent to the provider in `ChatRequest.Tools`. `Execute` is the
application-owned implementation. It receives the complete `llms.ToolCall`,
including raw JSON parameters and any provider-owned metadata, and returns the
result that should be sent back to the model.

The execution function deliberately receives the full call rather than only
decoded arguments so applications can:

- Unmarshal parameters into their own typed structs.
- Include the tool call ID in logs or audit records.
- Decide how to handle malformed JSON.
- Preserve provider-specific details without the loop understanding them.

### ToolResult

```go
type ToolResult struct {
    Content string
    IsError bool
}
```

`Content` maps to `llms.ToolResult.Content`. `IsError` maps to
`llms.ToolResult.IsError`.

The loop should not interpret the result payload. If an application wants
structured tool output, it can encode JSON into `Content`.

`ToolResult{IsError: true}` is a model-visible tool failure: the loop appends
it to the transcript and lets the model recover. An `Execute` error is a
loop-visible abort: the loop stops and returns `OutcomeToolError` unless the
context was canceled, in which case it returns `OutcomeCanceled`.

### Config

```go
type Config struct {
    Client llms.ChatClient
    Request llms.ChatRequest
    Tools []Tool

    MaxIterations int
    ToolChoice llms.ToolChoice

    OnIteration func(IterationEvent)
    OnToolCall  func(ToolCallEvent)
}
```

`Client`, `Request`, and `Tools` are required. `Tools` must not contain
duplicate names, and every tool must have a non-nil `Execute` function.

`Request.Messages` is the initial transcript. `Request.System`, `Purpose`,
`MaxTokens`, `Temperature`, and `Metadata` are reused on every provider call.
`Request.Tools` must be empty; the loop derives provider tool definitions from
`Config.Tools` so tool definitions and executors cannot drift. Supplying both
`Request.Tools` and `Config.Tools` is a configuration error.

`MaxIterations` limits the number of provider responses that contain tool
calls. A zero value should use a conservative default, such as 8. This default
is only a runaway bound, not a tuning recommendation; applications with known
workflows should set an explicit limit from their domain.

`ToolChoice` overrides `Request.ToolChoice` when non-zero. Supplying both a
non-zero `Config.ToolChoice` and a non-zero `Request.ToolChoice` is a
configuration error. If both are zero, the loop should default to
`llms.ToolChoiceAuto`. Applications that require tool use can set
`llms.ToolChoiceRequired`, but should remember that Ollama cannot enforce it.

Callbacks are optional observation hooks. They must not be required for
correctness.

### Events

```go
type IterationEvent struct {
    Index        int
    Response     llms.ChatResponse
    NumToolCalls int
}

type ToolCallEvent struct {
    Iteration int
    Call      llms.ToolCall
    Result    ToolResult
    Latency   time.Duration
    Err       error
}
```

Events carry provider-neutral observations only. They must not grow story IDs,
tenant IDs, agent state, request IDs, audit labels, or other application
concepts. Applications can correlate events by closing over their own state in
the callback.

`IterationEvent` is emitted after a provider response is received and appended
to the transcript. `ToolCallEvent` is emitted after each tool execution
attempt, including attempts that return an error.

### Run

```go
func Run(ctx context.Context, cfg Config) Outcome
```

`Run` performs the complete synchronous loop. It should always return an
`Outcome`, not a separate `(Outcome, error)` pair, so callers can switch on one
result shape and still inspect the final transcript on non-success outcomes.

### Outcome

```go
type OutcomeKind string

const (
    OutcomeFinalAnswer   OutcomeKind = "final_answer"
    OutcomeMaxIterations OutcomeKind = "max_iterations"
    OutcomeLLMError      OutcomeKind = "llm_error"
    OutcomeToolError     OutcomeKind = "tool_error"
    OutcomeCanceled      OutcomeKind = "canceled"
)

type Outcome struct {
    Kind       OutcomeKind
    Response   llms.ChatResponse
    Messages   []llms.Message
    TotalUsage llms.Usage
    Iterations int
    Err        error
}
```

`Messages` is the full transcript after the loop stops, including assistant
tool-call messages and tool-result messages.

`TotalUsage` is the best-effort sum of every `ChatResponse.Usage` observed
during the loop, including the final response when present. Numeric token
fields are added directly. `ProviderRequestID` cannot represent multiple
requests, so the loop should leave it empty unless exactly one provider
request contributed usage.

When `Kind == OutcomeFinalAnswer`, `Response` is the final provider response
that did not request tools.

For non-final outcomes, `Response` is the last provider response received, if
any. It is zero when no provider response was received.

`Iterations` is the number of provider responses that contained tool calls. A
single provider response with no tool calls returns `OutcomeFinalAnswer` with
`Iterations == 0`.

When `Kind == OutcomeMaxIterations`, `Messages` contains all completed prior
tool iterations plus the unresolved limit-hitting assistant message. The loop
should not make an extra provider call after the limit is reached unless the
application explicitly asks for that behavior. The default limit behavior is
pre-execute: if the limit-hitting provider response contains tool calls, the
loop appends that assistant message and returns `OutcomeMaxIterations` without
executing those tool calls.

Because pre-execute limit stopping leaves unresolved assistant tool-call parts
at the end of `Messages`, the returned transcript is diagnostic state, not a
valid transcript to feed directly into another `Complete` call. An application
that wants to resume from it must first append matching error tool results for
the unresolved calls or otherwise repair the pairing invariant.

`OutcomeToolError` is reserved for loop-level execution failures, such as
duplicate tool registrations, nil executors, or executor errors. Tool execution
errors that should be shown to the model should be represented by
`ToolResult{IsError: true}` and continue the loop.

`Err` is set by outcome kind:

- `OutcomeFinalAnswer`: nil.
- `OutcomeMaxIterations`: nil.
- `OutcomeLLMError`: the error returned by `ChatClient.Complete`.
- `OutcomeToolError`: the configuration validation error or `Execute` error.
- `OutcomeCanceled`: a cancellation error; callers should use `errors.Is(err,
  context.Canceled)` rather than checking a concrete type.

## Execution Semantics

The loop executes synchronously:

1. Validate `Config`. Invalid configuration returns `OutcomeToolError`.
2. Copy the initial transcript from `Request.Messages`.
3. Build a `llms.ChatRequest` from the base request, current transcript,
   derived tool definitions, and configured `ToolChoice`.
4. Call `Client.Complete`.
5. If `Complete` returned an error, return `OutcomeCanceled` when
   `errors.Is(err, context.Canceled)`, otherwise return `OutcomeLLMError`.
6. Add `resp.Usage` to `Outcome.TotalUsage`.
7. Append `resp.Message` to the transcript exactly as returned.
8. Emit `OnIteration`, if configured.
9. If `resp.ToolCalls` is empty, return `OutcomeFinalAnswer`.
10. Increment `Iterations`.
11. If the iteration limit has been reached, return `OutcomeMaxIterations`.
12. For every tool call in the assistant response, find the matching tool by
   name and execute it.
13. Emit `OnToolCall`, if configured.
14. If the executor returned an error, return `OutcomeToolError` unless the
   context was canceled, in which case return `OutcomeCanceled`.
15. Append one `llms.ToolResultMessage` containing one result for every tool
   call in the assistant response.
16. Repeat.

The loop must execute every tool call in a tool-call turn before making the
next provider request. This matches the validation middleware's structural
rule that every assistant tool call must receive a corresponding tool result
before the next user or assistant turn.

Tool results should be appended in the same order as `resp.ToolCalls`.

If the context is canceled before or during the provider call or during tool
execution, the loop returns `OutcomeCanceled`. Cancellation detection should
use `errors.Is(err, context.Canceled)`, matching the toolkit's provider-error
contract that caller cancellation is not converted into a `*llms.ProviderError`.

## Concurrency Contract

`Run` is safe to call concurrently with the same `Config` if the caller treats
the config, request, tools slice, tool definitions, and metadata as immutable
for the duration of each run. The loop must not mutate `Config` or
`Config.Request`; it should copy the transcript before appending loop messages.

`llms.ChatClient` implementations are already expected to be concurrency-safe.
Tool executors and callbacks are application code; if they close over shared
state, the application is responsible for making that state safe for concurrent
use.

## Unknown Tools

If the model calls a tool that was not registered, the default behavior should
be to append an error tool result and continue:

```text
tool "name" is not available
```

This gives the model a chance to recover when tool choice is not strictly
enforced by the provider. A config flag may later allow fail-fast behavior,
but recovery should be the default because it preserves the provider protocol.
Unknown-tool recovery should emit `OnToolCall` with an error `ToolResult` and
nil `Err`, because the loop itself continued successfully.

## Tool Execution Errors

The package should not prescribe an error taxonomy for application tools.
Instead, applications choose whether a failure is recoverable.

Recoverable, model-visible failures should return a tool result with
`IsError: true`:

```go
return ToolResult{Content: "not found", IsError: true}, nil
```

Non-recoverable, loop-visible failures should return an error:

```go
return ToolResult{}, err
```

Many tool failures are useful model feedback rather than loop failures, so
executors should reserve `error` for cases where the loop itself cannot
continue: context cancellation, infrastructure failure, corrupted local state,
authorization infrastructure failure, or other fatal application conditions.

## Truncation

Providers report truncation through provider-specific `StopReason` values.
The loop should not attempt a universal truncation classifier in v1.

Applications that need truncation recovery can inspect
`Outcome.Response.StopReason`, wrap the client, set `OnIteration`, or run
their own policy around `Outcome`.

## Future Hooks

A later version may add an optional truncation predicate:

```go
IsTruncated func(llms.ChatResponse) bool
```

If set, the loop could append an application-provided recovery message rather
than executing potentially incomplete tool calls.

## Parallel Execution

The initial version should execute tool calls sequentially. That is simpler,
deterministic, and avoids surprising applications whose tools have ordering
constraints or shared dependencies.

A future version may add:

```go
ParallelTools bool
```

If enabled, the loop must still append tool results in the original tool-call
order.

## Streaming

The initial tool loop is synchronous only. A streaming-aware tool loop is
deferred per ADR-0003: streaming tool-call orchestration needs
streaming-aware retry, timeout, circuit breaker, transcript, and partial-output
semantics first. That should be a separate ADR rather than an extension hidden
inside this proposal.

## Middleware Interaction

The tool loop should not duplicate middleware behavior. Callers should wrap
the `llms.ChatClient` before passing it to the loop:

```go
client := middleware.RecommendedChat(...)(providerClient)
out := toolloop.Run(ctx, toolloop.Config{Client: client, ...})
```

Validation middleware remains valuable because it verifies the transcript the
loop builds. Retry, timeout, circuit breaker, metrics, and rate limiting remain
client concerns.

## Chat Service Integration

The expected adoption path is that an application chat service starts with one
provider call:

```go
resp, err := client.Complete(ctx, req)
```

and later replaces only the orchestration implementation:

```go
out := toolloop.Run(ctx, toolloop.Config{
    Client:  client,
    Request: req,
    Tools:   tools,
})
```

The surrounding service boundary should stay stable:

```text
build request -> run completion/orchestration -> apply app policy -> persist/return response
```

This is important for Morris-style chat because the MVP can remain a single
`Complete` call while the service is already shaped for future tool use. The
handler, UI response contract, and persistence path should not need to know
whether the orchestration implementation made one provider call or several
tool-call turns.

Application policy hooks, such as output moderation or "police output"
checks, belong outside this package at the chat-service orchestration boundary.
In the MVP they can run after `Complete`; with the tool loop they can run
after `OutcomeFinalAnswer`. Applications that need stricter controls can also
apply policy before tool execution, after tool execution, or before persisting
the transcript.

## Example

```go
weather := toolloop.Tool{
    Definition: llms.ToolDefinition{
        Name:        "get_weather",
        Description: "Get current weather for a city.",
        InputSchema: json.RawMessage(`{
            "type":"object",
            "properties":{"city":{"type":"string"}},
            "required":["city"]
        }`),
    },
    Execute: func(ctx context.Context, call llms.ToolCall) (toolloop.ToolResult, error) {
        var args struct {
            City string `json:"city"`
        }
        if err := json.Unmarshal(call.Parameters, &args); err != nil {
            return toolloop.ToolResult{Content: err.Error(), IsError: true}, nil
        }
        return toolloop.ToolResult{
            Content: `{"city":"Paris","temp_c":18,"summary":"clear"}`,
        }, nil
    },
}

out := toolloop.Run(ctx, toolloop.Config{
    Client: client,
    Request: llms.ChatRequest{
        Purpose:  llms.PurposeChat,
        Messages: []llms.Message{llms.UserText("Weather in Paris?")},
        MaxTokens: 512,
    },
    Tools: []toolloop.Tool{weather},
    ToolChoice: llms.ToolChoice{Type: llms.ToolChoiceAuto},
})

if out.Kind == toolloop.OutcomeFinalAnswer {
    fmt.Println(out.Response.Text)
}
```

## Why This Is Still Useful With App-Supplied Executors

Tool execution functions must be app-supplied because tools are side effects.
That does not make the loop trivial. The reusable value is in consistently
handling the provider protocol:

- Building requests with the right tool definitions.
- Preserving assistant tool-call messages exactly.
- Returning all required tool results before the next model turn.
- Preserving provider signatures for models that require them.
- Keeping transcripts valid across providers.
- Providing a predictable stop and outcome model.
- Making simple tool-calling flows testable without an application framework.

This is enough for Morris-style tool calls and other apps with ordinary
request-scoped tools. It is intentionally not enough for Maestro's full agent
runtime, which should continue to own its richer orchestration layer.

## Open Questions

1. Should a future version add `FailOnUnknownTool`, or is model-visible
   unknown-tool recovery always the right default?
2. Should a future version add `ParallelTools`, or should concurrency stay
   entirely application-owned?

## ADR Path

If accepted for implementation, this proposal should become ADR-0011:
"Tool loop helper — scope, API, and non-goals." The ADR should keep the
non-goals binding, because they are the main guardrail against drifting from a
provider-protocol helper into an application runtime.

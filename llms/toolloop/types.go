package toolloop

import (
	"context"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// Tool is one application-supplied tool the model may call. Definition is the
// schema sent to the provider in ChatRequest.Tools; Execute is the
// app-owned implementation invoked when the model emits a matching call.
// Execute receives the complete llms.ToolCall (raw JSON parameters,
// provider signature, etc.) so applications can do their own typed
// unmarshalling, log the call ID, and preserve provider-owned metadata
// without the loop interpreting it.
type Tool struct {
	Execute    func(context.Context, llms.ToolCall) (ToolResult, error)
	Definition llms.ToolDefinition
}

// ToolResult is the result an executor returns. It is the loop-facing shape
// and is translated into llms.ToolResult (with the tool call ID wired in)
// when appended to the transcript. The loop never interprets Content;
// applications that want structured output may encode JSON into it.
//
// IsError is a model-visible signal: the loop appends the result and the
// model can recover. An Execute error (the second return) is a loop-visible
// abort: the loop stops and returns OutcomeToolError unless the context was
// canceled, in which case it returns OutcomeCanceled.
type ToolResult struct {
	Content string
	IsError bool
}

// Config drives a single Run. Client, Request, and Tools are required.
// Defaults: MaxIterations <= 0 uses defaultMaxIterations; a zero ToolChoice
// (in both Request and Config) defaults to llms.ToolChoiceAuto.
//
// Fail-closed configuration: see Run for the validation rules — duplicated
// tools across Request.Tools/Config.Tools, duplicated ToolChoice across
// Request/Config, duplicate tool names, nil Execute, and
// ToolChoice.RequiresTools with no tools are all rejected before any
// provider call.
//
// OnIteration and OnToolCall are optional observation hooks. They must
// never be required for correctness; the loop does not block on them and
// applications correlate events by closing over their own state in the
// callback.
type Config struct {
	Request       llms.ChatRequest
	Client        llms.ChatClient
	OnIteration   func(IterationEvent)
	OnToolCall    func(ToolCallEvent)
	ToolChoice    llms.ToolChoice
	Tools         []Tool
	MaxIterations int
}

// IterationEvent is emitted after each provider response is received and
// appended to the transcript. Index is 0-based across the whole Run. Fields
// are provider-neutral observations only: this struct must not grow story
// IDs, tenant IDs, agent state, request IDs, audit labels, or other
// application concepts (see ADR-0011's binding non-goals).
type IterationEvent struct {
	Response     llms.ChatResponse
	Index        int
	NumToolCalls int
}

// ToolCallEvent is emitted after each tool execution attempt, including
// attempts that returned an error and including unknown-tool recovery
// (where Err is nil and Result is an IsError tool result the loop
// synthesized so the model can self-correct). Iteration is the 0-based
// IterationEvent.Index of the assistant turn that emitted Call. Same
// neutrality rule as IterationEvent: no app-specific fields.
type ToolCallEvent struct {
	Err       error
	Call      llms.ToolCall
	Result    ToolResult
	Latency   time.Duration
	Iteration int
}

// OutcomeKind discriminates Outcome. The set is closed in v1; new kinds
// require a superseding ADR.
type OutcomeKind string

const (
	// OutcomeFinalAnswer is returned when the model emits a response with
	// no tool calls. Response is that response; Err is nil.
	OutcomeFinalAnswer OutcomeKind = "final_answer"
	// OutcomeMaxIterations is returned when the loop has observed the
	// configured number of tool-calling responses. The limit-hitting
	// assistant message is appended to Messages as diagnostic state; its
	// tool calls are NOT executed and Messages is NOT directly re-feedable
	// into Complete without repairing the unresolved tool-call pairing.
	// Err is nil.
	OutcomeMaxIterations OutcomeKind = "max_iterations"
	// OutcomeLLMError is returned when ChatClient.Complete returned a
	// non-cancellation error. Err is that error; Response is the last
	// successful provider response, if any.
	OutcomeLLMError OutcomeKind = "llm_error"
	// OutcomeToolError is returned for a loop-level execution failure:
	// configuration validation, or an Execute that returned an error.
	// Model-visible tool failures (ToolResult{IsError: true}) do NOT
	// produce OutcomeToolError — the loop continues.
	OutcomeToolError OutcomeKind = "tool_error"
	// OutcomeCanceled is returned when the loop's context was canceled
	// during the provider call or during tool execution. Detect via
	// errors.Is(err, context.Canceled) rather than checking a concrete
	// type — caller cancel is not converted into a *llms.ProviderError
	// (cross-cutting X5).
	OutcomeCanceled OutcomeKind = "canceled"
)

// Outcome is the result of a single Run. It is always returned (no
// separate error) so callers can switch on Kind and still inspect the
// transcript on non-success outcomes.
//
// Messages is the full transcript at the moment the loop stopped,
// including the initial Request.Messages, every assistant response
// received, and every tool result message the loop appended.
//
// TotalUsage is the best-effort sum of every Usage observed during the
// run, including the final response when present. Numeric token fields
// are added directly. ProviderRequestID cannot represent multiple
// requests and is left empty unless exactly one provider request
// contributed usage.
//
// Iterations is the number of provider responses that contained tool
// calls. A single response with no tool calls returns OutcomeFinalAnswer
// with Iterations == 0.
//
// Err is set by Kind:
//   - OutcomeFinalAnswer:   nil
//   - OutcomeMaxIterations: nil
//   - OutcomeLLMError:      the error returned by ChatClient.Complete
//   - OutcomeToolError:     the configuration validation error or Execute error
//   - OutcomeCanceled:      a cancellation error; use errors.Is(err, context.Canceled)
type Outcome struct {
	Response   llms.ChatResponse
	Err        error
	TotalUsage llms.Usage
	Kind       OutcomeKind
	Messages   []llms.Message
	Iterations int
}

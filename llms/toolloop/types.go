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
// abort: the loop stops and returns OutcomeToolError unless the loop's
// context was canceled or reached its deadline, in which case it returns
// OutcomeCanceled or OutcomeDeadline.
//
// Stop marks this call as terminal (ADR-0017): the loop appends the result
// and returns OutcomeTerminalTool instead of making another provider call.
// It is a per-result decision so an executor can validate first — an
// invalid submission returns IsError: true (the model fixes and
// resubmits), and only a valid one sets Stop. Stop and IsError together
// are a contract violation and produce OutcomeToolError. Like a return
// statement, Stop ends the assistant turn at that call: later calls in the
// same turn are not executed and receive a synthesized IsError result so
// the transcript stays well-formed.
type ToolResult struct {
	Content string
	IsError bool
	Stop    bool
}

// Config drives a single Run. Client and Request are required (Request.Messages
// must be non-empty). Tools is required only when the effective ToolChoice
// requires at least one tool — an empty Tools slice with the default
// ToolChoiceAuto is allowed and is equivalent to a single Complete call.
//
// MaxIterations bounds the number of tool-calling turns executed. Zero
// uses the default of 8; a negative value (NoIterationLimit) means no
// limit (ADR-0019).
//
// Defaults: a zero ToolChoice
// (in both Request and Config) defaults to llms.ToolChoice{Type:
// llms.ToolChoiceAuto}.
//
// Fail-closed configuration: see Run for the validation rules — duplicated
// tools across Request.Tools/Config.Tools, duplicated ToolChoice across
// Request/Config, duplicate tool names, nil Execute, unknown
// ToolChoice.Type values, and ToolChoice.RequiresTools with no tools are
// all rejected before any provider call.
//
// OnIteration and OnToolCall are optional observation hooks. They must
// never be required for correctness, but they ARE invoked synchronously on
// the loop's goroutine — a slow callback will block the next provider
// call or tool execution. Applications correlate events by closing over
// their own state in the callback.
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
// appended to the transcript (an empty response, which is not appended,
// still emits one before the loop returns OutcomeEmptyResponse). Index is 0-based across the whole Run. Fields
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

// OutcomeKind discriminates Outcome. The set is closed; new kinds require
// a superseding ADR (OutcomeTerminalTool was added by ADR-0017,
// OutcomeDeadline by ADR-0019, OutcomeEmptyResponse by ADR-0020,
// OutcomeContextExceeded by ADR-0026).
type OutcomeKind string

const (
	// OutcomeFinalAnswer is returned when the model emits a response with
	// no tool calls and some text. Response is that response; Err is nil.
	OutcomeFinalAnswer OutcomeKind = "final_answer"
	// OutcomeEmptyResponse is returned when the model's response has no
	// tool calls and no text (ADR-0020), typically a reasoning model that
	// spent its whole output budget on reasoning. Response is that
	// response, so StopReason (provider-specific, e.g. "length" or
	// "max_tokens") and Usage say why; its Usage is included in
	// TotalUsage. The empty assistant message is NOT appended, so Messages
	// ends at the last well-formed point and can be continued (for
	// example, by appending a user message asking the model to finish).
	// Err is nil.
	OutcomeEmptyResponse OutcomeKind = "empty_response"
	// OutcomeTerminalTool is returned when an executor set
	// ToolResult.Stop. TerminalCall and TerminalResult carry the
	// terminating call and its result; Response is the assistant response
	// that contained it. Messages ends with the tool-result message for
	// that turn (including synthesized not-executed results for any calls
	// after the terminal one), so it is well-formed and can be fed back
	// into Complete. Err is nil.
	OutcomeTerminalTool OutcomeKind = "terminal_tool"
	// OutcomeMaxIterations is returned when the loop has executed the
	// configured number of tool-calling responses without a terminal
	// call. The limit-hitting turn's tool calls ARE executed (so a
	// terminal call on that turn is honored, ADR-0017) and their results
	// are appended, so Messages is well-formed; the loop simply makes no
	// further provider call. Err is nil.
	OutcomeMaxIterations OutcomeKind = "max_iterations"
	// OutcomeLLMError is returned when ChatClient.Complete returned a
	// non-cancellation error. Err is that error; Response is the last
	// successful provider response, if any.
	OutcomeLLMError OutcomeKind = "llm_error"
	// OutcomeContextExceeded is returned when ChatClient.Complete failed
	// with a *llms.ProviderError of kind llms.ErrorKindContextLength
	// (ADR-0026): the transcript no longer fits the model's context
	// window. Messages is the transcript that was sent, unchanged and
	// well-formed, so the caller can shorten it (strip reasoning with
	// llms.WithoutReasoning, then trim or compact the history) and run
	// again with it as Request.Messages. Err is the provider error;
	// Response is the last successful provider response, if any. Overflow
	// a provider does not signal deterministically stays OutcomeLLMError.
	OutcomeContextExceeded OutcomeKind = "context_exceeded"
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
	// OutcomeDeadline is returned when the loop's own context reached its
	// deadline (ctx.Err() is context.DeadlineExceeded) during the provider
	// call or during tool execution (ADR-0019). It means "ran out of
	// time", not "something broke". errors.Is(err,
	// context.DeadlineExceeded) holds. A per-attempt deadline that leaves
	// the loop's context live, such as the Timeout middleware, is not the
	// loop's deadline and stays OutcomeLLMError.
	OutcomeDeadline OutcomeKind = "deadline"
)

// Outcome is the result of a single Run. It is always returned (no
// separate error) so callers can switch on Kind and still inspect the
// transcript on non-success outcomes.
//
// Messages is the full transcript at the moment the loop stopped,
// including the initial Request.Messages, every non-empty assistant
// response received, and every tool result message the loop appended.
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
// TerminalCall and TerminalResult are set only for OutcomeTerminalTool and
// are zero otherwise.
//
// Err is set by Kind:
//   - OutcomeFinalAnswer:     nil
//   - OutcomeEmptyResponse:   nil
//   - OutcomeTerminalTool:    nil
//   - OutcomeMaxIterations:   nil
//   - OutcomeLLMError:        the error returned by ChatClient.Complete
//   - OutcomeContextExceeded: the error returned by ChatClient.Complete, a *llms.ProviderError of Kind llms.ErrorKindContextLength
//   - OutcomeToolError:       the configuration validation error or Execute error
//   - OutcomeCanceled:        a cancellation error; use errors.Is(err, context.Canceled)
//   - OutcomeDeadline:        the error that surfaced; errors.Is(err, context.DeadlineExceeded) holds
type Outcome struct {
	Response       llms.ChatResponse
	Err            error
	TerminalCall   llms.ToolCall
	TotalUsage     llms.Usage
	TerminalResult ToolResult
	Kind           OutcomeKind
	Messages       []llms.Message
	Iterations     int
}

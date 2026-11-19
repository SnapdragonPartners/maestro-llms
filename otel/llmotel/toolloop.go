package llmotel

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/toolloop"
)

// Tools returns a copy of tools whose Execute runs inside an INTERNAL span
// named "execute_tool {tool}". The definitions are unchanged, and a tool with
// a nil Execute is copied as-is. The tool loop's own get_toolbox (ADR-0047)
// is not one of the app's tools, so it gets no span.
//
// An Execute error marks the span failed with error.type "_OTHER". A result
// with IsError marks it failed with error.type "tool_error": the loop treats
// it as a normal, model-visible result, but it is still a tool failure a trace
// view should show. Arguments and results are never exported.
func (t *Telemetry) Tools(tools []toolloop.Tool) []toolloop.Tool {
	if tools == nil {
		return nil
	}
	out := make([]toolloop.Tool, len(tools))
	for i := range tools {
		out[i] = tools[i]
		if tools[i].Execute == nil {
			continue
		}
		exec, name := tools[i].Execute, tools[i].Definition.Name
		out[i].Execute = func(ctx context.Context, call llms.ToolCall) (toolloop.ToolResult, error) {
			attrs := []attribute.KeyValue{
				semconv.GenAIOperationNameExecuteTool,
				semconv.GenAIToolName(name),
				semconv.GenAIToolType("function"),
			}
			if call.ID != "" {
				attrs = append(attrs, semconv.GenAIToolCallID(call.ID))
			}
			ctx, span := t.tracer.Start(ctx, spanName("execute_tool", name),
				trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(attrs...))
			defer endSpan(span)
			res, err := exec(ctx, call)
			switch {
			case err != nil:
				setFailed(span, ErrorTypeOther)
			case res.IsError:
				setFailed(span, ErrorTypeToolError)
			}
			return res, err
		}
	}
	return out
}

// RunOptions configures Telemetry.Run.
type RunOptions struct {
	// AgentName, when set, is gen_ai.agent.name and the span name suffix.
	AgentName string
}

// Run calls toolloop.Run inside an INTERNAL span named "invoke_agent" (or
// "invoke_agent {AgentName}"), so each turn's chat spans and each tool span
// nest under it. It does not wrap cfg.Client or cfg.Tools: do that with
// Chat/ChatCall and Tools.
//
// On return the span carries the outcome kind, Outcome.Iterations and the
// run's total input and output tokens (when > 0), compaction included. OutcomeFinalAnswer and
// OutcomeTerminalTool leave the status unset; every other kind, including
// kinds added later, marks the span failed with error.type = the kind.
func (t *Telemetry) Run(ctx context.Context, cfg toolloop.Config, ro RunOptions) toolloop.Outcome {
	attrs := []attribute.KeyValue{semconv.GenAIOperationNameInvokeAgent}
	if ro.AgentName != "" {
		attrs = append(attrs, semconv.GenAIAgentName(ro.AgentName))
	}
	if cfg.Client != nil {
		ref := cfg.Client.Model()
		if name := t.providerName(ref); name != "" {
			attrs = append(attrs, semconv.GenAIProviderNameKey.String(name))
		}
		if ref.Name != "" {
			attrs = append(attrs, semconv.GenAIRequestModel(ref.Name))
		}
	}
	ctx, span := t.tracer.Start(ctx, spanName("invoke_agent", ro.AgentName),
		trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(attrs...))
	defer endSpan(span)

	out := toolloop.Run(ctx, cfg)

	end := []attribute.KeyValue{
		AttrToolloopOutcome.String(string(out.Kind)),
		AttrToolloopIterations.Int(out.Iterations),
	}
	// The whole run is TotalUsage plus CompactionUsage (ADR-0030).
	end = positive(end, semconv.GenAIUsageInputTokensKey, out.TotalUsage.InputTokens+out.CompactionUsage.InputTokens)
	end = positive(end, semconv.GenAIUsageOutputTokensKey, out.TotalUsage.BillableOutputTokens+out.CompactionUsage.BillableOutputTokens)
	span.SetAttributes(end...)
	switch out.Kind {
	case toolloop.OutcomeFinalAnswer, toolloop.OutcomeTerminalTool:
	default:
		setFailed(span, string(out.Kind))
	}
	return out
}

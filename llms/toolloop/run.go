package toolloop

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// defaultMaxIterations is the runaway bound applied when
// Config.MaxIterations is unset (<= 0). It is not a tuning recommendation
// — applications with known workflows should set their own bound from
// their domain. Negative values are treated identically to zero (use the
// default) rather than rejected, since negatives are functionally a
// "leave at default" mistake, not an attack surface.
const defaultMaxIterations = 8

// ConfigError is the error type Run puts in Outcome.Err when it rejects a
// Config. It is a distinct type so callers can errors.As to distinguish
// "your loop config is wrong" from "an executor failed". Like
// *middleware.ValidationError it is non-retryable: a malformed config
// fails identically on every attempt.
type ConfigError struct {
	Reason string
}

func (e *ConfigError) Error() string { return "toolloop: " + e.Reason }

// Run performs the synchronous tool loop described in
// docs/toolloop-proposal.md / ADR-0011. It always returns an Outcome, even
// on validation or provider failure, so callers can inspect the partial
// transcript without unwrapping a second error.
func Run(ctx context.Context, cfg Config) Outcome {
	// Step 1: validate config. Failure returns OutcomeToolError with no
	// provider work done.
	tools, choice, err := validateConfig(cfg)
	if err != nil {
		return Outcome{Kind: OutcomeToolError, Err: err}
	}
	toolByName := indexTools(cfg.Tools)
	maxIter := cfg.MaxIterations
	if maxIter <= 0 {
		maxIter = defaultMaxIterations
	}

	// Step 2: copy the initial transcript so we never mutate caller state.
	// Allocate with zero length + capacity headroom so the first appends
	// don't immediately re-grow.
	transcript := make([]llms.Message, 0, len(cfg.Request.Messages)+2*maxIter)
	transcript = append(transcript, cfg.Request.Messages...)

	var (
		usage         llms.Usage
		usageContribs int               // number of provider responses whose Usage we summed
		lastUsageID   string            // ProviderRequestID of the single contributor (or "" once >1)
		lastResponse  llms.ChatResponse // most-recent successful response, for non-final Outcome.Response
		iterIdx       int               // 0-based index of the current iteration; equals Outcome.Iterations on tool-calling stops
	)

	for {
		// Step 3: build the per-attempt ChatRequest from the base request,
		// current transcript, derived tool definitions, and effective
		// ToolChoice. Other request fields (System, Purpose, MaxTokens,
		// Temperature, Metadata) flow through unchanged.
		req := cfg.Request
		req.Messages = transcript
		req.Tools = tools
		req.ToolChoice = choice

		// Step 4: call the provider.
		resp, callErr := cfg.Client.Complete(ctx, req)

		// Step 5: handle provider error before touching the transcript.
		if callErr != nil {
			if isCanceled(ctx, callErr) {
				return Outcome{
					Kind:       OutcomeCanceled,
					Response:   lastResponse,
					Messages:   transcript,
					TotalUsage: usage,
					Iterations: iterIdx,
					Err:        callErr,
				}
			}
			return Outcome{
				Kind:       OutcomeLLMError,
				Response:   lastResponse,
				Messages:   transcript,
				TotalUsage: usage,
				Iterations: iterIdx,
				Err:        callErr,
			}
		}

		// Step 6: accumulate Usage. ProviderRequestID is collapsed to ""
		// once more than one provider call has contributed.
		addUsage(&usage, &usageContribs, &lastUsageID, resp.Usage)

		// Step 7: append the response message verbatim so any
		// provider-owned metadata (e.g. ToolCall.ProviderSignature for
		// Gemini 3) survives into the next request.
		transcript = append(transcript, resp.Message)
		lastResponse = resp

		// Step 8: emit OnIteration after the transcript is appended.
		if cfg.OnIteration != nil {
			cfg.OnIteration(IterationEvent{
				Index:        iterIdx,
				Response:     resp,
				NumToolCalls: len(resp.ToolCalls),
			})
		}

		// Step 9: no tool calls -> final answer.
		if len(resp.ToolCalls) == 0 {
			return Outcome{
				Kind:       OutcomeFinalAnswer,
				Response:   resp,
				Messages:   transcript,
				TotalUsage: usage,
				Iterations: iterIdx,
			}
		}

		// Step 10: this response counts as a tool-calling iteration. We
		// increment BEFORE the limit check so Iterations on a
		// MaxIterations outcome reflects "this many tool-calling responses
		// observed", including the unresolved one we just appended.
		iterIdx++

		// Step 11: pre-execute limit stop. The limit-hitting assistant
		// message is already in transcript (step 7); its tool calls are
		// deliberately NOT executed. The returned transcript ends with an
		// unresolved tool-call message and is diagnostic state, not a
		// transcript callers can feed directly back into Complete.
		if iterIdx >= maxIter {
			return Outcome{
				Kind:       OutcomeMaxIterations,
				Response:   resp,
				Messages:   transcript,
				TotalUsage: usage,
				Iterations: iterIdx,
			}
		}

		// Steps 12-14: execute every tool call in this assistant turn,
		// in order. Results are appended in the same order as resp.ToolCalls
		// to keep the pairing deterministic for validation middleware.
		results, execOutcome := executeCalls(ctx, &cfg, resp, iterIdx, toolByName, transcript, usage)
		if execOutcome != nil {
			return *execOutcome
		}

		// Step 15: append exactly one tool-result message containing one
		// result per tool call in the assistant response.
		transcript = append(transcript, llms.ToolResultMessage(results...))

		// Step 16: loop.
	}
}

// executeCalls runs the inner for-loop over a single assistant turn's tool
// calls. It returns (results, nil) on success — caller appends the
// ToolResultMessage and continues — or (_, &outcome) to short-circuit the
// loop with Canceled or ToolError. iterIdx is the post-increment iteration
// index (1-based after step 10), so iterIdx-1 is the IterationEvent.Index
// of the assistant turn whose calls we are executing.
func executeCalls(
	ctx context.Context,
	cfg *Config,
	resp llms.ChatResponse,
	iterIdx int,
	toolByName map[string]Tool,
	transcript []llms.Message,
	usage llms.Usage,
) ([]llms.ToolResult, *Outcome) {
	results := make([]llms.ToolResult, len(resp.ToolCalls))
	for i := range resp.ToolCalls {
		// Defensive copy: resp.ToolCalls[i] is a shallow value, but
		// Parameters and ProviderSignature are slices whose backing
		// arrays are also reachable through resp.Message.Content (which
		// we already appended verbatim to the transcript at step 7).
		// Handing the shallow value to an executor or observer that
		// mutates these bytes would corrupt the transcript we'll send
		// back to the provider on the next Complete and would
		// undermine the ADR-0010 ProviderSignature round-trip
		// guarantee, so we clone before dispatch and event emission.
		call := cloneToolCall(resp.ToolCalls[i])
		start := time.Now()
		result, execErr := dispatch(ctx, call, toolByName)
		latency := time.Since(start)

		if cfg.OnToolCall != nil {
			cfg.OnToolCall(ToolCallEvent{
				Iteration: iterIdx - 1,
				Call:      call,
				Result:    result,
				Latency:   latency,
				Err:       execErr,
			})
		}

		if execErr != nil {
			out := Outcome{
				Response:   resp,
				Messages:   transcript,
				TotalUsage: usage,
				Iterations: iterIdx,
				Err:        execErr,
			}
			if isCanceled(ctx, execErr) {
				out.Kind = OutcomeCanceled
			} else {
				out.Kind = OutcomeToolError
			}
			return nil, &out
		}

		results[i] = llms.ToolResult{
			ToolCallID: call.ID,
			Content:    result.Content,
			IsError:    result.IsError,
		}
	}
	return results, nil
}

// dispatch routes a single call to its registered tool, or synthesizes an
// unknown-tool recovery result. Unknown-tool recovery is the documented
// default: the loop appends an IsError tool result so the model can
// self-correct when the provider cannot enforce tool_choice. The OnToolCall
// event for an unknown tool has Err == nil because the loop continued
// successfully.
func dispatch(ctx context.Context, call llms.ToolCall, toolByName map[string]Tool) (ToolResult, error) {
	tool, ok := toolByName[call.Name]
	if !ok {
		return ToolResult{
			Content: fmt.Sprintf("tool %q is not available", call.Name),
			IsError: true,
		}, nil
	}
	return tool.Execute(ctx, call)
}

// cloneToolCall returns a value-copy of c with independent backing arrays
// for the slice-typed fields (Parameters, ProviderSignature) so the
// transcript we already appended verbatim at step 7 cannot be mutated
// through the call we hand to executors and event observers.
func cloneToolCall(c llms.ToolCall) llms.ToolCall {
	return llms.ToolCall{
		ID:                c.ID,
		Name:              c.Name,
		Parameters:        slices.Clone(c.Parameters),
		ProviderSignature: slices.Clone(c.ProviderSignature),
	}
}

// isCanceled reports whether the loop should treat err as
// caller-cancellation. We accept either errors.Is(err, context.Canceled) —
// matching the toolkit's X5 contract that caller cancel is not converted
// into a *llms.ProviderError — or a non-nil ctx.Err() of context.Canceled,
// which catches the case where a provider returned a non-cancellation
// error while the caller's context was simultaneously canceled.
//
// context.DeadlineExceeded is deliberately NOT classified as canceled:
// providers wrap timeouts as a retryable *ProviderError, and executors that
// return DeadlineExceeded are returning a loop-visible error of their own
// choosing — both should surface as OutcomeLLMError / OutcomeToolError so
// the cause is not silently relabeled as "canceled".
func isCanceled(ctx context.Context, err error) bool {
	if errors.Is(err, context.Canceled) {
		return true
	}
	return errors.Is(ctx.Err(), context.Canceled)
}

// addUsage sums numeric token fields and tracks the single-contributor
// ProviderRequestID. Once more than one provider response has contributed,
// the aggregate ProviderRequestID is cleared, since it cannot meaningfully
// represent multiple requests.
func addUsage(total *llms.Usage, contribs *int, lastID *string, add llms.Usage) {
	total.InputTokens += add.InputTokens
	total.OutputTokens += add.OutputTokens
	total.TotalTokens += add.TotalTokens
	total.EmbeddingTokens += add.EmbeddingTokens
	total.CacheReadTokens += add.CacheReadTokens
	total.CacheWriteTokens += add.CacheWriteTokens

	*contribs++
	switch *contribs {
	case 1:
		*lastID = add.ProviderRequestID
		total.ProviderRequestID = add.ProviderRequestID
	default:
		// More than one contributor — collapse to empty.
		*lastID = ""
		total.ProviderRequestID = ""
	}
}

// validateConfig enforces the fail-closed rules from the proposal/ADR and
// returns the effective tool definitions + ToolChoice on success. Errors
// returned here are *ConfigError so callers can errors.As them.
//
//nolint:cyclop // the rules are intentionally enumerated; splitting them obscures the order.
func validateConfig(cfg Config) ([]llms.ToolDefinition, llms.ToolChoice, error) {
	if cfg.Client == nil {
		return nil, llms.ToolChoice{}, &ConfigError{Reason: "Config.Client is required"}
	}
	if len(cfg.Request.Messages) == 0 {
		return nil, llms.ToolChoice{}, &ConfigError{Reason: "Config.Request.Messages must contain at least one message"}
	}
	// Request.Tools must be empty: the loop derives provider tool
	// definitions from Config.Tools so definitions and executors cannot
	// drift. Both-set is the specific failure mode the proposal calls out,
	// but Request.Tools-set-alone is also wrong (no executors).
	if len(cfg.Request.Tools) > 0 {
		return nil, llms.ToolChoice{}, &ConfigError{
			Reason: "Config.Request.Tools must be empty; supply tools via Config.Tools so definitions and executors cannot drift",
		}
	}

	seen := make(map[string]struct{}, len(cfg.Tools))
	for i := range cfg.Tools {
		t := &cfg.Tools[i]
		if t.Definition.Name == "" {
			return nil, llms.ToolChoice{}, &ConfigError{Reason: fmt.Sprintf("Config.Tools[%d].Definition.Name is empty", i)}
		}
		if t.Execute == nil {
			return nil, llms.ToolChoice{}, &ConfigError{Reason: fmt.Sprintf("Config.Tools[%d] (%q): Execute is nil", i, t.Definition.Name)}
		}
		if _, dup := seen[t.Definition.Name]; dup {
			return nil, llms.ToolChoice{}, &ConfigError{Reason: fmt.Sprintf("Config.Tools contains duplicate tool name %q", t.Definition.Name)}
		}
		seen[t.Definition.Name] = struct{}{}
	}

	// ToolChoice precedence: Config.ToolChoice wins when non-zero. Both
	// non-zero is a configuration error (the proposal's fail-closed rule);
	// both zero defaults to Auto.
	reqZero := isZeroToolChoice(cfg.Request.ToolChoice)
	cfgZero := isZeroToolChoice(cfg.ToolChoice)
	if !reqZero && !cfgZero {
		return nil, llms.ToolChoice{}, &ConfigError{
			Reason: "both Config.Request.ToolChoice and Config.ToolChoice are non-zero; set exactly one",
		}
	}
	effective := cfg.ToolChoice
	if cfgZero {
		effective = cfg.Request.ToolChoice
	}
	if isZeroToolChoice(effective) {
		effective = llms.ToolChoice{Type: llms.ToolChoiceAuto}
	}
	// Reject unknown ToolChoice.Type values up front. Several provider
	// adapters silently fall through on an unknown type and drop the
	// caller's intent; failing closed here keeps that misuse out of the
	// transcript and surfaces it as a *ConfigError before any provider
	// call. The empty type is excluded because it has already been
	// normalized to Auto above.
	if !isKnownToolChoiceType(effective.Type) {
		return nil, llms.ToolChoice{}, &ConfigError{
			Reason: fmt.Sprintf("unknown ToolChoice.Type %q; must be one of auto/none/required/tool", effective.Type),
		}
	}
	if effective.RequiresTools() && len(cfg.Tools) == 0 {
		return nil, llms.ToolChoice{}, &ConfigError{
			Reason: fmt.Sprintf("ToolChoice %q requires at least one Config.Tools entry", effective.Type),
		}
	}
	if effective.Type == llms.ToolChoiceTool {
		if _, ok := seen[effective.Name]; !ok {
			return nil, llms.ToolChoice{}, &ConfigError{
				Reason: fmt.Sprintf("ToolChoice names tool %q which is not in Config.Tools", effective.Name),
			}
		}
	}

	tools := make([]llms.ToolDefinition, len(cfg.Tools))
	for i := range cfg.Tools {
		tools[i] = cfg.Tools[i].Definition
	}
	return tools, effective, nil
}

func isZeroToolChoice(tc llms.ToolChoice) bool {
	return tc.Type == "" && tc.Name == ""
}

// isKnownToolChoiceType reports whether t is one of the toolkit's defined
// ToolChoice type constants. Used by validateConfig to fail closed before
// a provider adapter silently drops an unknown value.
func isKnownToolChoiceType(t llms.ToolChoiceType) bool {
	switch t {
	case llms.ToolChoiceAuto, llms.ToolChoiceNone, llms.ToolChoiceRequired, llms.ToolChoiceTool:
		return true
	default:
		return false
	}
}

// indexTools builds the name -> Tool lookup the dispatcher uses. validateConfig
// has already guaranteed unique names and non-nil Execute.
func indexTools(tools []Tool) map[string]Tool {
	out := make(map[string]Tool, len(tools))
	for i := range tools {
		out[tools[i].Definition.Name] = tools[i]
	}
	return out
}

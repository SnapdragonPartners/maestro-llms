package toolloop

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/middleware"
)

// defaultMaxIterations is the runaway bound applied when
// Config.MaxIterations is zero (unset). It is not a tuning recommendation
// — applications with known workflows should set their own bound from
// their domain. Negative values mean no limit (NoIterationLimit, ADR-0019).
const defaultMaxIterations = 8

// NoIterationLimit is the Config.MaxIterations value that runs the loop with no
// iteration ceiling (ADR-0019). Any negative value has the same effect;
// the named constant makes the choice visibly deliberate. The loop then
// stops only on a final answer, a terminal tool, an error, or the
// context (OutcomeCanceled / OutcomeDeadline). No context deadline is
// required: a daemon agent whose tool blocks waiting for events is a
// legitimate unbounded run. Its transcript grows with every turn, so an
// endless Run eventually reaches the model's context limit.
const NoIterationLimit = -1

// preallocIterations caps how many iterations' worth of transcript (one
// assistant message plus one tool-result message each) Run preallocates.
const preallocIterations = 16

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
// docs/toolloop-proposal.md / ADR-0011, with terminal tools and the
// executed limit turn from ADR-0017. It always returns an Outcome, even
// on validation or provider failure, so callers can inspect the partial
// transcript without unwrapping a second error. With Config.Compact set,
// the transcript may be compacted before a turn or after an overflow
// (ADR-0030).
func Run(ctx context.Context, cfg Config) (out Outcome) {
	// Step 1: validate config. Failure returns OutcomeToolError with no
	// provider work done.
	tools, choice, err := validateConfig(cfg)
	if err != nil {
		return Outcome{Kind: OutcomeToolError, Err: err}
	}
	toolByName := indexTools(cfg.Tools)
	maxIter := cfg.MaxIterations
	if maxIter == 0 {
		maxIter = defaultMaxIterations
	}
	limited := maxIter > 0

	// Step 2: copy the initial transcript so we never mutate caller state.
	// Allocate with zero length + capacity headroom so the first appends
	// don't immediately re-grow. Headroom is capped independently of
	// maxIter: callers bounding a run by context deadline pass a very large
	// MaxIterations, and sizing by it allocated billions of messages (#73).
	// An unlimited run uses the cap. append grows the slice past the cap;
	// that cost is negligible next to a model call.
	prealloc := preallocIterations
	if limited {
		prealloc = min(maxIter, preallocIterations)
	}
	transcript := make([]llms.Message, 0, len(cfg.Request.Messages)+2*prealloc)
	transcript = append(transcript, cfg.Request.Messages...)

	var (
		usage        usageSum          // loop model's calls -> TotalUsage
		lastResponse llms.ChatResponse // most-recent successful response, for non-final Outcome.Response
		iterIdx      int               // 0-based index of the current iteration; equals Outcome.Iterations on tool-calling stops
	)
	cp := &compaction{fn: cfg.Compact, headroom: 2 * prealloc}
	defer func() {
		out.CompactionUsage = cp.usage.total
		out.Compactions = cp.count
	}()

	for {
		// Step 3: build the per-attempt ChatRequest from the base request,
		// current transcript, derived tool definitions, and effective
		// ToolChoice. Other request fields (System, Purpose, MaxTokens,
		// Temperature, Metadata) flow through unchanged.
		req := cfg.Request
		req.Messages = transcript
		req.Tools = tools
		req.ToolChoice = choice

		// Step 4: call the provider. With a Compactor, the transcript may be
		// compacted before the call or after an overflow (ADR-0030);
		// transcript becomes whatever was last sent.
		resp, sent, kind, callErr := cp.send(ctx, cfg.Client, req, lastResponse.Usage)
		transcript = sent

		// Step 5: handle a stop before touching the transcript.
		if callErr != nil {
			return Outcome{
				Kind:       kind,
				Response:   lastResponse,
				Messages:   transcript,
				TotalUsage: usage.total,
				Iterations: iterIdx,
				Err:        callErr,
			}
		}

		// Step 6: accumulate Usage. ProviderRequestID is collapsed to ""
		// once more than one provider call has contributed.
		addUsage(&usage, resp.Usage)

		// Step 7: append the response message verbatim so any
		// provider-owned metadata (e.g. ToolCall.ProviderSignature for
		// Gemini 3) survives into the next request. An empty response
		// (ADR-0020) is not appended: an assistant message with no
		// content cannot be sent back, and Messages must stay sendable.
		empty := isEmptyResponse(&resp)
		if !empty {
			transcript = append(transcript, resp.Message)
		}
		lastResponse = resp

		// Step 8: emit OnIteration after the transcript is appended.
		if cfg.OnIteration != nil {
			cfg.OnIteration(IterationEvent{
				Index:        iterIdx,
				Response:     resp,
				NumToolCalls: len(resp.ToolCalls),
			})
		}

		// Step 8a: empty response -> OutcomeEmptyResponse. Typically a
		// reasoning model that spent its whole output budget on
		// reasoning; Response.StopReason says why.
		if empty {
			return Outcome{
				Kind:       OutcomeEmptyResponse,
				Response:   resp,
				Messages:   transcript,
				TotalUsage: usage.total,
				Iterations: iterIdx,
			}
		}

		// Step 9: no tool calls -> final answer.
		if len(resp.ToolCalls) == 0 {
			return Outcome{
				Kind:       OutcomeFinalAnswer,
				Response:   resp,
				Messages:   transcript,
				TotalUsage: usage.total,
				Iterations: iterIdx,
			}
		}

		// Step 10: this response counts as a tool-calling iteration.
		iterIdx++

		// Steps 11-13: execute this assistant turn's tool calls in order.
		// Results are appended in the same order as resp.ToolCalls to keep
		// the pairing deterministic for validation middleware. A Stop
		// result ends the turn like a return statement (ADR-0017); any
		// later calls get synthesized not-executed results.
		results, terminal, execOutcome := executeCalls(ctx, &cfg, resp, iterIdx, toolByName, transcript, usage.total)
		if execOutcome != nil {
			return *execOutcome
		}

		// Step 14: append exactly one tool-result message containing one
		// result per tool call in the assistant response.
		transcript = append(transcript, llms.ToolResultMessage(results...))

		// Step 15: terminal stop. Checked before the limit so a terminal
		// call on the limit-hitting turn is honored (ADR-0017).
		if terminal != nil {
			return Outcome{
				Kind:           OutcomeTerminalTool,
				Response:       resp,
				Messages:       transcript,
				TotalUsage:     usage.total,
				Iterations:     iterIdx,
				TerminalCall:   terminal.call,
				TerminalResult: terminal.result,
			}
		}

		// Step 16: limit stop. The limit-hitting turn's tools have run and
		// their results are appended, so the transcript is well-formed;
		// we just make no further provider call.
		if limited && iterIdx >= maxIter {
			return Outcome{
				Kind:       OutcomeMaxIterations,
				Response:   resp,
				Messages:   transcript,
				TotalUsage: usage.total,
				Iterations: iterIdx,
			}
		}

		// Step 17: loop.
	}
}

// isEmptyResponse reports whether resp carries nothing the model said: no
// tool calls and no non-empty text. Reasoning parts don't count (ADR-0021):
// a response that spent its whole output budget on reasoning is empty on
// every provider, whether or not the adapter returns the reasoning. An empty
// text part counts as empty because providers reject empty text blocks too.
func isEmptyResponse(resp *llms.ChatResponse) bool {
	if len(resp.ToolCalls) > 0 {
		return false
	}
	for i := range resp.Message.Content {
		p := &resp.Message.Content[i]
		switch {
		case p.Type == llms.ContentReasoning:
		case p.Type != llms.ContentText || p.Text != "":
			return false
		}
	}
	return true
}

// terminalStop records the call whose executor set ToolResult.Stop.
type terminalStop struct {
	call   llms.ToolCall
	result ToolResult
}

// executeCalls runs the inner for-loop over a single assistant turn's tool
// calls. It returns (results, terminal, nil) on success — caller appends
// the ToolResultMessage, then stops if terminal is non-nil or continues
// otherwise — or (_, _, &outcome) to short-circuit the loop with Canceled
// or ToolError. iterIdx is the post-increment iteration index (1-based
// after step 10), so iterIdx-1 is the IterationEvent.Index of the
// assistant turn whose calls we are executing.
func executeCalls(
	ctx context.Context,
	cfg *Config,
	resp llms.ChatResponse,
	iterIdx int,
	toolByName map[string]Tool,
	transcript []llms.Message,
	usage llms.Usage,
) ([]llms.ToolResult, *terminalStop, *Outcome) {
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

		if execErr == nil && result.Stop && result.IsError {
			execErr = fmt.Errorf("toolloop: tool %q (call %q) returned ToolResult with both Stop and IsError set", call.Name, call.ID)
		}
		if execErr != nil {
			kind, err := classifyStop(ctx, execErr, OutcomeToolError)
			return nil, nil, &Outcome{
				Kind:       kind,
				Response:   resp,
				Messages:   transcript,
				TotalUsage: usage,
				Iterations: iterIdx,
				Err:        err,
			}
		}

		results[i] = llms.ToolResult{
			ToolCallID: call.ID,
			Content:    result.Content,
			IsError:    result.IsError,
		}

		if result.Stop {
			// Return-statement semantics: the remaining calls in this
			// turn are not executed. Each still needs a matching result
			// for the transcript to be well-formed, so synthesize a
			// model-visible not-executed error. No OnToolCall event is
			// emitted for these — they were never attempted.
			for j := i + 1; j < len(resp.ToolCalls); j++ {
				skipped := &resp.ToolCalls[j]
				results[j] = llms.ToolResult{
					ToolCallID: skipped.ID,
					Content:    fmt.Sprintf("not executed: tool loop ended by %q", call.Name),
					IsError:    true,
				}
			}
			// Fresh clone for the Outcome: call's buffers were handed to
			// Execute and OnToolCall, which may have mutated them, and
			// TerminalCall.Parameters is the structured output callers
			// decode. It must match the transcript's original submission.
			return results, &terminalStop{call: cloneToolCall(resp.ToolCalls[i]), result: result}, nil
		}
	}
	return results, nil, nil
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

// classifyStop maps a provider or executor error to the Outcome kind and
// Err the loop returns. It is the single choke point for context-driven
// stops (ADR-0019), applied identically to provider and executor errors.
//
// The loop's own context is authoritative: when ctx is done, the outcome
// follows ctx.Err() regardless of what the call returned —
// context.Canceled gives OutcomeCanceled and context.DeadlineExceeded gives
// OutcomeDeadline. Err is then guaranteed to match ctx.Err() under
// errors.Is: the call's error is returned as-is when it already does, and
// otherwise wrapped together with ctx.Err() so neither cause is lost.
//
// When ctx is still live, an error matching context.Canceled is still
// OutcomeCanceled (cross-cutting X5: caller cancel is not converted into
// a *llms.ProviderError, and may come from a context the call derived).
// A context.DeadlineExceeded with a live loop context is NOT the loop's
// deadline — typically the per-attempt Timeout middleware, which providers
// classify as a retryable timeout *llms.ProviderError, or an executor's own
// sub-deadline — so it stays fallback (OutcomeLLMError / OutcomeToolError).
func classifyStop(ctx context.Context, err error, fallback OutcomeKind) (OutcomeKind, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		kind := OutcomeCanceled
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			kind = OutcomeDeadline
		}
		if !errors.Is(err, ctxErr) {
			err = fmt.Errorf("toolloop: %w: %w", ctxErr, err)
		}
		return kind, err
	}
	if errors.Is(err, context.Canceled) {
		return OutcomeCanceled, err
	}
	return fallback, err
}

// llmErrorKind is the classifyStop fallback for a provider error: a
// context-window overflow (ADR-0026) is OutcomeContextExceeded, anything
// else OutcomeLLMError. It is only the fallback, so a done loop context
// still wins (OutcomeCanceled / OutcomeDeadline). Executor errors never use
// it: a tool that itself overflows some other model's context is a tool
// failure, not this loop's transcript overflowing.
func llmErrorKind(err error) OutcomeKind {
	var pe *llms.ProviderError
	if errors.As(err, &pe) && pe.Kind == llms.ErrorKindContextLength {
		return OutcomeContextExceeded
	}
	return OutcomeLLMError
}

// compaction is Run's compaction state (ADR-0030). With a nil fn, send is a
// plain Complete call.
type compaction struct {
	fn       Compactor
	usage    usageSum // Compactor-reported usage -> Outcome.CompactionUsage
	headroom int      // capacity added to a compacted copy for the loop's appends
	count    int      // compacted transcripts adopted -> Outcome.Compactions
}

// send makes one provider call for the loop: the before-turn compaction, the
// call, and on an overflow one compaction and retry. It returns the response
// and the transcript last sent, or that transcript with the stop kind and
// error. A failed compaction returns the transcript before it.
func (c *compaction) send(ctx context.Context, client llms.ChatClient, req llms.ChatRequest, last llms.Usage) (llms.ChatResponse, []llms.Message, OutcomeKind, error) {
	if c.fn != nil {
		msgs, cerr := c.run(ctx, CompactInput{Reason: CompactBeforeTurn, Messages: req.Messages, LastUsage: last})
		if cerr != nil {
			kind, err := classifyStop(ctx, cerr, OutcomeCompactionError)
			return llms.ChatResponse{}, req.Messages, kind, err
		}
		if msgs != nil {
			req.Messages = msgs
		}
	}
	for retried := false; ; retried = true {
		resp, callErr := client.Complete(ctx, req)
		if callErr == nil {
			return resp, req.Messages, "", nil
		}
		kind, err := classifyStop(ctx, callErr, llmErrorKind(callErr))
		if kind != OutcomeContextExceeded || c.fn == nil || retried {
			return llms.ChatResponse{}, req.Messages, kind, err
		}
		msgs, cerr := c.run(ctx, CompactInput{Reason: CompactOverflow, Messages: req.Messages, LastUsage: last, Err: err})
		if cerr != nil {
			kind, err = classifyStop(ctx, cerr, OutcomeCompactionError)
			return llms.ChatResponse{}, req.Messages, kind, err
		}
		if msgs == nil {
			// Classify again: the loop's context may have ended while the
			// hook ran, and it stays authoritative (ADR-0019).
			kind, err = classifyStop(ctx, callErr, llmErrorKind(callErr))
			return llms.ChatResponse{}, req.Messages, kind, err
		}
		req.Messages = msgs
	}
}

// run calls the Compactor and checks its result. It returns nil, nil for "no
// change", a private copy of the compacted transcript with headroom for the
// loop's appends, or a *CompactionError. Usage the hook reports is summed
// even when it also fails, since the tokens were spent.
func (c *compaction) run(ctx context.Context, in CompactInput) ([]llms.Message, error) {
	res, err := c.fn(ctx, in)
	if res.Usage != (llms.Usage{}) {
		addUsage(&c.usage, res.Usage)
	}
	if err != nil {
		return nil, &CompactionError{Reason: in.Reason, Err: err}
	}
	if res.Messages == nil {
		return nil, nil
	}
	if err := middleware.ValidateMessages(res.Messages); err != nil {
		return nil, &CompactionError{Reason: in.Reason, Err: err}
	}
	if res.Messages[len(res.Messages)-1].Role == llms.RoleAssistant {
		return nil, &CompactionError{Reason: in.Reason, Err: errors.New("compacted transcript ends with an assistant message; the next call continues from it")}
	}
	out := make([]llms.Message, len(res.Messages), len(res.Messages)+c.headroom)
	copy(out, res.Messages)
	c.count++
	return out, nil
}

// usageSum is a running Usage total plus the number of responses in it.
type usageSum struct {
	total    llms.Usage
	contribs int
}

// addUsage sums every numeric token field (TestRun_TotalUsageSumsEveryIntField
// fails if a new llms.Usage int field is added here without being summed) and
// tracks the single-contributor
// ProviderRequestID. Once more than one provider response has contributed,
// the aggregate ProviderRequestID is cleared, since it cannot meaningfully
// represent multiple requests.
func addUsage(sum *usageSum, add llms.Usage) {
	total := &sum.total
	total.InputTokens += add.InputTokens
	total.OutputTokens += add.OutputTokens
	total.TotalTokens += add.TotalTokens
	total.ReasoningTokens += add.ReasoningTokens
	total.BillableOutputTokens += add.BillableOutputTokens
	total.EmbeddingTokens += add.EmbeddingTokens
	total.CacheReadTokens += add.CacheReadTokens
	total.CacheWriteTokens += add.CacheWriteTokens

	sum.contribs++
	if sum.contribs == 1 {
		total.ProviderRequestID = add.ProviderRequestID
	} else {
		// More than one contributor — collapse to empty.
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

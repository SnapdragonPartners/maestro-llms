package middleware

import (
	"encoding/base64"
	"encoding/json"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/internal/media"
	"github.com/SnapdragonPartners/maestro-llms/llms/internal/satint"
	"github.com/SnapdragonPartners/maestro-llms/llms/ratelimit"
)

// InputBounder is an optional capability a TokenEstimator may also implement
// (ADR-0043). RateLimitChat and RateLimitEmbeddings discover it with a type
// assertion; TokenEstimator is frozen (README "Stability contract (v1)").
//
// A bound is an upper bound on a request's usage, not an estimate: a hard
// budget can admit on it and never be overshot. Where no bound can be
// established, a method returns ok false, and the reservation carries no
// bound (ratelimit.ReservationRequest.BoundUnits is nil). An error is
// reserved for a real failure, and the middleware returns it before
// reserving, so the provider is not called.
//
// The model is the wrapped client's, so a bound can depend on the provider.
type InputBounder interface {
	BoundChat(model llms.ModelRef, req llms.ChatRequest) (units ratelimit.UsageUnits, ok bool, err error)
	BoundEmbeddings(model llms.ModelRef, req llms.EmbeddingRequest) (units ratelimit.UsageUnits, ok bool, err error)
}

// Bound tuning (ADR-0043). The text bound rests on tokens <= UTF-8 bytes,
// which holds for byte-level BPE and byte-fallback SentencePiece tokenizers
// (each token covers at least one byte). Providers don't document their
// per-request, per-message or per-part framing, so the margins below are
// generous allowances, checked against billed usage by the live tests.
const (
	// boundPerRequest covers request-level framing (role headers, the
	// assistant priming, a provider's short hidden preamble).
	boundPerRequest = 64
	// boundPerMessage covers each message's role and turn markers.
	boundPerMessage = 16
	// boundPerPart covers each content part's framing: block type, an
	// is_error flag, a tool call's or result's JSON structure.
	boundPerPart = 16
	// boundPerTool covers each tool definition's JSON structure around
	// its name, description and schema.
	boundPerTool = 32
	// anthropicToolOverhead covers the hidden tool-use system prompt
	// Anthropic adds when tools are present. Its pricing page lists it per
	// model; the largest figure listed (2026-11-10) is 804 tokens (Opus
	// 4.7, tool_choice any/tool), and current models list 286. A model
	// documented above this allowance would break the bound, so the live
	// tests check it against billed usage.
	anthropicToolOverhead = 1024
)

// BoundChat implements InputBounder: an upper bound on a chat request's input
// tokens, with MaxTokens as the output bound. There is no bound (ok false)
// when:
//
//   - MaxTokens is unset, since the output cap is then the adapter's or
//     model's default;
//   - the provider is not Anthropic, OpenAI or Google: Ollama and
//     OpenAI-compatible servers apply their own chat templates, which can
//     add any amount of hidden text;
//   - the request has ProviderParams, which can add input this package
//     doesn't see;
//   - any part (including a tool result part) is an image or a document:
//     provider image and page formulas vary by model, so media bounds stay
//     with the application, which can implement InputBounder itself;
//   - the request has tools and the provider is not Anthropic: OpenAI and
//     Google don't document the hidden tokens they add for tools.
//
// Otherwise the input bound is the UTF-8 byte count of every text field sent
// (system, message text, tool call IDs, names, arguments and signatures,
// tool results, reasoning text and opaque provider data, tool definitions),
// plus the framing margins, plus anthropicToolOverhead for an Anthropic
// request with tools. Replayed reasoning is counted by its bytes; the live
// tests check that against billed usage.
//
// Each field is counted at the size the adapters can put on the wire, which
// can exceed its size in the request (the wire-expansion cases):
//
//   - JSON an adapter decodes and re-encodes (Anthropic tool schemas, Gemini
//     tool call arguments) counts at the larger of its raw and re-encoded
//     length, since re-encoding expands exponent-form numbers;
//   - a tool call signature, sent base64-encoded (Gemini's
//     thought_signature), counts at its base64 length;
//   - a tool result counts its function name again, resolved from the
//     matching tool call (Gemini sends it with every result), or its call ID
//     again when there is no match;
//   - on Google, a tool result's text counts at its JSON-encoded length plus
//     the response object around it, since Gemini sends it as a string inside
//     the function response's JSON object; encoding escapes control
//     characters (six bytes each) and, in Go, <, > and &.
//
// The bound covers the request as the adapters send it. A client set up to
// change the body beyond the request (anthropic.WithRequestOptions with a
// body option such as option.WithJSONSet, or an HTTP client or proxy that
// rewrites or adds content) is outside it, like ProviderParams: an app that
// configures one supplies its own InputBounder, or one that returns ok false.
func (DefaultEstimator) BoundChat(model llms.ModelRef, req llms.ChatRequest) (ratelimit.UsageUnits, bool, error) {
	if req.MaxTokens <= 0 || len(req.ProviderParams) > 0 {
		return ratelimit.UsageUnits{}, false, nil
	}
	switch model.Provider {
	case llms.ProviderAnthropic, llms.ProviderOpenAI, llms.ProviderGoogle:
	default:
		return ratelimit.UsageUnits{}, false, nil
	}
	if len(req.Tools) > 0 && model.Provider != llms.ProviderAnthropic {
		return ratelimit.UsageUnits{}, false, nil
	}
	w := wire{names: toolCallNames(req.Messages), jsonResults: model.Provider == llms.ProviderGoogle}
	input, ok := boundParts(req.System, &w)
	if !ok {
		return ratelimit.UsageUnits{}, false, nil
	}
	input = sum(input, boundPerRequest)
	for i := range req.Messages {
		n, ok := boundParts(req.Messages[i].Content, &w)
		if !ok {
			return ratelimit.UsageUnits{}, false, nil
		}
		input = sum(input, boundPerMessage, n)
	}
	if len(req.Tools) > 0 {
		input = sum(input, anthropicToolOverhead, len(req.ToolChoice.Name))
		for i := range req.Tools {
			td := &req.Tools[i]
			input = sum(input, boundPerTool, len(td.Name), len(td.Description), jsonWireLen(td.InputSchema))
		}
	}
	return ratelimit.UsageUnits{
		InputTokens:     input,
		OutputTokensMax: req.MaxTokens,
		TotalTokensMax:  satint.Add(input, req.MaxTokens),
		Requests:        1,
	}, true, nil
}

// BoundEmbeddings implements InputBounder. Embedding requests have no bound
// yet (ok false): the providers' task-type and title framing is
// undocumented, and no consumer budgets embeddings.
func (DefaultEstimator) BoundEmbeddings(llms.ModelRef, llms.EmbeddingRequest) (ratelimit.UsageUnits, bool, error) {
	return ratelimit.UsageUnits{}, false, nil
}

// toolCallNames maps each tool call ID in msgs to its name, as the Gemini
// adapter resolves a tool result's function name.
func toolCallNames(msgs []llms.Message) map[string]string {
	names := map[string]string{}
	for i := range msgs {
		for j := range msgs[i].Content {
			if tc := msgs[i].Content[j].ToolCall; tc != nil && tc.ID != "" {
				names[tc.ID] = tc.Name
			}
		}
	}
	return names
}

// jsonWireLen is the size of raw JSON on the wire after an adapter decodes it
// into generic values and re-encodes it: the larger of the raw and re-encoded
// lengths (re-encoding expands an exponent such as 1e20 into 21 digits).
// Invalid JSON counts at its raw length; the adapter rejects it before the
// call.
func jsonWireLen(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return len(raw)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return len(raw)
	}
	return max(len(raw), len(out))
}

// wire is what boundPart needs to know about how the adapter encodes a
// request.
type wire struct {
	// names resolves tool result names (toolCallNames).
	names map[string]string
	// jsonResults is set when tool result text is sent as a JSON string
	// inside a response object (Gemini's function response).
	jsonResults bool
}

// geminiResultFrame is the Gemini function response object around the
// result text (llms/providers/google functionResponse), with is_error at
// its longer value.
const geminiResultFrame = len(`{"content":,"is_error":false}`)

// jsonStringLen is the length of s encoded as a JSON string, quotes
// included, by encoding/json (which escapes control characters, <, > and &,
// and replaces invalid UTF-8).
func jsonStringLen(s string) int {
	out, err := json.Marshal(s)
	if err != nil { // unreachable for a string; the worst-case escape
		return sum(2, 6*len(s))
	}
	return len(out)
}

// boundParts bounds a list of content parts, or reports false when one has no
// bound (media).
func boundParts(parts []llms.ContentPart, w *wire) (int, bool) {
	total := 0
	for i := range parts {
		n, ok := boundPart(&parts[i], w)
		if !ok {
			return 0, false
		}
		total = sum(total, n)
	}
	return total, true
}

// boundPart bounds one content part. Every byte the adapters can send counts,
// so a field an adapter omits only loosens the bound.
func boundPart(p *llms.ContentPart, w *wire) (int, bool) {
	if media.IsMedia(p.Type) || p.Media != nil {
		return 0, false
	}
	n := sum(boundPerPart, len(p.Text))
	if tc := p.ToolCall; tc != nil {
		n = sum(n, len(tc.ID), len(tc.Name), jsonWireLen(tc.Parameters),
			base64.StdEncoding.EncodedLen(len(tc.ProviderSignature)))
	}
	if tr := p.ToolResult; tr != nil {
		// The name sent again with the result: the matching call's, or
		// the ID itself when the history has no match.
		resent, ok := w.names[tr.ToolCallID]
		if !ok {
			resent = tr.ToolCallID
		}
		n = sum(n, len(tr.ToolCallID), len(resent))
		for i := range tr.Parts {
			rp := &tr.Parts[i]
			if media.IsMedia(rp.Type) || rp.Media != nil {
				return 0, false
			}
			n = sum(n, boundPerPart)
			if !w.jsonResults {
				n = sum(n, len(rp.Text))
			}
		}
		if w.jsonResults {
			// The text the Gemini adapter sends: Content, or the text
			// parts joined.
			text := tr.Content
			if len(tr.Parts) > 0 {
				text = media.JoinText(tr.Parts)
			}
			n = sum(n, geminiResultFrame, jsonStringLen(text))
		} else {
			n = sum(n, len(tr.Content))
		}
	}
	if r := p.Reasoning; r != nil {
		n = sum(n, len(r.Text), len(r.ProviderData))
	}
	return n, true
}

package testllm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// The fakes record requests so tests can assert on them. Callers commonly
// reuse and mutate a request struct between calls (loops, table tests), so
// recordings are deep-copied on the way in and on the way out of Calls();
// otherwise an assertion would observe post-call mutations or race with a
// concurrent caller.

func copyContentParts(in []llms.ContentPart) []llms.ContentPart {
	if in == nil {
		return nil
	}
	out := make([]llms.ContentPart, len(in))
	for i := range in {
		p := in[i]
		if p.ToolCall != nil {
			tc := *p.ToolCall
			tc.Parameters = slices.Clone(p.ToolCall.Parameters)
			tc.ProviderSignature = slices.Clone(p.ToolCall.ProviderSignature)
			p.ToolCall = &tc
		}
		if p.ToolResult != nil {
			tr := *p.ToolResult
			p.ToolResult = &tr
		}
		if p.Reasoning != nil {
			r := *p.Reasoning
			r.ProviderData = slices.Clone(p.Reasoning.ProviderData)
			p.Reasoning = &r
		}
		out[i] = p
	}
	return out
}

func copyChatRequest(r llms.ChatRequest) llms.ChatRequest {
	r.System = copyContentParts(r.System)
	if r.Messages != nil {
		msgs := make([]llms.Message, len(r.Messages))
		for i := range r.Messages {
			msgs[i] = llms.Message{
				Role:    r.Messages[i].Role,
				Content: copyContentParts(r.Messages[i].Content),
			}
		}
		r.Messages = msgs
	}
	if r.Tools != nil {
		tools := make([]llms.ToolDefinition, len(r.Tools))
		for i := range r.Tools {
			tools[i] = r.Tools[i]
			tools[i].InputSchema = slices.Clone(r.Tools[i].InputSchema)
		}
		r.Tools = tools
	}
	if r.Temperature != nil {
		t := *r.Temperature
		r.Temperature = &t
	}
	if r.Reasoning != nil {
		rc := *r.Reasoning
		r.Reasoning = &rc
	}
	r.Metadata = maps.Clone(r.Metadata)
	if r.ProviderParams != nil {
		pp := make(map[llms.ProviderID]map[string]any, len(r.ProviderParams))
		for k, v := range r.ProviderParams {
			pp[k], _ = deepCopyJSON(v).(map[string]any)
		}
		r.ProviderParams = pp
	}
	return r
}

// recordChatRequest is copyChatRequest for the way in. ProviderParams are
// recorded as their JSON form, which is what an adapter sends (ADR-0024):
// objects as map[string]any, arrays as []any, numbers as json.Number. The
// snapshot shares nothing with the caller's values whatever their Go types,
// private state and json:"-" cycles included. A value that doesn't marshal
// is reported as an error, as the adapters reject it.
func recordChatRequest(r llms.ChatRequest) (llms.ChatRequest, error) {
	params := r.ProviderParams
	r.ProviderParams = nil
	out := copyChatRequest(r)
	if params == nil {
		return out, nil
	}
	out.ProviderParams = make(map[llms.ProviderID]map[string]any, len(params))
	var firstErr error
	for k, v := range params {
		snap, err := jsonSnapshot(v)
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("ProviderParams[%q] does not marshal to JSON: %w", k, err)
		}
		out.ProviderParams[k] = snap
	}
	return out, firstErr
}

func jsonSnapshot(v map[string]any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err //nolint:wrapcheck // the caller wraps it with the provider key
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var out map[string]any
	if err := d.Decode(&out); err != nil {
		return nil, err //nolint:wrapcheck // the caller wraps it with the provider key
	}
	return out, nil
}

// deepCopyJSON copies the nested maps and slices of a JSON-shaped value (the
// recorded ProviderParams form); every other JSON value is an immutable
// scalar.
func deepCopyJSON(v any) any {
	switch t := v.(type) {
	case map[string]any:
		if t == nil {
			return nil
		}
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = deepCopyJSON(e)
		}
		return out
	case []any:
		if t == nil {
			return nil
		}
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = deepCopyJSON(e)
		}
		return out
	default:
		return v
	}
}

func copyEmbeddingRequest(r llms.EmbeddingRequest) llms.EmbeddingRequest {
	r.Inputs = slices.Clone(r.Inputs)
	r.Metadata = maps.Clone(r.Metadata)
	return r
}

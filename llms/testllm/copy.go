package testllm

import (
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
			p.ToolCall = &tc
		}
		if p.ToolResult != nil {
			tr := *p.ToolResult
			p.ToolResult = &tr
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
	r.Metadata = maps.Clone(r.Metadata)
	return r
}

func copyEmbeddingRequest(r llms.EmbeddingRequest) llms.EmbeddingRequest {
	r.Inputs = slices.Clone(r.Inputs)
	r.Metadata = maps.Clone(r.Metadata)
	return r
}

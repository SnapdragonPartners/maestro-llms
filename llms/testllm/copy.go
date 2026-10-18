package testllm

import (
	"maps"
	"reflect"
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
			pp[k], _ = deepCopy(v).(map[string]any)
		}
		r.ProviderParams = pp
	}
	return r
}

// deepCopy returns a copy of v that shares no mutable storage with it:
// maps, slices, arrays, pointers and interfaces are copied recursively, with
// their concrete types preserved (a []string stays a []string, a
// json.RawMessage stays a json.RawMessage). Struct values are copied with
// their exported fields deep-copied; unexported fields are copied as-is.
// Funcs and channels, which can't marshal and so can't be valid
// ProviderParams, are copied as-is.
func deepCopy(v any) any {
	if v == nil {
		return nil
	}
	return deepCopyValue(reflect.ValueOf(v)).Interface()
}

func deepCopyValue(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Map:
		return deepCopyMap(v)
	case reflect.Slice, reflect.Array:
		return deepCopySeq(v)
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return v
		}
		if v.Kind() == reflect.Pointer {
			out := reflect.New(v.Type().Elem())
			out.Elem().Set(deepCopyValue(v.Elem()))
			return out
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(deepCopyValue(v.Elem()))
		return out
	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		out.Set(v)
		for i := range v.NumField() {
			if f := out.Field(i); f.CanSet() {
				f.Set(deepCopyValue(v.Field(i)))
			}
		}
		return out
	default:
		return v
	}
}

func deepCopyMap(v reflect.Value) reflect.Value {
	if v.IsNil() {
		return v
	}
	out := reflect.MakeMapWithSize(v.Type(), v.Len())
	iter := v.MapRange()
	for iter.Next() {
		out.SetMapIndex(deepCopyValue(iter.Key()), deepCopyValue(iter.Value()))
	}
	return out
}

// deepCopySeq copies a slice or an array element by element.
func deepCopySeq(v reflect.Value) reflect.Value {
	var out reflect.Value
	if v.Kind() == reflect.Slice {
		if v.IsNil() {
			return v
		}
		out = reflect.MakeSlice(v.Type(), v.Len(), v.Len())
	} else {
		out = reflect.New(v.Type()).Elem()
	}
	for i := range v.Len() {
		out.Index(i).Set(deepCopyValue(v.Index(i)))
	}
	return out
}

func copyEmbeddingRequest(r llms.EmbeddingRequest) llms.EmbeddingRequest {
	r.Inputs = slices.Clone(r.Inputs)
	r.Metadata = maps.Clone(r.Metadata)
	return r
}

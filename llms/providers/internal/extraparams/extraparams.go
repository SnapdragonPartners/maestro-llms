// Package extraparams applies ChatRequest.ProviderParams: raw request-body
// fields an application merges into a provider's wire request (ADR-0024).
// Every adapter uses the same merge, so the semantics are identical across
// providers: objects merge recursively, and any other value (scalar, array,
// null) replaces what is there.
package extraparams

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// For returns p's entry from req.ProviderParams, normalized through a JSON
// round trip so struct values merge as objects and numbers keep their
// precision (json.Number). ok is false when there is no entry for p; err is
// set when the entry does not marshal to JSON.
func For(req *llms.ChatRequest, p llms.ProviderID) (params map[string]any, ok bool, err error) {
	src := req.ProviderParams[p]
	if len(src) == 0 {
		return nil, false, nil
	}
	b, err := json.Marshal(src)
	if err != nil {
		return nil, false, fmt.Errorf("ProviderParams[%q] does not marshal to JSON: %w", p, err)
	}
	var out map[string]any
	if err := decode(b, &out); err != nil {
		return nil, false, fmt.Errorf("ProviderParams[%q]: %w", p, err)
	}
	return out, true, nil
}

// Merge deep-merges src into dst: when both values for a key are objects
// they merge recursively; otherwise src's value replaces dst's.
func Merge(dst, src map[string]any) {
	for k, v := range src {
		if sv, ok := v.(map[string]any); ok {
			if dv, ok := dst[k].(map[string]any); ok {
				Merge(dv, sv)
				continue
			}
		}
		dst[k] = v
	}
}

// MergeJSON deep-merges src into a JSON object body and re-encodes it.
func MergeJSON(body []byte, src map[string]any) ([]byte, error) {
	var dst map[string]any
	if err := decode(body, &dst); err != nil {
		return nil, fmt.Errorf("request body is not a JSON object: %w", err)
	}
	if dst == nil {
		dst = map[string]any{}
	}
	Merge(dst, src)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(dst); err != nil {
		return nil, fmt.Errorf("re-encoding request body: %w", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// Overrides reports whether merging src would change the value at path:
// src sets the path itself, or replaces one of its ancestors with a
// non-object. Adapters use it to drop overridden fields from
// ChatResponse.AppliedReasoning.
func Overrides(src map[string]any, path ...string) bool {
	m := src
	for i, k := range path {
		v, ok := m[k]
		if !ok {
			return false
		}
		if i == len(path)-1 {
			return true
		}
		sub, ok := v.(map[string]any)
		if !ok {
			return true
		}
		m = sub
	}
	return false
}

// Middleware returns an SDK HTTP middleware (the shape openai-go and
// anthropic-sdk-go share) that merges src into each request's JSON body.
func Middleware(src map[string]any) func(*http.Request, func(*http.Request) (*http.Response, error)) (*http.Response, error) {
	return func(r *http.Request, next func(*http.Request) (*http.Response, error)) (*http.Response, error) {
		if r.Body == nil || r.Body == http.NoBody {
			return next(r)
		}
		body, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("reading request body: %w", err)
		}
		merged, err := MergeJSON(body, src)
		if err != nil {
			return nil, err
		}
		r.Body = io.NopCloser(bytes.NewReader(merged))
		r.ContentLength = int64(len(merged))
		r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(merged)), nil }
		return next(r)
	}
}

func decode(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("decoding JSON: %w", err)
	}
	return nil
}

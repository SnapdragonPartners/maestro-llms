package google

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/auth/httptransport"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

var _ llms.ChatClient = (*Client)(nil)

// Gemini 3 requires the opaque functionCall thought_signature to be replayed
// on later turns (hard 400 otherwise). The toolkit must surface it from the
// response and round-trip it on the next request via ToolCall.ProviderSignature
// (G1 / ADR-0010) — no per-client cache.
func TestThoughtSignatureRoundTrips(t *testing.T) {
	sig := []byte("opaque-thought-sig-\x00\x01\xff")
	sigB64 := base64.StdEncoding.EncodeToString(sig)

	// Turn 1: response carries a functionCall part + thoughtSignature.
	turn1 := `{"candidates":[{"content":{"role":"model","parts":[` +
		`{"functionCall":{"name":"list_files","args":{"path":"."}},"thoughtSignature":"` + sigB64 + `"}` +
		`]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":4,"totalTokenCount":7}}`
	c1 := newClient(t, jsonHandler(t, 200, turn1))
	resp, err := c1.Complete(context.Background(), llms.ChatRequest{
		Messages: []llms.Message{llms.UserText("list files")},
		Tools:    []llms.ToolDefinition{{Name: "list_files", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatalf("turn 1 Complete: %v", err)
	}
	if len(resp.ToolCalls) != 1 || !bytes.Equal(resp.ToolCalls[0].ProviderSignature, sig) {
		t.Fatalf("thought_signature not surfaced into ToolCall.ProviderSignature: %+v", resp.ToolCalls)
	}

	// Turn 2: feed the assistant turn back; the outgoing genai functionCall
	// part must carry the same thoughtSignature.
	var body string
	c2 := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respTextJSON)
	})
	_, err = c2.Complete(context.Background(), llms.ChatRequest{
		Tools: []llms.ToolDefinition{{Name: "list_files", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		Messages: []llms.Message{
			llms.UserText("list files"),
			resp.Message, // assistant turn with the tool call + its ProviderSignature
			llms.ToolResultMessage(llms.ToolResult{ToolCallID: resp.ToolCalls[0].ID, Content: `{"files":[]}`}),
		},
	})
	if err != nil {
		t.Fatalf("turn 2 Complete: %v", err)
	}
	if !strings.Contains(body, `"thoughtSignature":"`+sigB64+`"`) {
		t.Fatalf("thought_signature not replayed on the resent functionCall part:\n%s", body)
	}
}

const respTextJSON = `{
"candidates":[{"content":{"role":"model","parts":[{"text":"hello world"}]},"finishReason":"STOP"}],
"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":7,"totalTokenCount":18},
"responseId":"resp_g1"}`

func newClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(WithAPIKey("k"), WithModel("gemini-test"),
		WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func jsonHandler(t *testing.T, status int, body string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func TestChatTextResponse(t *testing.T) {
	c := newClient(t, jsonHandler(t, 200, respTextJSON))
	resp, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages: []llms.Message{llms.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Message.Role != llms.RoleAssistant || len(resp.Message.Content) != 1 ||
		resp.Message.Content[0].Text != "hello world" {
		t.Fatalf("message not source of truth: %+v", resp.Message)
	}
	if resp.Text != "hello world" || resp.StopReason != "STOP" {
		t.Fatalf("text/stop mirror wrong: %q %q", resp.Text, resp.StopReason)
	}
	if resp.Usage.InputTokens != 11 || resp.Usage.OutputTokens != 7 ||
		resp.Usage.TotalTokens != 18 || resp.Usage.ProviderRequestID != "resp_g1" {
		t.Fatalf("usage mapping wrong: %+v", resp.Usage)
	}
	if resp.Raw == nil || c.Model().Provider != "google" || c.Model().Name != "gemini-test" {
		t.Fatalf("raw/model wrong")
	}
}

func TestChatToolCallResponse(t *testing.T) {
	body := `{"candidates":[{"content":{"role":"model","parts":[
 {"functionCall":{"name":"lookup","args":{"q":"Dan"}}}]},"finishReason":"STOP"}],
"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":9,"totalTokenCount":14}}`
	c := newClient(t, jsonHandler(t, 200, body))
	resp, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages: []llms.Message{llms.UserText("who is Dan?")},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "lookup" {
		t.Fatalf("tool call mirror wrong: %+v", resp.ToolCalls)
	}
	// Gemini has no call id; ID falls back to the function name.
	if resp.ToolCalls[0].ID != "lookup" {
		t.Fatalf("expected ID to fall back to name, got %q", resp.ToolCalls[0].ID)
	}
	var args map[string]any
	if err := json.Unmarshal(resp.ToolCalls[0].Parameters, &args); err != nil || args["q"] != "Dan" {
		t.Fatalf("tool args not preserved: %s (%v)", resp.ToolCalls[0].Parameters, err)
	}
	if resp.Message.Content[0].Type != llms.ContentToolCall {
		t.Fatalf("message tool_call part missing: %+v", resp.Message.Content)
	}
}

func TestToolChoiceRequiredSendsAnyMode(t *testing.T) {
	var captured map[string]any
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &captured)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respTextJSON)
	})
	_, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages:   []llms.Message{llms.UserText("hi")},
		Tools:      []llms.ToolDefinition{{Name: "t", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		ToolChoice: llms.ToolChoice{Type: llms.ToolChoiceRequired},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	tcfg, _ := captured["toolConfig"].(map[string]any)
	fcc, _ := tcfg["functionCallingConfig"].(map[string]any)
	if fcc["mode"] != "ANY" {
		t.Fatalf("ToolChoiceRequired must send functionCallingConfig.mode ANY, got %v", captured["toolConfig"])
	}
	if fcc["allowedFunctionNames"] != nil {
		t.Fatalf("Required must not restrict tool names, got %v", fcc["allowedFunctionNames"])
	}
}

func TestToolChoiceRequiredWithoutToolsRejected(t *testing.T) {
	c := newClient(t, jsonHandler(t, 200, respTextJSON))
	for _, tc := range []llms.ToolChoice{
		{Type: llms.ToolChoiceRequired},
		{Type: llms.ToolChoiceTool, Name: "x"},
	} {
		_, err := c.Complete(context.Background(), llms.ChatRequest{
			Messages:   []llms.Message{llms.UserText("hi")},
			ToolChoice: tc,
		})
		var pe *llms.ProviderError
		if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindBadRequest {
			t.Fatalf("%s with no tools must be bad_request, got %v", tc.Type, err)
		}
	}
}

// #88: with no tools offered, auto/none (and the zero choice) must not send
// toolConfig; strict servers reject a tool choice without tools.
func TestToolChoiceOmittedWithoutTools(t *testing.T) {
	for _, tc := range []llms.ToolChoice{
		{},
		{Type: llms.ToolChoiceAuto},
		{Type: llms.ToolChoiceNone},
	} {
		var captured map[string]any
		c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &captured)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, respTextJSON)
		})
		_, err := c.Complete(context.Background(), llms.ChatRequest{
			Messages:   []llms.Message{llms.UserText("hi")},
			ToolChoice: tc, // no Tools offered
		})
		if err != nil {
			t.Fatalf("%q: Complete: %v", tc.Type, err)
		}
		if v, ok := captured["toolConfig"]; ok {
			t.Fatalf("%q with no tools must omit toolConfig, got %v", tc.Type, v)
		}
		if v, ok := captured["tools"]; ok {
			t.Fatalf("%q with no tools must omit tools, got %v", tc.Type, v)
		}
	}
}

func TestCacheBreakpointIgnoredGracefully(t *testing.T) {
	c := newClient(t, jsonHandler(t, 200, respTextJSON))
	_, err := c.Complete(context.Background(), llms.ChatRequest{
		System:   []llms.ContentPart{{Type: llms.ContentText, Text: "s", CacheBreakpoint: true}},
		Messages: []llms.Message{{Role: llms.RoleUser, Content: []llms.ContentPart{{Type: llms.ContentText, Text: "hi", CacheBreakpoint: true}}}},
	})
	if err != nil {
		t.Fatalf("cache hint must be a safe no-op for Gemini, got %v", err)
	}
}

func TestChatRequestTranslation(t *testing.T) {
	var captured map[string]any
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &captured)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respTextJSON)
	})
	temp := float32(0)
	_, err := c.Complete(context.Background(), llms.ChatRequest{
		System: []llms.ContentPart{llms.Text("be terse")},
		Messages: []llms.Message{
			llms.UserText("relate me to Dan"),
			{Role: llms.RoleAssistant, Content: []llms.ContentPart{{
				Type:     llms.ContentToolCall,
				ToolCall: &llms.ToolCall{ID: "lookup", Name: "lookup", Parameters: json.RawMessage(`{"q":"Dan"}`)},
			}}},
			llms.ToolResultMessage(llms.ToolResult{ToolCallID: "lookup", Content: `{"ok":true}`}),
		},
		Temperature: &temp,
		Tools: []llms.ToolDefinition{{
			Name: "lookup", Description: "look up",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`),
		}},
		ToolChoice: llms.ToolChoice{Type: llms.ToolChoiceTool, Name: "lookup"},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if captured["systemInstruction"] == nil {
		t.Fatal("system not sent as systemInstruction")
	}
	contents, _ := captured["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("want 3 contents (user, model, user), got %d: %v", len(contents), captured["contents"])
	}
	roles := make([]string, 3)
	for i, ct := range contents {
		m, _ := ct.(map[string]any)
		roles[i], _ = m["role"].(string)
	}
	if roles[0] != "user" || roles[1] != "model" || roles[2] != "user" {
		t.Fatalf("content roles wrong: %v", roles)
	}
	tcfg, _ := captured["toolConfig"].(map[string]any)
	fcc, _ := tcfg["functionCallingConfig"].(map[string]any)
	if fcc["mode"] != "ANY" {
		t.Fatalf("forced tool choice not sent: %v", captured["toolConfig"])
	}
}

func TestChatErrorClassification(t *testing.T) {
	body := `{"error":{"code":429,"message":"quota","status":"RESOURCE_EXHAUSTED"}}`
	c := newClient(t, jsonHandler(t, 429, body))
	_, err := c.Complete(context.Background(), llms.ChatRequest{Messages: []llms.Message{llms.UserText("hi")}})
	var pe *llms.ProviderError
	if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindRateLimited || pe.StatusCode != 429 {
		t.Fatalf("want rate_limited ProviderError, got %v", err)
	}
}

func TestChatBadRequests(t *testing.T) {
	c := newClient(t, jsonHandler(t, 200, respTextJSON))
	cases := map[string]llms.ChatRequest{
		"no messages": {},
		"empty text": {Messages: []llms.Message{{Role: llms.RoleUser,
			Content: []llms.ContentPart{{Type: llms.ContentText}}}}},
		"system non-text": {
			System:   []llms.ContentPart{{Type: llms.ContentToolResult, ToolResult: &llms.ToolResult{}}},
			Messages: []llms.Message{llms.UserText("hi")}},
		"tool choice no name": {
			Messages:   []llms.Message{llms.UserText("hi")},
			ToolChoice: llms.ToolChoice{Type: llms.ToolChoiceTool}},
		"non-object schema": {
			Messages: []llms.Message{llms.UserText("hi")},
			Tools:    []llms.ToolDefinition{{Name: "x", InputSchema: json.RawMessage(`{"type":"array"}`)}}},
	}
	for name, req := range cases {
		_, err := c.Complete(context.Background(), req)
		var pe *llms.ProviderError
		if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindBadRequest {
			t.Fatalf("%s: want bad_request ProviderError, got %v", name, err)
		}
	}
}

func TestChatUnsupportedSchemaTypeRejected(t *testing.T) {
	c := newClient(t, jsonHandler(t, 200, respTextJSON))
	for name, schema := range map[string]string{
		"unknown leaf type": `{"type":"object","properties":{"x":{"type":"widget"}}}`,
		"null type":         `{"type":"object","properties":{"x":{"type":"null"}}}`,
		"type array":        `{"type":"object","properties":{"x":{"type":["string","null"]}}}`,
	} {
		_, err := c.Complete(context.Background(), llms.ChatRequest{
			Messages: []llms.Message{llms.UserText("hi")},
			Tools:    []llms.ToolDefinition{{Name: "t", InputSchema: json.RawMessage(schema)}},
		})
		var pe *llms.ProviderError
		if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindBadRequest {
			t.Fatalf("%s: want bad_request (no silent string coercion), got %v", name, err)
		}
	}
}

func TestChatToolResultNameResolvedFromPriorCall(t *testing.T) {
	var captured map[string]any
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &captured)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respTextJSON)
	})
	// Opaque, non-name ID (as OpenAI/Anthropic would produce).
	_, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages: []llms.Message{
			llms.UserText("weather?"),
			{Role: llms.RoleAssistant, Content: []llms.ContentPart{{
				Type:     llms.ContentToolCall,
				ToolCall: &llms.ToolCall{ID: "call_abc123", Name: "get_weather", Parameters: json.RawMessage(`{}`)},
			}}},
			llms.ToolResultMessage(llms.ToolResult{ToolCallID: "call_abc123", Content: `{}`}),
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	contents, _ := captured["contents"].([]any)
	last, _ := contents[len(contents)-1].(map[string]any)
	parts, _ := last["parts"].([]any)
	fr, _ := parts[0].(map[string]any)
	resp, _ := fr["functionResponse"].(map[string]any)
	if resp["name"] != "get_weather" {
		t.Fatalf("functionResponse.name not resolved from prior call: %v (want get_weather)", resp["name"])
	}
}

func TestNewConfigErrors(t *testing.T) {
	for _, opts := range [][]Option{
		{WithModel("m")},
		{WithAPIKey("k")},
		{WithAPIKey("k"), WithModel("m"), WithMaxRetries(-1)},
	} {
		_, err := New(opts...)
		var pe *llms.ProviderError
		if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindConfig {
			t.Fatalf("opts %v: want config ProviderError, got %v", opts, err)
		}
	}
}

// New and NewEmbeddings share one Option type (ADR-0027), so an option that
// only NewEmbeddings accepts must fail New loudly, never be ignored.
func TestNewRejectsEmbeddingOptions(t *testing.T) {
	for _, tc := range []struct {
		opt  Option
		want string
	}{
		{WithDimensions(0), "WithDimensions applies only to NewEmbeddings"},
		{WithMaxInputBytes(10), "WithMaxInputBytes applies only to NewEmbeddings"},
		{WithAutoTruncate(false), "WithAutoTruncate applies only to NewEmbeddings"},
	} {
		_, err := New(WithAPIKey("k"), WithModel("m"), tc.opt)
		var pe *llms.ProviderError
		if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindConfig || !strings.HasPrefix(pe.Message, tc.want) {
			t.Fatalf("want config error %q, got %v", tc.want, err)
		}
	}
}

// ADR-0028: New selects its backend through the same clientConfig as
// NewEmbeddings, so every backend rule holds for chat too.
func TestNewVertexBackendConfig(t *testing.T) {
	creds := auth.NewCredentials(&auth.CredentialsOptions{TokenProvider: stubTokenProvider{}})
	for _, tc := range []struct {
		name string
		opts []Option
		want string
	}{
		{"no backend", nil, "missing credentials"},
		{"vertex missing location", []Option{WithVertex("p", ""), WithCredentials(creds)}, "Vertex backend requires WithVertex"},
		{"vertex missing project", []Option{WithVertex("", "us"), WithCredentials(creds)}, "Vertex backend requires WithVertex"},
		{"vertex missing credentials", []Option{WithVertex("p", "us")}, "Vertex backend requires WithCredentials"},
		{"credentials without WithVertex", []Option{WithCredentials(creds)}, "Vertex backend requires WithVertex"},
		{"credentials with no TokenProvider", []Option{WithVertex("p", "us"), WithCredentials(&auth.Credentials{})}, "Google credentials have no TokenProvider"},
		{"api key + WithVertex", []Option{WithAPIKey("k"), WithVertex("p", "us")}, "ambiguous backend"},
		{"api key + empty WithVertex", []Option{WithAPIKey("k"), WithVertex("", "")}, "ambiguous backend"},
		{"api key + WithCredentials", []Option{WithAPIKey("k"), WithCredentials(creds)}, "ambiguous backend"},
		{"api key + nil WithCredentials", []Option{WithAPIKey("k"), WithCredentials(nil)}, "ambiguous backend"},
	} {
		_, err := New(append([]Option{WithModel("m")}, tc.opts...)...)
		var pe *llms.ProviderError
		if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindConfig || !strings.HasPrefix(pe.Message, tc.want) {
			t.Fatalf("%s: want config error %q, got %v", tc.name, tc.want, err)
		}
	}
	if _, err := New(WithModel("gemini-2.5-flash"), WithVertex("p", "global"), WithCredentials(creds)); err != nil {
		t.Fatalf("Vertex chat config must construct: %v", err)
	}
}

// A Vertex chat request goes to the Vertex publisher path under the
// injected (PSC) base URL, and a ProviderParams override's per-request
// HTTPOptions keeps that base URL (genai merges request options over client
// options field by field). An injected client replaces genai's
// authenticated one, so per ADR-0009 it must carry Google auth itself; this
// test builds it the way an application would.
func TestVertexChatRoutingAndAuth(t *testing.T) {
	var path, authz, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		path, authz, body = r.URL.Path, r.Header.Get("Authorization"), string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respTextJSON)
	}))
	defer srv.Close()
	creds := auth.NewCredentials(&auth.CredentialsOptions{TokenProvider: stubTokenProvider{}})
	pscClient := srv.Client()
	if err := httptransport.AddAuthorizationMiddleware(pscClient, creds); err != nil {
		t.Fatal(err)
	}
	c, err := New(WithModel("gemini-test"), WithVertex("proj", "global"), WithCredentials(creds),
		WithBaseURL(srv.URL), WithHTTPClient(pscClient))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages: []llms.Message{llms.UserText("hi")},
		ProviderParams: map[llms.ProviderID]map[string]any{
			llms.ProviderGoogle: {"generationConfig": map[string]any{"maxOutputTokens": 7}},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text == "" {
		t.Fatalf("empty response text: %+v", resp)
	}
	const wantPath = "/projects/proj/locations/global/publishers/google/models/gemini-test:generateContent"
	if !strings.HasSuffix(path, wantPath) {
		t.Fatalf("path %q, want suffix %q", path, wantPath)
	}
	if authz != "Bearer stub" {
		t.Fatalf("Authorization %q, want the PSC client's token", authz)
	}
	if !strings.Contains(body, `"maxOutputTokens":7`) {
		t.Fatalf("ProviderParams override missing from the Vertex body: %s", body)
	}
}

// TestUsageReasoningTokens pins ADR-0016: ThoughtsTokenCount surfaces
// as Usage.ReasoningTokens. This is the path that explains why a
// Gemini reasoning model can hit MAX_TOKENS with a small visible
// OutputTokens — the bulk of the budget went to thinking. Prior to
// ADR-0016 this number was silently dropped.
func TestUsageReasoningTokens(t *testing.T) {
	// Simulate a Gemini 3-class reasoning response: small visible
	// output (36), large thoughts (988), MAX_TOKENS finish reason.
	body := `{"candidates":[{"content":{"role":"model","parts":[
{"text":"short visible answer"}]},"finishReason":"MAX_TOKENS"}],
"usageMetadata":{"promptTokenCount":285,"candidatesTokenCount":36,
"thoughtsTokenCount":988,"totalTokenCount":1309},
"responseId":"resp_reasoning"}`
	c := newClient(t, jsonHandler(t, 200, body))
	resp, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages: []llms.Message{llms.UserText("explain something")},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	u := resp.Usage
	if u.InputTokens != 285 {
		t.Errorf("InputTokens = %d, want 285", u.InputTokens)
	}
	if u.OutputTokens != 36 {
		t.Errorf("OutputTokens = %d, want 36 (visible only)", u.OutputTokens)
	}
	if u.ReasoningTokens != 988 {
		t.Errorf("ReasoningTokens = %d, want 988 (thoughtsTokenCount)", u.ReasoningTokens)
	}
	if u.BillableOutputTokens != 36+988 {
		t.Errorf("BillableOutputTokens = %d, want %d (candidates+thoughts)",
			u.BillableOutputTokens, 36+988)
	}
	if u.TotalTokens != 1309 {
		t.Errorf("TotalTokens = %d, want 1309 (provider sum)", u.TotalTokens)
	}
	// Budget math: input + output + reasoning should reconcile to total.
	if u.InputTokens+u.OutputTokens+u.ReasoningTokens != u.TotalTokens {
		t.Errorf("budget math broken: %d + %d + %d != %d",
			u.InputTokens, u.OutputTokens, u.ReasoningTokens, u.TotalTokens)
	}
	// Billable identity: OutputTokens + ReasoningTokens == BillableOutputTokens.
	if u.OutputTokens+u.ReasoningTokens != u.BillableOutputTokens {
		t.Errorf("billable identity broken: %d + %d != %d",
			u.OutputTokens, u.ReasoningTokens, u.BillableOutputTokens)
	}
	// The MAX_TOKENS stop now has a story (988 reasoning tokens) rather
	// than looking like a confusing "36 max_tokens".
	if resp.StopReason != "MAX_TOKENS" {
		t.Errorf("StopReason = %q, want MAX_TOKENS", resp.StopReason)
	}
}

// TestUsageReasoningTokensZeroForNonReasoning verifies the field
// stays zero for the common non-reasoning case (no thoughtsTokenCount
// in the response). Existing callers that don't read ReasoningTokens
// are unaffected.
func TestUsageReasoningTokensZeroForNonReasoning(t *testing.T) {
	c := newClient(t, jsonHandler(t, 200, respTextJSON))
	resp, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages: []llms.Message{llms.UserText("hi")},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Usage.ReasoningTokens != 0 {
		t.Errorf("ReasoningTokens = %d, want 0 (no thoughts in response)",
			resp.Usage.ReasoningTokens)
	}
}

// TestForeignReasoningNotSent: a transcript carrying another provider's
// reasoning (here Anthropic's) is accepted and none of it reaches the wire
// (ADR-0021).
func TestForeignReasoningNotSent(t *testing.T) {
	var body string
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respTextJSON)
	})
	_, err := c.Complete(context.Background(), llms.ChatRequest{Messages: []llms.Message{
		llms.UserText("q"),
		{Role: llms.RoleAssistant, Content: []llms.ContentPart{
			{Type: llms.ContentReasoning, Reasoning: &llms.Reasoning{
				Provider:     llms.ProviderAnthropic,
				Text:         "THINK-MARKER",
				ProviderData: []byte(`{"type":"thinking","signature":"SIG-MARKER"}`),
			}},
			llms.Text("calling"),
			{Type: llms.ContentToolCall, ToolCall: &llms.ToolCall{ID: "call_1", Name: "f", Parameters: json.RawMessage(`{}`)}},
		}},
		llms.ToolResultMessage(llms.ToolResult{ToolCallID: "call_1", Content: "r"}),
	}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if strings.Contains(body, "MARKER") {
		t.Fatalf("foreign reasoning reached the wire:\n%s", body)
	}
	if !strings.Contains(body, "calling") {
		t.Fatalf("the assistant text must still be sent:\n%s", body)
	}
}

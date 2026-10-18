package testllm

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"reflect"
	"sync"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// Compile-time interface checks (kept in the test file: package-level var
// assertions would trip gochecknoglobals in non-test source).
var (
	_ llms.ChatClient      = (*FakeChatClient)(nil)
	_ llms.EmbeddingClient = (*FakeEmbeddingClient)(nil)
)

func TestFakeChatFixedText(t *testing.T) {
	c := &FakeChatClient{ModelRef: llms.ModelRef{Provider: "fake", Name: "m"}, Text: "hello"}
	resp, err := c.Complete(context.Background(), llms.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "hello" || resp.Message.Content[0].Text != "hello" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if c.Model().Name != "m" {
		t.Fatalf("model = %+v", c.Model())
	}
}

func TestFakeChatScriptedResponsesInOrderThenRepeat(t *testing.T) {
	c := &FakeChatClient{Responses: []llms.ChatResponse{
		TextResponse("first"), TextResponse("second"),
	}}
	got := []string{}
	for range 4 {
		r, err := c.Complete(context.Background(), llms.ChatRequest{})
		if err != nil {
			t.Fatal(err)
		}
		// Message is the source of truth; Text must mirror it.
		if r.Message.Content[0].Text != r.Text {
			t.Fatalf("Text %q does not mirror Message %q", r.Text, r.Message.Content[0].Text)
		}
		got = append(got, r.Text)
	}
	want := []string{"first", "second", "second", "second"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call %d = %q, want %q (all=%v)", i, got[i], want[i], got)
		}
	}
	if len(c.Calls()) != 4 {
		t.Fatalf("recorded %d calls, want 4", len(c.Calls()))
	}
}

func TestFakeChatErrorAndContextCancel(t *testing.T) {
	sentinel := errors.New("scripted")
	c := &FakeChatClient{Err: sentinel}
	if _, err := c.Complete(context.Background(), llms.ChatRequest{}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want scripted", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Reset()
	if _, err := c.Complete(ctx, llms.ChatRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(c.Calls()) != 1 {
		t.Fatalf("canceled call not recorded: %d calls", len(c.Calls()))
	}
}

func TestFakeChatRecordingIsolatedFromCallerMutation(t *testing.T) {
	c := &FakeChatClient{Text: "ok"}
	req := llms.ChatRequest{
		Messages: []llms.Message{llms.UserText("original")},
		Metadata: map[string]string{"k": "v1"},
	}
	if _, err := c.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	// Mutate the caller's request and its nested slice/map after the call.
	req.Messages[0].Content[0].Text = "mutated"
	req.Metadata["k"] = "v2"

	rec := c.Calls()[0]
	if rec.Messages[0].Content[0].Text != "original" || rec.Metadata["k"] != "v1" {
		t.Fatalf("recording reflects post-call mutation: %+v / %v", rec.Messages[0].Content[0], rec.Metadata)
	}
}

func TestTextResponseIsSpecFaithful(t *testing.T) {
	r := TextResponse("hi")
	if r.Message.Role != llms.RoleAssistant || r.Message.Content[0].Text != "hi" || r.Text != "hi" {
		t.Fatalf("not spec-faithful: %+v", r)
	}
}

func TestToolCallResponseHelper(t *testing.T) {
	r := ToolCallResponse("id1", "lookup", `{"q":"x"}`)
	if len(r.ToolCalls) != 1 || r.ToolCalls[0].Name != "lookup" {
		t.Fatalf("tool calls = %+v", r.ToolCalls)
	}
	p := r.Message.Content[0]
	if p.Type != llms.ContentToolCall || p.ToolCall.ID != "id1" {
		t.Fatalf("content part = %+v", p)
	}
}

func TestFakeEmbeddingDeterministicAndOrdered(t *testing.T) {
	a := &FakeEmbeddingClient{Dims: 16}
	b := &FakeEmbeddingClient{Dims: 16}
	req := llms.EmbeddingRequest{Inputs: []llms.EmbeddingInput{
		{ID: "1", Text: "alpha"}, {ID: "2", Text: "beta"},
	}}
	ra, err := a.Embed(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	rb, err := b.Embed(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(ra.Vectors) != 2 || ra.Vectors[0].ID != "1" || ra.Vectors[1].ID != "2" {
		t.Fatalf("order/ids wrong: %+v", ra.Vectors)
	}
	if len(ra.Vectors[0].Values) != 16 {
		t.Fatalf("dims = %d, want 16", len(ra.Vectors[0].Values))
	}
	// Same text -> identical vector across independent clients.
	for i := range ra.Vectors[0].Values {
		if ra.Vectors[0].Values[i] != rb.Vectors[0].Values[i] {
			t.Fatalf("non-deterministic at %d", i)
		}
	}
	// Different text -> different vector.
	if ra.Vectors[0].Values[0] == ra.Vectors[1].Values[0] &&
		ra.Vectors[0].Values[1] == ra.Vectors[1].Values[1] {
		t.Fatal("distinct inputs produced identical leading components")
	}
}

func TestFakeEmbeddingVectorMapAndDimsOverride(t *testing.T) {
	c := &FakeEmbeddingClient{
		Dims:    4,
		Vectors: map[string][]float32{"pinned": {9, 9, 9}},
	}
	resp, err := c.Embed(context.Background(), llms.EmbeddingRequest{
		Dimensions: 32,
		Inputs:     []llms.EmbeddingInput{{ID: "p", Text: "pinned"}, {ID: "h", Text: "hashed"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Vectors[0].Values; len(got) != 3 || got[0] != 9 {
		t.Fatalf("pinned vector not used: %+v", got)
	}
	if len(resp.Vectors[1].Values) != 32 {
		t.Fatalf("per-request Dimensions override ignored: %d", len(resp.Vectors[1].Values))
	}
}

func TestFakesConcurrentUse(t *testing.T) {
	c := &FakeChatClient{Text: "ok"}
	e := &FakeEmbeddingClient{Dims: 4}
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = c.Complete(context.Background(), llms.ChatRequest{}) }()
		go func() {
			defer wg.Done()
			_, _ = e.Embed(context.Background(), llms.EmbeddingRequest{
				Inputs: []llms.EmbeddingInput{{ID: "x", Text: "y"}},
			})
		}()
	}
	wg.Wait()
	if len(c.Calls()) != 50 || len(e.Calls()) != 50 {
		t.Fatalf("lost calls under concurrency: chat=%d emb=%d", len(c.Calls()), len(e.Calls()))
	}
}

// Opaque payload pointers and byte slices are deep-copied too, on the way in
// and on the way out of Calls().
func TestFakeChatRecordingIsolatesOpaquePayloads(t *testing.T) {
	c := &FakeChatClient{Text: "ok"}
	req := llms.ChatRequest{Messages: []llms.Message{llms.UserText("q"), {Role: llms.RoleAssistant, Content: []llms.ContentPart{
		{Type: llms.ContentReasoning, Reasoning: &llms.Reasoning{Provider: llms.ProviderAnthropic, Text: "t", ProviderData: []byte("data")}},
		{Type: llms.ContentToolCall, ToolCall: &llms.ToolCall{ID: "c", Name: "f", ProviderSignature: []byte("sig")}},
	}}}}
	if _, err := c.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	// Caller mutates after the call.
	parts := req.Messages[1].Content
	parts[0].Reasoning.Text = "mutated"
	parts[0].Reasoning.ProviderData[0] = 'X'
	parts[1].ToolCall.ProviderSignature[0] = 'X'
	// A test mutates what Calls() returned.
	got := c.Calls()[0].Messages[1].Content
	got[0].Reasoning.ProviderData[1] = 'Y'

	rec := c.Calls()[0].Messages[1].Content
	if rec[0].Reasoning.Text != "t" || string(rec[0].Reasoning.ProviderData) != "data" {
		t.Fatalf("reasoning recording not isolated: %+v / %q", rec[0].Reasoning, rec[0].Reasoning.ProviderData)
	}
	if string(rec[1].ToolCall.ProviderSignature) != "sig" {
		t.Fatalf("ProviderSignature recording not isolated: %q", rec[1].ToolCall.ProviderSignature)
	}
}

func TestFakeChatRecordingIsolatesReasoningConfig(t *testing.T) {
	c := &FakeChatClient{Text: "ok"}
	req := llms.ChatRequest{
		Messages:  []llms.Message{llms.UserText("q")},
		Reasoning: &llms.ReasoningConfig{Effort: llms.ReasoningEffortLow},
	}
	if _, err := c.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	req.Reasoning.Effort = llms.ReasoningEffortHigh
	c.Calls()[0].Reasoning.BudgetTokens = 99
	if rec := c.Calls()[0].Reasoning; rec.Effort != llms.ReasoningEffortLow || rec.BudgetTokens != 0 {
		t.Fatalf("ReasoningConfig recording not isolated: %+v", rec)
	}
}

func TestFakeChatRecordingIsolatesProviderParams(t *testing.T) {
	c := &FakeChatClient{Text: "ok"}
	req := llms.ChatRequest{
		Messages: []llms.Message{llms.UserText("q")},
		ProviderParams: map[llms.ProviderID]map[string]any{
			llms.ProviderVLLM: {"chat_template_kwargs": map[string]any{"enable_thinking": false}, "stop": []any{"x"}},
		},
	}
	if _, err := c.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	in := req.ProviderParams[llms.ProviderVLLM]
	kw, _ := in["chat_template_kwargs"].(map[string]any)
	kw["enable_thinking"] = true
	stop, _ := in["stop"].([]any)
	stop[0] = "y"
	c.Calls()[0].ProviderParams[llms.ProviderVLLM]["new"] = 1
	rec := c.Calls()[0].ProviderParams[llms.ProviderVLLM]
	recKW, _ := rec["chat_template_kwargs"].(map[string]any)
	recStop, _ := rec["stop"].([]any)
	if recKW["enable_thinking"] != false || len(recStop) != 1 || recStop[0] != "x" || rec["new"] != nil {
		t.Fatalf("ProviderParams recording not isolated: %+v", rec)
	}
}

// #93 review: ProviderParams are recorded as their JSON form, so no Go value
// shape can alias the recording: slices, maps, raw JSON, pointers, structs,
// private state (*big.Int) and json:"-" cycles. Both mutation directions.
func TestFakeChatRecordingIsolatesProviderParamValueShapes(t *testing.T) {
	type node struct {
		Name string `json:"name"`
		Self *node  `json:"-"`
	}
	cyc := &node{Name: "n"}
	cyc.Self = cyc
	n := big.NewInt(123)
	params := map[string]any{
		"strings": []string{"a"},
		"bools":   map[string]bool{"x": true},
		"raw":     json.RawMessage(`{"k":1}`),
		"big":     n,
		"cycle":   cyc,
		"array":   [1][]int{{1}},
	}
	c := &FakeChatClient{Text: "ok"}
	if _, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages:       []llms.Message{llms.UserText("q")},
		ProviderParams: map[llms.ProviderID]map[string]any{llms.ProviderVLLM: params},
	}); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"strings": []any{"a"},
		"bools":   map[string]any{"x": true},
		"raw":     map[string]any{"k": json.Number("1")},
		"big":     json.Number("123"),
		"cycle":   map[string]any{"name": "n"},
		"array":   []any{[]any{json.Number("1")}},
	}
	if got := c.Calls()[0].ProviderParams[llms.ProviderVLLM]; !reflect.DeepEqual(got, want) {
		t.Fatalf("recorded = %#v\nwant       %#v", got, want)
	}
	// The caller mutates its values after the call...
	params["strings"].([]string)[0] = "MUT" //nolint:forcetypeassert // known fixture shape
	n.SetInt64(456)
	cyc.Name = "MUT"
	// ...and a test mutates what Calls() returned.
	got := c.Calls()[0].ProviderParams[llms.ProviderVLLM]
	got["bools"].(map[string]any)["x"] = false //nolint:forcetypeassert // known recorded shape
	got["strings"].([]any)[0] = "MUT"          //nolint:forcetypeassert // known recorded shape
	if again := c.Calls()[0].ProviderParams[llms.ProviderVLLM]; !reflect.DeepEqual(again, want) {
		t.Fatalf("recording changed by mutation: %#v", again)
	}
}

// A ProviderParams value that can't marshal is rejected as bad_request (as
// the adapters reject it), and the call is still recorded.
func TestFakeChatRejectsNonMarshalableProviderParams(t *testing.T) {
	c := &FakeChatClient{Text: "ok", ModelRef: llms.ModelRef{Provider: llms.ProviderVLLM, Name: "m"}}
	_, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages:       []llms.Message{llms.UserText("q")},
		ProviderParams: map[llms.ProviderID]map[string]any{llms.ProviderVLLM: {"bad": make(chan int)}},
	})
	var pe *llms.ProviderError
	if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindBadRequest {
		t.Fatalf("err = %v, want bad_request", err)
	}
	if len(c.Calls()) != 1 {
		t.Fatalf("calls = %d, want the rejected call recorded", len(c.Calls()))
	}
}

// PR #98 review: an adapter validates only its own entry, so an invalid entry
// for another provider must not fail a fake standing in for this one.
func TestFakeChatIgnoresInvalidForeignProviderParams(t *testing.T) {
	c := &FakeChatClient{Text: "ok", ModelRef: llms.ModelRef{Provider: llms.ProviderVLLM, Name: "m"}}
	_, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages: []llms.Message{llms.UserText("q")},
		ProviderParams: map[llms.ProviderID]map[string]any{
			llms.ProviderVLLM:   {"max_tokens": 5},
			llms.ProviderOpenAI: {"bad": make(chan int)},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v, want success: the invalid entry is another provider's", err)
	}
	rec := c.Calls()[0].ProviderParams
	if rec[llms.ProviderVLLM]["max_tokens"] != json.Number("5") {
		t.Fatalf("selected entry = %#v, want its JSON snapshot", rec[llms.ProviderVLLM])
	}
	if rec[llms.ProviderOpenAI] != nil {
		t.Fatalf("invalid foreign entry = %#v, want recorded as nil", rec[llms.ProviderOpenAI])
	}

	// With no selected provider, the fake stands in for any adapter, so the
	// invalid entry fails the call.
	unscoped := &FakeChatClient{Text: "ok"}
	_, err = unscoped.Complete(context.Background(), llms.ChatRequest{
		Messages:       []llms.Message{llms.UserText("q")},
		ProviderParams: map[llms.ProviderID]map[string]any{llms.ProviderOpenAI: {"bad": make(chan int)}},
	})
	var pe *llms.ProviderError
	if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindBadRequest {
		t.Fatalf("unscoped fake: err = %v, want bad_request", err)
	}
}

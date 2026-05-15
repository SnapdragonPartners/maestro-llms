package testllm

import (
	"context"
	"errors"
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
		{Text: "first"}, {Text: "second"},
	}}
	got := []string{}
	for range 4 {
		r, err := c.Complete(context.Background(), llms.ChatRequest{})
		if err != nil {
			t.Fatal(err)
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
	if _, err := c.Complete(ctx, llms.ChatRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
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

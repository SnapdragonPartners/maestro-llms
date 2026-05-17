package middleware

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/ratelimit"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"
)

// recorder is a concurrency-safe Observer that keeps every Event.
type recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *recorder) Observe(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) snapshot() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

func TestMetricsChatObservesSuccess(t *testing.T) {
	rec := &recorder{}
	fake := &testllm.FakeChatClient{
		ModelRef: llms.ModelRef{Provider: "p", Name: "m"},
		Func: func(_ context.Context, _ llms.ChatRequest) (llms.ChatResponse, error) {
			time.Sleep(10 * time.Millisecond)
			return llms.ChatResponse{Text: "ok", Usage: llms.Usage{InputTokens: 7, OutputTokens: 3}}, nil
		},
	}
	c := MetricsChat(rec)(fake)
	resp, err := c.Complete(context.Background(), llms.ChatRequest{Purpose: llms.PurposeChat})
	if err != nil || resp.Text != "ok" {
		t.Fatalf("unexpected result %q / %v", resp.Text, err)
	}
	ev := rec.snapshot()
	if len(ev) != 1 {
		t.Fatalf("want exactly 1 event, got %d", len(ev))
	}
	e := ev[0]
	if e.Provider != "p" || e.Model != "m" || e.Operation != ratelimit.OperationChat || e.Purpose != llms.PurposeChat {
		t.Fatalf("event identity wrong: %+v", e)
	}
	if e.Err != nil || e.Usage.InputTokens != 7 || e.Usage.OutputTokens != 3 {
		t.Fatalf("usage/err wrong: %+v", e)
	}
	if e.Latency < 8*time.Millisecond {
		t.Fatalf("latency not measured: %v", e.Latency)
	}
}

func TestMetricsChatObservesErrorAndPassesThrough(t *testing.T) {
	rec := &recorder{}
	wantErr := &llms.ProviderError{Provider: "p", Model: "m", Kind: llms.ErrorKindUnavailable}
	fake := &testllm.FakeChatClient{
		Func: func(_ context.Context, _ llms.ChatRequest) (llms.ChatResponse, error) {
			return llms.ChatResponse{}, wantErr
		},
	}
	c := MetricsChat(rec)(fake)
	_, err := c.Complete(context.Background(), llms.ChatRequest{})

	var pe *llms.ProviderError
	if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindUnavailable {
		t.Fatalf("error must pass through unwrapped, got %v", err)
	}
	ev := rec.snapshot()
	if len(ev) != 1 || ev[0].Err == nil {
		t.Fatalf("error call must still emit an event with Err set: %+v", ev)
	}
	if (ev[0].Usage != llms.Usage{}) {
		t.Fatalf("error path must report zero usage, got %+v", ev[0].Usage)
	}
}

func TestMetricsNilObserverIsPassThrough(t *testing.T) {
	fake := &testllm.FakeChatClient{Text: "ok"}
	c := MetricsChat(nil)(fake)
	if c != llms.ChatClient(fake) {
		t.Fatal("nil observer must return next unchanged (no wrapper)")
	}
	resp, err := c.Complete(context.Background(), llms.ChatRequest{})
	if err != nil || resp.Text != "ok" {
		t.Fatalf("passthrough broken: %q / %v", resp.Text, err)
	}

	efake := &testllm.FakeEmbeddingClient{}
	if ec := MetricsEmbeddings(nil)(efake); ec != llms.EmbeddingClient(efake) {
		t.Fatal("nil observer (embeddings) must return next unchanged")
	}
}

func TestMetricsEmbeddingsObserves(t *testing.T) {
	rec := &recorder{}
	fake := &testllm.FakeEmbeddingClient{
		ModelRef: llms.ModelRef{Provider: "p", Name: "e"},
		Func: func(_ context.Context, _ llms.EmbeddingRequest) (llms.EmbeddingResponse, error) {
			return llms.EmbeddingResponse{
				Vectors: []llms.EmbeddingVector{{ID: "a"}},
				Usage:   llms.Usage{EmbeddingTokens: 5},
			}, nil
		},
	}
	c := MetricsEmbeddings(rec)(fake)
	if _, err := c.Embed(context.Background(), llms.EmbeddingRequest{Purpose: llms.PurposeEmbedding}); err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	ev := rec.snapshot()
	if len(ev) != 1 || ev[0].Operation != ratelimit.OperationEmbedding || ev[0].Usage.EmbeddingTokens != 5 {
		t.Fatalf("embedding event wrong: %+v", ev)
	}
}

func TestMetricsConcurrentUseRaceClean(t *testing.T) {
	rec := &recorder{}
	fake := &testllm.FakeChatClient{Text: "ok"}
	c := MetricsChat(rec)(fake)

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			for range 20 {
				c.Complete(context.Background(), llms.ChatRequest{}) //nolint:errcheck // exercising observer under -race
			}
		})
	}
	wg.Wait()
	if got := len(rec.snapshot()); got != 1000 {
		t.Fatalf("want 1000 events (50x20), got %d", got)
	}
}

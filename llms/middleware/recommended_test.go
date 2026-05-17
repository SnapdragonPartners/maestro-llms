package middleware

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"
)

func TestRecommendedChatValidationIsOutermost(t *testing.T) {
	rec := &recorder{}
	var calls int32
	base := countingFake(&calls)
	c := RecommendedChat(base, RecommendedConfig{Observer: rec})

	// A malformed request must be rejected before reaching the provider OR
	// the (innermost) metrics observer.
	_, err := c.Complete(context.Background(), llms.ChatRequest{}) // no messages
	if !errors.As(err, new(*ValidationError)) {
		t.Fatalf("want *ValidationError, got %v", err)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatal("validation must reject before the provider is called")
	}
	if len(rec.snapshot()) != 0 {
		t.Fatal("validation is outermost: metrics must not observe a rejected request")
	}
}

func TestRecommendedChatRetryWrapsMetrics(t *testing.T) {
	rec := &recorder{}
	var calls int32
	base := &testllm.FakeChatClient{
		ModelRef: llms.ModelRef{Provider: "p", Name: "m"},
		Func: func(_ context.Context, _ llms.ChatRequest) (llms.ChatResponse, error) {
			if atomic.AddInt32(&calls, 1) < 3 {
				return llms.ChatResponse{}, &llms.ProviderError{Kind: llms.ErrorKindUnavailable}
			}
			return llms.ChatResponse{Text: "ok"}, nil
		},
	}
	c := RecommendedChat(base, RecommendedConfig{Retry: fastRetry(5), Observer: rec})

	resp, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages: []llms.Message{llms.UserText("hi")},
	})
	if err != nil || resp.Text != "ok" {
		t.Fatalf("expected success after retries, got %q / %v", resp.Text, err)
	}
	// Metrics is innermost (inside retry): one event per provider attempt.
	if got := len(rec.snapshot()); got != 3 {
		t.Fatalf("want 3 metrics events (one per attempt), got %d", got)
	}
}

func TestRecommendedChatMinimalConfigWorks(t *testing.T) {
	var calls int32
	c := RecommendedChat(countingFake(&calls), RecommendedConfig{})
	resp, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages: []llms.Message{llms.UserText("hi")},
	})
	if err != nil || resp.Text != "ok" || calls != 1 {
		t.Fatalf("minimal recommended chain broken: %q / %v / calls=%d", resp.Text, err, calls)
	}
}

func TestRecommendedChatTimeoutIncludedWhenPositive(t *testing.T) {
	base := &testllm.FakeChatClient{
		Func: func(ctx context.Context, _ llms.ChatRequest) (llms.ChatResponse, error) {
			select {
			case <-time.After(200 * time.Millisecond):
				return llms.ChatResponse{Text: "slow"}, nil
			case <-ctx.Done():
				return llms.ChatResponse{}, ctx.Err()
			}
		},
	}
	// Tiny timeout + no retry so the deadline is observable and not retried.
	c := RecommendedChat(base, RecommendedConfig{
		Timeout: 15 * time.Millisecond,
		Retry:   RetryConfig{MaxAttempts: 1},
	})
	_, err := c.Complete(context.Background(), llms.ChatRequest{
		Messages: []llms.Message{llms.UserText("hi")},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout middleware should have been applied, got %v", err)
	}
}

func TestRecommendedEmbeddingsNoValidationButComposed(t *testing.T) {
	rec := &recorder{}
	var calls int32
	base := &testllm.FakeEmbeddingClient{
		ModelRef: llms.ModelRef{Provider: "p", Name: "e"},
		Func: func(_ context.Context, _ llms.EmbeddingRequest) (llms.EmbeddingResponse, error) {
			if atomic.AddInt32(&calls, 1) < 2 {
				return llms.EmbeddingResponse{}, &llms.ProviderError{Kind: llms.ErrorKindTimeout}
			}
			return llms.EmbeddingResponse{Vectors: []llms.EmbeddingVector{{ID: "a"}}}, nil
		},
	}
	c := RecommendedEmbeddings(base, RecommendedConfig{Retry: fastRetry(3), Observer: rec})

	// Empty request is fine: embeddings have no structural validation.
	resp, err := c.Embed(context.Background(), llms.EmbeddingRequest{})
	if err != nil || len(resp.Vectors) != 1 {
		t.Fatalf("recommended embeddings broken: %+v / %v", resp, err)
	}
	if got := len(rec.snapshot()); got != 2 {
		t.Fatalf("want 2 metrics events (retry then success), got %d", got)
	}
}

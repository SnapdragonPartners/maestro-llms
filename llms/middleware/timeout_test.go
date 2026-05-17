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

// ctxAwareSlow returns a fake Complete that finishes after d, or returns the
// context error if the call's deadline trips first.
func ctxAwareSlow(d time.Duration) func(context.Context, llms.ChatRequest) (llms.ChatResponse, error) {
	return func(ctx context.Context, _ llms.ChatRequest) (llms.ChatResponse, error) {
		select {
		case <-time.After(d):
			return llms.ChatResponse{Text: "ok"}, nil
		case <-ctx.Done():
			return llms.ChatResponse{}, ctx.Err()
		}
	}
}

func TestTimeoutChatTripsOnSlowCall(t *testing.T) {
	fake := &testllm.FakeChatClient{Func: ctxAwareSlow(200 * time.Millisecond)}
	c := TimeoutChat(20 * time.Millisecond)(fake)

	start := time.Now()
	_, err := c.Complete(context.Background(), llms.ChatRequest{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context.DeadlineExceeded, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Fatalf("deadline not enforced promptly, took %v", elapsed)
	}
}

func TestTimeoutChatFastCallUnaffected(t *testing.T) {
	fake := &testllm.FakeChatClient{Func: ctxAwareSlow(5 * time.Millisecond)}
	c := TimeoutChat(time.Second)(fake)
	resp, err := c.Complete(context.Background(), llms.ChatRequest{})
	if err != nil || resp.Text != "ok" {
		t.Fatalf("fast call must succeed, got %q / %v", resp.Text, err)
	}
}

func TestTimeoutChatNonPositiveIsNoOp(t *testing.T) {
	var hadDeadline bool
	fake := &testllm.FakeChatClient{
		Func: func(ctx context.Context, _ llms.ChatRequest) (llms.ChatResponse, error) {
			_, hadDeadline = ctx.Deadline()
			return llms.ChatResponse{Text: "ok"}, nil
		},
	}
	for _, d := range []time.Duration{0, -time.Second} {
		hadDeadline = false
		c := TimeoutChat(d)(fake)
		if _, err := c.Complete(context.Background(), llms.ChatRequest{}); err != nil {
			t.Fatalf("d=%v: unexpected error %v", d, err)
		}
		if hadDeadline {
			t.Fatalf("d=%v: middleware must not impose a deadline", d)
		}
	}
}

// Composition: timeout sits inside retry, so a first attempt that times out is
// retried with a fresh deadline (the prior expiry must not poison attempt 2).
// Real providers surface a tripped deadline as a retryable timeout via apierr;
// the fake mimics that classification.
func TestTimeoutComposesInsideRetryFreshBudgetPerAttempt(t *testing.T) {
	var calls int32
	fake := &testllm.FakeChatClient{
		Func: func(ctx context.Context, _ llms.ChatRequest) (llms.ChatResponse, error) {
			if atomic.AddInt32(&calls, 1) == 1 {
				<-ctx.Done() // first attempt: block until its deadline trips
				return llms.ChatResponse{}, &llms.ProviderError{
					Provider: "p", Model: "m", Kind: llms.ErrorKindTimeout, Cause: ctx.Err(),
				}
			}
			return llms.ChatResponse{Text: "ok"}, nil // second attempt: fast
		},
	}
	c := RetryChat(fastRetry(3))(TimeoutChat(20 * time.Millisecond)(fake))
	resp, err := c.Complete(context.Background(), llms.ChatRequest{})
	if err != nil {
		t.Fatalf("retry should recover after a per-attempt timeout, got %v", err)
	}
	if resp.Text != "ok" {
		t.Fatalf("unexpected response %q", resp.Text)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("want 2 attempts (timeout then success), got %d", got)
	}
}

func TestTimeoutChatPassesThroughProviderError(t *testing.T) {
	fake := &testllm.FakeChatClient{
		Func: func(_ context.Context, _ llms.ChatRequest) (llms.ChatResponse, error) {
			return llms.ChatResponse{}, &llms.ProviderError{Provider: "p", Model: "m", Kind: llms.ErrorKindBadRequest}
		},
	}
	c := TimeoutChat(time.Second)(fake)
	_, err := c.Complete(context.Background(), llms.ChatRequest{})
	var pe *llms.ProviderError
	if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindBadRequest {
		t.Fatalf("non-deadline error must pass through unwrapped, got %v", err)
	}
}

func TestTimeoutEmbeddingsTripsOnSlowCall(t *testing.T) {
	fake := &testllm.FakeEmbeddingClient{
		Func: func(ctx context.Context, _ llms.EmbeddingRequest) (llms.EmbeddingResponse, error) {
			select {
			case <-time.After(200 * time.Millisecond):
				return llms.EmbeddingResponse{}, nil
			case <-ctx.Done():
				return llms.EmbeddingResponse{}, ctx.Err()
			}
		},
	}
	c := TimeoutEmbeddings(20 * time.Millisecond)(fake)
	if _, err := c.Embed(context.Background(), llms.EmbeddingRequest{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context.DeadlineExceeded, got %v", err)
	}
}

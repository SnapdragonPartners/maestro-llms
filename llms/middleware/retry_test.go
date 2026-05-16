package middleware

import (
	"context"
	"errors"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"
)

// fastRetry is DefaultRetryConfig with sub-millisecond, jitter-free backoff so
// tests exercise the retry logic without sleeping for real seconds.
func fastRetry(maxAttempts int) RetryConfig {
	return RetryConfig{
		MaxAttempts:   maxAttempts,
		InitialDelay:  time.Millisecond,
		MaxDelay:      2 * time.Millisecond,
		BackoffFactor: 2.0,
		Jitter:        0,
	}
}

func provErr(kind llms.ErrorKind, retryAfter time.Duration) error {
	return &llms.ProviderError{Provider: "p", Model: "m", Kind: kind, RetryAfter: retryAfter}
}

func TestRetryChatRetriesThenSucceeds(t *testing.T) {
	var calls int32
	fake := &testllm.FakeChatClient{
		ModelRef: llms.ModelRef{Provider: "p", Name: "m"},
		Func: func(_ context.Context, _ llms.ChatRequest) (llms.ChatResponse, error) {
			if atomic.AddInt32(&calls, 1) < 3 {
				return llms.ChatResponse{}, provErr(llms.ErrorKindUnavailable, 0)
			}
			return llms.ChatResponse{Text: "ok"}, nil
		},
	}
	c := RetryChat(fastRetry(5))(fake)
	resp, err := c.Complete(context.Background(), llms.ChatRequest{})
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if resp.Text != "ok" {
		t.Fatalf("unexpected response: %q", resp.Text)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("want 3 attempts, got %d", got)
	}
}

func TestRetryChatNonRetryableFailsFast(t *testing.T) {
	var calls int32
	fake := &testllm.FakeChatClient{
		Func: func(_ context.Context, _ llms.ChatRequest) (llms.ChatResponse, error) {
			atomic.AddInt32(&calls, 1)
			return llms.ChatResponse{}, provErr(llms.ErrorKindAuth, 0)
		},
	}
	c := RetryChat(fastRetry(5))(fake)
	_, err := c.Complete(context.Background(), llms.ChatRequest{})
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("non-retryable must not retry; want 1 attempt, got %d", got)
	}
	var pe *llms.ProviderError
	if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindAuth {
		t.Fatalf("error must pass through unwrapped as *ProviderError(auth), got %v", err)
	}
}

func TestRetryChatExhaustionReturnsLastError(t *testing.T) {
	var calls int32
	fake := &testllm.FakeChatClient{
		Func: func(_ context.Context, _ llms.ChatRequest) (llms.ChatResponse, error) {
			atomic.AddInt32(&calls, 1)
			return llms.ChatResponse{}, provErr(llms.ErrorKindUnavailable, 0)
		},
	}
	c := RetryChat(fastRetry(3))(fake)
	_, err := c.Complete(context.Background(), llms.ChatRequest{})
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("want exactly MaxAttempts=3 calls, got %d", got)
	}
	var pe *llms.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("exhausted retry must return the last error unwrapped, got %v", err)
	}
}

func TestRetryHonorsRetryAfter(t *testing.T) {
	const retryAfter = 60 * time.Millisecond
	var calls int32
	fake := &testllm.FakeChatClient{
		Func: func(_ context.Context, _ llms.ChatRequest) (llms.ChatResponse, error) {
			if atomic.AddInt32(&calls, 1) == 1 {
				return llms.ChatResponse{}, provErr(llms.ErrorKindRateLimited, retryAfter)
			}
			return llms.ChatResponse{Text: "ok"}, nil
		},
	}
	// InitialDelay is tiny; the RetryAfter hint (60ms) must dominate the wait.
	cfg := fastRetry(3)
	cfg.MaxDelay = time.Second
	c := RetryChat(cfg)(fake)
	start := time.Now()
	if _, err := c.Complete(context.Background(), llms.ChatRequest{}); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if elapsed := time.Since(start); elapsed < retryAfter {
		t.Fatalf("retry ignored RetryAfter: waited %v, want >= %v", elapsed, retryAfter)
	}
}

func TestRetryAbortsOnContextCancel(t *testing.T) {
	fake := &testllm.FakeChatClient{
		Func: func(_ context.Context, _ llms.ChatRequest) (llms.ChatResponse, error) {
			return llms.ChatResponse{}, provErr(llms.ErrorKindUnavailable, 0)
		},
	}
	cfg := fastRetry(5)
	cfg.InitialDelay = 5 * time.Second // long; the test must end via cancel, not the timer
	cfg.MaxDelay = 5 * time.Second
	c := RetryChat(cfg)(fake)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()

	start := time.Now()
	_, err := c.Complete(ctx, llms.ChatRequest{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cancel during backoff should return promptly, took %v", elapsed)
	}
}

func TestRetryChatPassesThroughLimitError(t *testing.T) {
	var calls int32
	fake := &testllm.FakeChatClient{
		Func: func(_ context.Context, _ llms.ChatRequest) (llms.ChatResponse, error) {
			atomic.AddInt32(&calls, 1)
			return llms.ChatResponse{}, &llms.LimitError{Provider: "p", Model: "m"}
		},
	}
	c := RetryChat(fastRetry(3))(fake)
	_, err := c.Complete(context.Background(), llms.ChatRequest{})
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("LimitError is retryable; want 3 attempts, got %d", got)
	}
	var le *llms.LimitError
	if !errors.As(err, &le) {
		t.Fatalf("LimitError must pass through unwrapped, got %v", err)
	}
}

func TestRetryEmbeddingsRetriesThenSucceeds(t *testing.T) {
	var calls int32
	fake := &testllm.FakeEmbeddingClient{
		ModelRef: llms.ModelRef{Provider: "p", Name: "e"},
		Func: func(_ context.Context, _ llms.EmbeddingRequest) (llms.EmbeddingResponse, error) {
			if atomic.AddInt32(&calls, 1) < 2 {
				return llms.EmbeddingResponse{}, provErr(llms.ErrorKindTimeout, 0)
			}
			return llms.EmbeddingResponse{Vectors: []llms.EmbeddingVector{{ID: "a"}}}, nil
		},
	}
	c := RetryEmbeddings(fastRetry(4))(fake)
	resp, err := c.Embed(context.Background(), llms.EmbeddingRequest{})
	if err != nil {
		t.Fatalf("expected success after retry, got %v", err)
	}
	if len(resp.Vectors) != 1 {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("want 2 attempts, got %d", got)
	}
}

func TestRetryConfigNormalizedDefaults(t *testing.T) {
	d := DefaultRetryConfig()
	// Zero config = default backoff schedule, but jitter stays OFF (0 is an
	// explicit opt-out, not "unset"); everything else matches defaults.
	got := RetryConfig{}.normalized()
	want := d
	want.Jitter = 0
	if got != want {
		t.Fatalf("zero RetryConfig must normalize to defaults-with-jitter-off; got %+v", got)
	}
	// DefaultRetryConfig() round-trips through normalize unchanged.
	if d.normalized() != d {
		t.Fatalf("DefaultRetryConfig must be normalize-stable; got %+v", d.normalized())
	}
	// Negative jitter clamps to 0; other invalid numeric fields fall back.
	n := RetryConfig{MaxAttempts: -1, BackoffFactor: 0.5, Jitter: -1}.normalized()
	if n.MaxAttempts != d.MaxAttempts || n.BackoffFactor != d.BackoffFactor || n.Jitter != 0 {
		t.Fatalf("invalid fields not normalized: %+v", n)
	}

	// NaN/Inf must not slip through (NaN compares false to everything) and
	// must not propagate into the backoff math. Jitter > 1 clamps to 1.
	nan, inf := math.NaN(), math.Inf(1)
	for _, bf := range []float64{nan, inf, math.Inf(-1)} {
		if got := (RetryConfig{BackoffFactor: bf}).normalized().BackoffFactor; got != d.BackoffFactor {
			t.Fatalf("BackoffFactor %v must fall back to default, got %v", bf, got)
		}
	}
	for _, j := range []float64{nan, inf, math.Inf(-1)} {
		if got := (RetryConfig{Jitter: j}).normalized().Jitter; got != 0 {
			t.Fatalf("Jitter %v must sanitize to 0, got %v", j, got)
		}
	}
	if got := (RetryConfig{Jitter: 5}).normalized().Jitter; got != 1 {
		t.Fatalf("Jitter > 1 must clamp to 1, got %v", got)
	}
}

func TestJitterBounds(t *testing.T) {
	if got := jittered(100*time.Millisecond, 0); got != 100*time.Millisecond {
		t.Fatalf("zero jitter must be identity, got %v", got)
	}
	const base = 100 * time.Millisecond
	for range 1000 {
		j := jittered(base, 0.25)
		if j < 0 || j > time.Duration(1.25*float64(base)) {
			t.Fatalf("jitter out of [0, +25%%] bounds: %v", j)
		}
	}
}

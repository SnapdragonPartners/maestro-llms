package middleware

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/ratelimit"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"
)

// ADR-0033: the classification boundary between "worth retrying" and
// "evidence the provider is unhealthy". Limiter rejections (*llms.LimitError)
// never count against the breaker; only provider failures do. Retry retries
// only temporary rejections.

func boundaryCases() []struct {
	err         error
	name        string
	tripsBreak  bool
	retryable   bool
	matchesCtxE error
} {
	return []struct {
		err         error
		name        string
		tripsBreak  bool
		retryable   bool
		matchesCtxE error
	}{
		{name: "limit temporary (capacity exhausted / MaxWait)", err: &llms.LimitError{Reason: "wait exceeded MaxWait"}, retryable: true},
		{name: "limit impossible", err: &llms.LimitError{Reason: "can never fit", Permanent: true}},
		{name: "limit caller canceled", err: &llms.LimitError{Reason: "context done", Cause: context.Canceled}, matchesCtxE: context.Canceled},
		{name: "limit deadline (per-attempt or caller)", err: &llms.LimitError{Reason: "context done", Cause: context.DeadlineExceeded}, retryable: true, matchesCtxE: context.DeadlineExceeded},
		{name: "provider 429", err: &llms.ProviderError{Kind: llms.ErrorKindRateLimited}, retryable: true, tripsBreak: true},
		{name: "provider outage", err: &llms.ProviderError{Kind: llms.ErrorKindUnavailable}, retryable: true, tripsBreak: true},
	}
}

func TestCircuitLimitErrorsAreNeutral(t *testing.T) {
	const threshold = 3
	for _, tc := range boundaryCases() {
		t.Run(tc.name, func(t *testing.T) {
			sw := &errSwitch{err: tc.err}
			c := CircuitChat(fastCircuit(threshold, 1, time.Minute))(&testllm.FakeChatClient{Func: sw.fn()})
			for range threshold * 3 {
				_, err := c.Complete(context.Background(), llms.ChatRequest{})
				if errors.As(err, new(*CircuitOpenError)) {
					if !tc.tripsBreak {
						t.Fatalf("breaker opened on %v; limiter rejections must be neutral", tc.err)
					}
					return
				}
				if !errors.Is(err, tc.err) {
					t.Fatalf("error must pass through unchanged, got %v", err)
				}
			}
			if tc.tripsBreak {
				t.Fatalf("breaker never opened on provider failure %v", tc.err)
			}
		})
	}
}

func TestRetryLimitErrorClassification(t *testing.T) {
	const attempts = 4
	for _, tc := range boundaryCases() {
		t.Run(tc.name, func(t *testing.T) {
			sw := &errSwitch{err: tc.err}
			c := RetryChat(fastRetry(attempts))(&testllm.FakeChatClient{Func: sw.fn()})
			_, err := c.Complete(context.Background(), llms.ChatRequest{})
			want := int32(1)
			if tc.retryable {
				want = attempts
			}
			if got := atomic.LoadInt32(&sw.calls); got != want {
				t.Fatalf("%d attempts, want %d", got, want)
			}
			if tc.matchesCtxE != nil && !errors.Is(err, tc.matchesCtxE) {
				t.Fatalf("errors.Is(err, %v) = false: %v", tc.matchesCtxE, err)
			}
		})
	}
}

// A limiter rejection during a HalfOpen probe is neutral: it releases the
// probe gate without counting as a failure (the breaker doesn't reopen) or
// as a recovery success (the breaker doesn't close).
func TestCircuitHalfOpenLimitErrorIsNeitherFailureNorSuccess(t *testing.T) {
	const open = 20 * time.Millisecond
	sw := &errSwitch{err: retryableErr()}
	c := CircuitChat(fastCircuit(2, 1, open))(&testllm.FakeChatClient{Func: sw.fn()})
	for range 2 { // trip
		c.Complete(context.Background(), llms.ChatRequest{}) //nolint:errcheck // tripping failures
	}
	time.Sleep(open + 10*time.Millisecond)

	sw.set(&llms.LimitError{Reason: "wait exceeded MaxWait"})
	if _, err := c.Complete(context.Background(), llms.ChatRequest{}); !errors.As(err, new(*llms.LimitError)) {
		t.Fatalf("HalfOpen probe should reach the limiter and return its rejection, got %v", err)
	}
	// Not a failure: the gate is released and the breaker is still HalfOpen,
	// so the next call is admitted as a probe rather than rejected.
	sw.set(retryableErr())
	_, err := c.Complete(context.Background(), llms.ChatRequest{})
	if errors.As(err, new(*CircuitOpenError)) {
		t.Fatal("a LimitError probe must release the HalfOpen gate")
	}
	// Not a success either: had the LimitError closed the breaker
	// (SuccessThreshold 1), this one provider failure would leave it Closed
	// (FailureThreshold 2). Still HalfOpen, the failed probe reopens it.
	if _, err := c.Complete(context.Background(), llms.ChatRequest{}); !errors.As(err, new(*CircuitOpenError)) {
		t.Fatalf("a LimitError probe must not count as a recovery success; want reopened breaker, got %v", err)
	}
}

// End to end through RecommendedChat with a real limiter: one request that
// can never be admitted is not retried, never reaches the provider, and does
// not open the breaker for the next valid request. Before ADR-0033 the
// impossible request burned every attempt and tripped the breaker.
func TestRecommendedChatImpossibleRequestDoesNotTripCircuit(t *testing.T) {
	fake := &testllm.FakeChatClient{Text: "ok"}
	lim := ratelimit.NewInMemoryLimiter(ratelimit.Config{TokensPerMinute: 20_000})
	retry := fastRetry(5)
	c := RecommendedChat(fake, RecommendedConfig{
		Retry:   retry,
		Circuit: CircuitConfig{FailureThreshold: retry.MaxAttempts},
		Limiter: lim,
	})
	big := llms.ChatRequest{Messages: []llms.Message{{Role: llms.RoleUser, Content: []llms.ContentPart{llms.Text(strings.Repeat("x", 400_000))}}}}
	_, err := c.Complete(context.Background(), big)
	var le *llms.LimitError
	if !errors.As(err, &le) || !le.Permanent {
		t.Fatalf("want a Permanent LimitError, got %#v", err)
	}
	small := llms.ChatRequest{Messages: []llms.Message{{Role: llms.RoleUser, Content: []llms.ContentPart{llms.Text("hi")}}}}
	if _, err := c.Complete(context.Background(), small); err != nil {
		t.Fatalf("a valid request after an impossible one must succeed, got %v", err)
	}
	if n := len(fake.Calls()); n != 1 {
		t.Fatalf("provider called %d times, want 1 (only the valid request)", n)
	}
}

// Temporary exhaustion: repeated MaxWait rejections past the failure
// threshold leave the breaker closed, so the request after capacity frees
// reaches the provider.
func TestRecommendedChatTemporaryExhaustionDoesNotTripCircuit(t *testing.T) {
	fake := &testllm.FakeChatClient{Text: "ok"}
	lim := ratelimit.NewInMemoryLimiter(ratelimit.Config{MaxConcurrency: 1, MaxWait: 2 * time.Millisecond, PollInterval: time.Millisecond})
	held, err := lim.Reserve(context.Background(), ratelimit.ReservationRequest{})
	if err != nil {
		t.Fatal(err)
	}
	c := RecommendedChat(fake, RecommendedConfig{
		Retry:   fastRetry(2),
		Circuit: CircuitConfig{FailureThreshold: 2, OpenTimeout: time.Minute},
		Limiter: lim,
	})
	req := llms.ChatRequest{Messages: []llms.Message{{Role: llms.RoleUser, Content: []llms.ContentPart{llms.Text("hi")}}}}
	for range 3 { // 6 retryable rejections, 3x the threshold
		if _, err := c.Complete(context.Background(), req); !errors.As(err, new(*llms.LimitError)) {
			t.Fatalf("want a LimitError while the slot is held, got %v", err)
		}
	}
	if err := held.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Complete(context.Background(), req); err != nil {
		t.Fatalf("breaker must stay closed through limiter rejections, got %v", err)
	}
}

// heldLimiter returns a one-slot limiter whose slot is held until release
// after d, so the first waits on it end by their own context.
func heldLimiter(t *testing.T, d time.Duration) *ratelimit.InMemoryLimiter {
	t.Helper()
	lim := ratelimit.NewInMemoryLimiter(ratelimit.Config{MaxConcurrency: 1, PollInterval: time.Millisecond})
	held, err := lim.Reserve(context.Background(), ratelimit.ReservationRequest{})
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(d, func() { _ = held.Release(context.Background()) })
	return lim
}

// The timeout middleware runs inside retry, so the limiter waits on a
// per-attempt context. That attempt's deadline ending the wait must stay
// retryable: the caller is still active and the next attempt can be
// admitted (Codex PR 1 round 1; ADR-0033 Decision 4).
func TestRecommendedPerAttemptDeadlineInLimiterStillRetries(t *testing.T) {
	cfg := func(lim ratelimit.Limiter) RecommendedConfig {
		return RecommendedConfig{Retry: fastRetry(3), Timeout: 40 * time.Millisecond, Limiter: lim}
	}
	t.Run("chat", func(t *testing.T) {
		fake := &testllm.FakeChatClient{Text: "ok"}
		c := RecommendedChat(fake, cfg(heldLimiter(t, 60*time.Millisecond)))
		req := llms.ChatRequest{Messages: []llms.Message{{Role: llms.RoleUser, Content: []llms.ContentPart{llms.Text("hi")}}}}
		if _, err := c.Complete(context.Background(), req); err != nil {
			t.Fatalf("a per-attempt deadline in the limiter must be retried, got %v", err)
		}
		if n := len(fake.Calls()); n != 1 {
			t.Fatalf("provider called %d times, want 1", n)
		}
	})
	t.Run("embeddings", func(t *testing.T) {
		fake := &testllm.FakeEmbeddingClient{}
		c := RecommendedEmbeddings(fake, cfg(heldLimiter(t, 60*time.Millisecond)))
		req := llms.EmbeddingRequest{Inputs: []llms.EmbeddingInput{{ID: "a", Text: "hi"}}}
		if _, err := c.Embed(context.Background(), req); err != nil {
			t.Fatalf("a per-attempt deadline in the limiter must be retried, got %v", err)
		}
		if n := len(fake.Calls()); n != 1 {
			t.Fatalf("provider called %d times, want 1", n)
		}
	})
}

// The caller's own deadline ending the wait stops retry promptly on the
// caller's context, still matching context.DeadlineExceeded.
func TestRecommendedCallerDeadlineInLimiterStopsRetry(t *testing.T) {
	fake := &testllm.FakeChatClient{Text: "ok"}
	c := RecommendedChat(fake, RecommendedConfig{Retry: fastRetry(50), Limiter: heldLimiter(t, time.Hour)})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	req := llms.ChatRequest{Messages: []llms.Message{{Role: llms.RoleUser, Content: []llms.ContentPart{llms.Text("hi")}}}}
	_, err := c.Complete(ctx, req)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want an error matching context.DeadlineExceeded, got %v", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("retry kept going %v after the caller's deadline", el)
	}
	if n := len(fake.Calls()); n != 0 {
		t.Fatalf("provider called %d times, want 0", n)
	}
}

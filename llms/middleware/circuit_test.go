package middleware

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"
)

func fastCircuit(failT, succT int, open time.Duration) CircuitConfig {
	return CircuitConfig{FailureThreshold: failT, SuccessThreshold: succT, OpenTimeout: open}
}

// errSwitch lets a test flip the fake between failing and succeeding.
type errSwitch struct {
	mu    sync.Mutex
	err   error
	calls int32
}

func (s *errSwitch) set(err error) { s.mu.Lock(); s.err = err; s.mu.Unlock() }
func (s *errSwitch) fn() func(context.Context, llms.ChatRequest) (llms.ChatResponse, error) {
	return func(_ context.Context, _ llms.ChatRequest) (llms.ChatResponse, error) {
		atomic.AddInt32(&s.calls, 1)
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.err != nil {
			return llms.ChatResponse{}, s.err
		}
		return llms.ChatResponse{Text: "ok"}, nil
	}
}

func retryableErr() error {
	return &llms.ProviderError{Provider: "p", Model: "m", Kind: llms.ErrorKindUnavailable}
}

func TestCircuitTripsAfterThresholdAndRejects(t *testing.T) {
	sw := &errSwitch{err: retryableErr()}
	fake := &testllm.FakeChatClient{ModelRef: llms.ModelRef{Provider: "p", Name: "m"}, Func: sw.fn()}
	c := CircuitChat(fastCircuit(3, 2, time.Minute))(fake)

	for i := range 3 {
		if _, err := c.Complete(context.Background(), llms.ChatRequest{}); err == nil {
			t.Fatalf("call %d should fail", i)
		}
	}
	// Breaker is now open: next call rejected without touching the inner client.
	before := atomic.LoadInt32(&sw.calls)
	_, err := c.Complete(context.Background(), llms.ChatRequest{})
	var oe *CircuitOpenError
	if !errors.As(err, &oe) {
		t.Fatalf("want *CircuitOpenError when open, got %v", err)
	}
	if oe.Provider != "p" || oe.Model != "m" || oe.RetryAfter <= 0 {
		t.Fatalf("CircuitOpenError fields not populated: %+v", oe)
	}
	if atomic.LoadInt32(&sw.calls) != before {
		t.Fatal("open breaker must not call the inner client")
	}
}

func TestCircuitOpenErrorIsNotRetryable(t *testing.T) {
	if llms.Retryable(&CircuitOpenError{Provider: "p", Model: "m"}) {
		t.Fatal("CircuitOpenError must be non-retryable so retry fails fast")
	}
}

func TestCircuitNonRetryableErrorsAreNeutral(t *testing.T) {
	authErr := &llms.ProviderError{Provider: "p", Model: "m", Kind: llms.ErrorKindAuth}
	sw := &errSwitch{err: authErr}
	fake := &testllm.FakeChatClient{Func: sw.fn()}
	c := CircuitChat(fastCircuit(3, 2, time.Minute))(fake)

	// Far more than the threshold of non-retryable failures must NOT trip it.
	for range 10 {
		_, err := c.Complete(context.Background(), llms.ChatRequest{})
		var pe *llms.ProviderError
		if !errors.As(err, &pe) || pe.Kind != llms.ErrorKindAuth {
			t.Fatalf("auth error must pass through unwrapped, got %v", err)
		}
	}
	if atomic.LoadInt32(&sw.calls) != 10 {
		t.Fatalf("breaker tripped on neutral errors; inner called %d/10", sw.calls)
	}
}

func TestCircuitSuccessResetsClosedStreak(t *testing.T) {
	sw := &errSwitch{err: retryableErr()}
	fake := &testllm.FakeChatClient{Func: sw.fn()}
	c := CircuitChat(fastCircuit(3, 2, time.Minute))(fake)

	c.Complete(context.Background(), llms.ChatRequest{}) //nolint:errcheck // fail 1
	c.Complete(context.Background(), llms.ChatRequest{}) //nolint:errcheck // fail 2
	sw.set(nil)
	c.Complete(context.Background(), llms.ChatRequest{}) //nolint:errcheck // success resets streak
	sw.set(retryableErr())
	c.Complete(context.Background(), llms.ChatRequest{}) //nolint:errcheck // fail 1 again
	c.Complete(context.Background(), llms.ChatRequest{}) //nolint:errcheck // fail 2 again

	// Only 2 consecutive failures since the reset; breaker must still be closed.
	if _, err := c.Complete(context.Background(), llms.ChatRequest{}); err != nil {
		var oe *CircuitOpenError
		if errors.As(err, &oe) {
			t.Fatal("success must reset the failure streak; breaker tripped too early")
		}
	}
}

func TestCircuitHalfOpenRecoversAndReopens(t *testing.T) {
	const open = 30 * time.Millisecond
	sw := &errSwitch{err: retryableErr()}
	fake := &testllm.FakeChatClient{Func: sw.fn()}
	c := CircuitChat(fastCircuit(2, 2, open))(fake)

	// Trip it.
	c.Complete(context.Background(), llms.ChatRequest{}) //nolint:errcheck
	c.Complete(context.Background(), llms.ChatRequest{}) //nolint:errcheck
	if _, err := c.Complete(context.Background(), llms.ChatRequest{}); !errors.As(err, new(*CircuitOpenError)) {
		t.Fatalf("expected open, got %v", err)
	}

	time.Sleep(open + 10*time.Millisecond) // let it move to HalfOpen on next call

	// HalfOpen: a failing probe must reopen the breaker.
	if _, err := c.Complete(context.Background(), llms.ChatRequest{}); err == nil {
		t.Fatal("probe should have failed")
	}
	if _, err := c.Complete(context.Background(), llms.ChatRequest{}); !errors.As(err, new(*CircuitOpenError)) {
		t.Fatalf("failed probe must reopen, got %v", err)
	}

	// Wait again, then succeed SuccessThreshold times to close.
	time.Sleep(open + 10*time.Millisecond)
	sw.set(nil)
	for i := range 2 {
		if _, err := c.Complete(context.Background(), llms.ChatRequest{}); err != nil {
			t.Fatalf("recovery success %d failed: %v", i, err)
		}
	}
	// Closed again: a fresh failure should not immediately reject.
	sw.set(retryableErr())
	if _, err := c.Complete(context.Background(), llms.ChatRequest{}); errors.As(err, new(*CircuitOpenError)) {
		t.Fatal("breaker should be closed after recovery")
	}
}

// Composition: retry is OUTSIDE circuit. Once the breaker opens mid-retry,
// CircuitOpenError is non-retryable so retry stops immediately — the inner
// client is hit exactly FailureThreshold times, not MaxAttempts.
func TestCircuitFailsFastUnderRetry(t *testing.T) {
	sw := &errSwitch{err: retryableErr()}
	fake := &testllm.FakeChatClient{Func: sw.fn()}
	c := RetryChat(fastRetry(20))(CircuitChat(fastCircuit(3, 2, time.Minute))(fake))

	_, err := c.Complete(context.Background(), llms.ChatRequest{})
	if !errors.As(err, new(*CircuitOpenError)) {
		t.Fatalf("retry should surface the open-circuit error, got %v", err)
	}
	if got := atomic.LoadInt32(&sw.calls); got != 3 {
		t.Fatalf("fail-fast: inner client must be called exactly FailureThreshold=3, got %d", got)
	}
}

// HalfOpen must admit only ONE probe: while it is in flight, concurrent
// callers are rejected so a recovering provider sees no thundering herd.
func TestCircuitHalfOpenSingleFlight(t *testing.T) {
	const open = 20 * time.Millisecond
	var mode atomic.Int32 // 0 = fail, 1 = probe (signals start, then blocks)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	fake := &testllm.FakeChatClient{
		Func: func(_ context.Context, _ llms.ChatRequest) (llms.ChatResponse, error) {
			if mode.Load() == 0 {
				return llms.ChatResponse{}, retryableErr()
			}
			started <- struct{}{}
			<-release
			return llms.ChatResponse{Text: "ok"}, nil
		},
	}
	c := CircuitChat(fastCircuit(1, 1, open))(fake)

	if _, err := c.Complete(context.Background(), llms.ChatRequest{}); err == nil {
		t.Fatal("expected the tripping failure")
	}
	mode.Store(1)
	time.Sleep(open + 10*time.Millisecond) // now eligible for a HalfOpen probe

	var wg sync.WaitGroup
	probeDone := make(chan error, 1)
	wg.Go(func() {
		_, err := c.Complete(context.Background(), llms.ChatRequest{})
		probeDone <- err
	})
	<-started // the single probe is now in flight, holding the HalfOpen slot

	var rejected int32
	for range 8 {
		wg.Go(func() {
			if _, err := c.Complete(context.Background(), llms.ChatRequest{}); errors.As(err, new(*CircuitOpenError)) {
				atomic.AddInt32(&rejected, 1)
			}
		})
	}
	time.Sleep(25 * time.Millisecond) // contenders are rejected synchronously in allow()
	if got := atomic.LoadInt32(&rejected); got != 8 {
		t.Fatalf("single-flight: all 8 concurrent callers must be rejected while a probe is in flight, got %d", got)
	}

	close(release)
	if err := <-probeDone; err != nil {
		t.Fatalf("probe should succeed: %v", err)
	}
	wg.Wait()

	// SuccessThreshold=1 → breaker closed; a subsequent call proceeds.
	if _, err := c.Complete(context.Background(), llms.ChatRequest{}); err != nil {
		t.Fatalf("breaker should be closed after the successful probe: %v", err)
	}
}

func TestCircuitEmbeddingsTrips(t *testing.T) {
	var calls int32
	fake := &testllm.FakeEmbeddingClient{
		ModelRef: llms.ModelRef{Provider: "p", Name: "e"},
		Func: func(_ context.Context, _ llms.EmbeddingRequest) (llms.EmbeddingResponse, error) {
			atomic.AddInt32(&calls, 1)
			return llms.EmbeddingResponse{}, &llms.ProviderError{Kind: llms.ErrorKindUnavailable}
		},
	}
	c := CircuitEmbeddings(fastCircuit(2, 2, time.Minute))(fake)
	c.Embed(context.Background(), llms.EmbeddingRequest{}) //nolint:errcheck
	c.Embed(context.Background(), llms.EmbeddingRequest{}) //nolint:errcheck
	if _, err := c.Embed(context.Background(), llms.EmbeddingRequest{}); !errors.As(err, new(*CircuitOpenError)) {
		t.Fatalf("embeddings breaker should be open, got %v", err)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("open breaker must not call inner; calls=%d", calls)
	}
}

func TestCircuitConfigNormalizedDefaults(t *testing.T) {
	if got := (CircuitConfig{}).normalized(); got != DefaultCircuitConfig() {
		t.Fatalf("zero CircuitConfig must normalize to defaults, got %+v", got)
	}
	n := CircuitConfig{FailureThreshold: -1, SuccessThreshold: 0, OpenTimeout: -1}.normalized()
	if n != DefaultCircuitConfig() {
		t.Fatalf("invalid fields not normalized: %+v", n)
	}
}

func TestCircuitConcurrentUseRaceClean(t *testing.T) {
	sw := &errSwitch{err: retryableErr()}
	fake := &testllm.FakeChatClient{Func: sw.fn()}
	c := CircuitChat(fastCircuit(5, 3, 10*time.Millisecond))(fake)

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			for range 20 {
				c.Complete(context.Background(), llms.ChatRequest{}) //nolint:errcheck // exercising state under -race
			}
		})
	}
	wg.Wait()

	// Still usable after concurrent hammering, returning a typed outcome
	// (either the open-circuit sentinel or the retryable provider error).
	_, err := c.Complete(context.Background(), llms.ChatRequest{})
	if err != nil && !errors.As(err, new(*CircuitOpenError)) && !llms.Retryable(err) {
		t.Fatalf("unexpected error type after concurrent use: %v", err)
	}
}

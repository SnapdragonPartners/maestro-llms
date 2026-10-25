package middleware

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/ratelimit"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"
)

// callKey is the consumer-owned logical-call key a test puts in ctx.
type callKey struct{}

func callCtx(id string) context.Context {
	return context.WithValue(context.Background(), callKey{}, id)
}

func callID(ctx context.Context) string {
	id, _ := ctx.Value(callKey{}).(string)
	return id
}

// attemptRec is what ctxRecorder keeps from each ObserveContext.
type attemptRec struct {
	err         error
	call        string
	attempt     int
	hasAttempt  bool
	hasDeadline bool
}

// ctxRecorder is a concurrency-safe ContextObserver. Observe must never be
// called on it; plain counts any call that is.
type ctxRecorder struct {
	recs  []attemptRec
	mu    sync.Mutex
	plain atomic.Int32
}

func (r *ctxRecorder) Observe(Event) { r.plain.Add(1) }

func (r *ctxRecorder) ObserveContext(ctx context.Context, ev Event) {
	n, ok := AttemptFromContext(ctx)
	_, hasDeadline := ctx.Deadline()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recs = append(r.recs, attemptRec{
		err: ev.Err, call: callID(ctx), attempt: n, hasAttempt: ok, hasDeadline: hasDeadline,
	})
}

func (r *ctxRecorder) snapshot() []attemptRec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.recs)
}

func attemptsOf(recs []attemptRec) []int {
	out := make([]int, 0, len(recs))
	for i := range recs {
		out = append(out, recs[i].attempt)
	}
	return out
}

// failFirst returns a fake Func that fails the first n calls with a
// retryable unavailable error and then succeeds.
func failFirst(n int32) func(context.Context, llms.ChatRequest) (llms.ChatResponse, error) {
	var calls atomic.Int32
	return func(_ context.Context, _ llms.ChatRequest) (llms.ChatResponse, error) {
		if calls.Add(1) <= n {
			return llms.ChatResponse{}, provErr(llms.ErrorKindUnavailable, 0)
		}
		return llms.ChatResponse{Text: "ok"}, nil
	}
}

// scriptedLimiter admits every Reserve except the 1-based call numbers in
// reject, which get a retryable *llms.LimitError.
type scriptedLimiter struct {
	reject map[int]bool
	mu     sync.Mutex
	n      int
}

func (l *scriptedLimiter) Reserve(context.Context, ratelimit.ReservationRequest) (ratelimit.Reservation, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.n++
	if l.reject[l.n] {
		return nil, &llms.LimitError{Reason: "scripted"}
	}
	return noopReservation{}, nil
}

type noopReservation struct{}

func (noopReservation) Commit(context.Context, llms.Usage) error { return nil }
func (noopReservation) Release(context.Context) error            { return nil }

func TestContextObserverSeesEachAttemptWithCallerContext(t *testing.T) {
	rec := &ctxRecorder{}
	base := &testllm.FakeChatClient{ModelRef: llms.ModelRef{Provider: "p", Name: "m"}, Func: failFirst(2)}
	c := RecommendedChat(base, RecommendedConfig{Retry: fastRetry(5), Timeout: time.Minute, Observer: rec})

	resp, err := c.Complete(callCtx("c1"), userReq("hi"))
	if err != nil || resp.Text != "ok" {
		t.Fatalf("want success after 2 retries, got %q / %v", resp.Text, err)
	}
	recs := rec.snapshot()
	if got := attemptsOf(recs); !slices.Equal(got, []int{1, 2, 3}) {
		t.Fatalf("want attempts [1 2 3], got %v", got)
	}
	for i := range recs {
		r := &recs[i]
		if !r.hasAttempt || r.call != "c1" {
			t.Fatalf("attempt %d: want call c1 with an attempt number, got %+v", i+1, *r)
		}
		// The observer gets the attempt's own ctx (inside the per-attempt
		// timeout), not the logical call's, which has no deadline.
		if !r.hasDeadline {
			t.Fatalf("attempt %d: want the per-attempt ctx (with deadline)", i+1)
		}
		if wantErr := i < 2; (r.err != nil) != wantErr {
			t.Fatalf("attempt %d: err = %v, want error: %v", i+1, r.err, wantErr)
		}
	}
	if !errors.As(recs[0].err, new(*llms.ProviderError)) {
		t.Fatalf("want the attempt's *llms.ProviderError, got %v", recs[0].err)
	}
	if n := rec.plain.Load(); n != 0 {
		t.Fatalf("a ContextObserver must get ObserveContext only, but Observe was called %d times", n)
	}
}

func TestContextObserverAttemptGapOnLimiterRejection(t *testing.T) {
	rec := &ctxRecorder{}
	base := &testllm.FakeChatClient{ModelRef: llms.ModelRef{Provider: "p", Name: "m"}, Func: failFirst(1)}
	// Attempt 1 reaches the provider and fails; attempt 2 is rejected by the
	// limiter (retryable, so it consumes an attempt); attempt 3 succeeds.
	lim := &scriptedLimiter{reject: map[int]bool{2: true}}
	c := RecommendedChat(base, RecommendedConfig{Retry: fastRetry(5), Limiter: lim, Observer: rec})

	if _, err := c.Complete(callCtx("c1"), userReq("hi")); err != nil {
		t.Fatalf("want success, got %v", err)
	}
	if n := len(base.Calls()); n != 2 {
		t.Fatalf("want 2 provider calls, got %d", n)
	}
	if got := attemptsOf(rec.snapshot()); !slices.Equal(got, []int{1, 3}) {
		t.Fatalf("want the true retry numbers [1 3] (attempt 2 never reached the observer), got %v", got)
	}
}

func TestContextObserverDoesNotSeeOuterRejections(t *testing.T) {
	rec := &ctxRecorder{}
	base := &testllm.FakeChatClient{
		ModelRef: llms.ModelRef{Provider: "p", Name: "m"},
		Err:      provErr(llms.ErrorKindUnavailable, 0),
	}
	c := RecommendedChat(base, RecommendedConfig{
		Retry: fastRetry(3), Circuit: fastCircuit(1, 1, time.Hour), Observer: rec,
	})

	// Attempt 1 fails and trips the breaker; attempt 2 fast-fails with the
	// non-retryable *CircuitOpenError before reaching metrics.
	_, err := c.Complete(callCtx("c1"), userReq("hi"))
	if !errors.As(err, new(*CircuitOpenError)) {
		t.Fatalf("want *CircuitOpenError, got %v", err)
	}
	// A malformed request is rejected by validation, outermost.
	if _, err := c.Complete(callCtx("c2"), llms.ChatRequest{}); !errors.As(err, new(*ValidationError)) {
		t.Fatalf("want *ValidationError, got %v", err)
	}
	recs := rec.snapshot()
	if len(recs) != 1 || recs[0].call != "c1" || recs[0].attempt != 1 {
		t.Fatalf("want only c1's provider attempt 1, got %+v", recs)
	}
}

func TestContextObserverConcurrentCallsDoNotCross(t *testing.T) {
	rec := &ctxRecorder{}
	var seen sync.Map
	base := &testllm.FakeChatClient{
		ModelRef: llms.ModelRef{Provider: "p", Name: "m"},
		// Each logical call fails its first attempt.
		Func: func(ctx context.Context, _ llms.ChatRequest) (llms.ChatResponse, error) {
			if _, loaded := seen.LoadOrStore(callID(ctx), true); !loaded {
				return llms.ChatResponse{}, provErr(llms.ErrorKindUnavailable, 0)
			}
			return llms.ChatResponse{Text: "ok"}, nil
		},
	}
	// Every call fails once at about the same time; keep the breaker out of
	// the way so all of them reach their second attempt.
	c := RecommendedChat(base, RecommendedConfig{
		Retry: fastRetry(3), Circuit: fastCircuit(1000, 1, time.Second), Observer: rec,
	})

	const calls = 16
	var wg sync.WaitGroup
	for i := range calls {
		wg.Go(func() {
			if _, err := c.Complete(callCtx(fmt.Sprintf("c%d", i)), userReq("hi")); err != nil {
				t.Errorf("call %d: %v", i, err)
			}
		})
	}
	wg.Wait()

	byCall := map[string][]int{}
	for _, r := range rec.snapshot() {
		byCall[r.call] = append(byCall[r.call], r.attempt)
	}
	if len(byCall) != calls {
		t.Fatalf("want %d logical calls, got %d: %v", calls, len(byCall), byCall)
	}
	for id, attempts := range byCall {
		if !slices.Equal(attempts, []int{1, 2}) {
			t.Fatalf("%s: want attempts [1 2], got %v", id, attempts)
		}
	}
}

func TestAttemptFromContextAbsentOutsideRetry(t *testing.T) {
	rec := &ctxRecorder{}
	base := &testllm.FakeChatClient{ModelRef: llms.ModelRef{Provider: "p", Name: "m"}, Func: failFirst(1)}
	// Metrics outside retry observes the logical call, which has no attempt.
	c := ChainChat(base, MetricsChat(rec), RetryChat(fastRetry(3)))

	if _, err := c.Complete(callCtx("c1"), userReq("hi")); err != nil {
		t.Fatalf("want success, got %v", err)
	}
	recs := rec.snapshot()
	if len(recs) != 1 || recs[0].hasAttempt || recs[0].call != "c1" {
		t.Fatalf("want one logical-call event without an attempt number, got %+v", recs)
	}
	if _, ok := AttemptFromContext(context.Background()); ok {
		t.Fatal("a bare context must not report an attempt")
	}
}

func TestContextObserverEmbeddingsSeesEachAttempt(t *testing.T) {
	rec := &ctxRecorder{}
	var calls atomic.Int32
	base := &testllm.FakeEmbeddingClient{
		ModelRef: llms.ModelRef{Provider: "p", Name: "e"},
		Func: func(_ context.Context, _ llms.EmbeddingRequest) (llms.EmbeddingResponse, error) {
			if calls.Add(1) == 1 {
				return llms.EmbeddingResponse{}, provErr(llms.ErrorKindUnavailable, 0)
			}
			return llms.EmbeddingResponse{}, nil
		},
	}
	c := RecommendedEmbeddings(base, RecommendedConfig{Retry: fastRetry(3), Observer: rec})

	if _, err := c.Embed(callCtx("e1"), llms.EmbeddingRequest{Inputs: []llms.EmbeddingInput{{ID: "1", Text: "x"}}}); err != nil {
		t.Fatalf("want success, got %v", err)
	}
	recs := rec.snapshot()
	if got := attemptsOf(recs); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("want attempts [1 2], got %v", got)
	}
	if recs[0].call != "e1" || recs[1].call != "e1" {
		t.Fatalf("want both attempts tied to e1, got %+v", recs)
	}
}

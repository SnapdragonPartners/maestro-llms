package middleware_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/middleware"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"
)

// callIDKey is the application's own logical-call key; the toolkit
// generates no IDs.
type callIDKey struct{}

// attemptRecord is one provider attempt. It keeps the error's kind, not its
// text, which can contain request content.
type attemptRecord struct {
	CallID       string `json:"call_id"`
	Model        string `json:"model"`
	ErrorKind    string `json:"error_kind,omitempty"`
	Attempt      int    `json:"attempt"`
	OutputTokens int    `json:"output_tokens"`
	LatencyMS    int64  `json:"latency_ms"`
}

// callRecord is one logical call. OtherMS is the logical latency the
// attempts don't account for: backoff, limiter waits, and overhead.
type callRecord struct {
	CallID         string `json:"call_id"`
	ErrorKind      string `json:"error_kind,omitempty"`
	Attempts       int    `json:"attempts"`
	LatencyMS      int64  `json:"latency_ms"`
	ProviderTimeMS int64  `json:"provider_time_ms"`
	OtherMS        int64  `json:"other_ms"`
}

// sink collects records; a real one writes them to the app's store.
type sink struct {
	attempts map[string][]attemptRecord
	calls    []callRecord
	mu       sync.Mutex
}

// ObserveContext makes sink a middleware.ContextObserver.
func (s *sink) ObserveContext(ctx context.Context, ev middleware.Event) {
	id, _ := ctx.Value(callIDKey{}).(string)
	n, _ := middleware.AttemptFromContext(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts[id] = append(s.attempts[id], attemptRecord{
		CallID: id, Model: ev.Model, ErrorKind: errorKind(ev.Err), Attempt: n,
		OutputTokens: ev.Usage.OutputTokens, LatencyMS: ev.Latency.Milliseconds(),
	})
}

// Observe is required by middleware.Observer; with ObserveContext present
// the metrics middleware never calls it.
func (s *sink) Observe(middleware.Event) {}

func errorKind(err error) string {
	var pe *llms.ProviderError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &pe):
		return string(pe.Kind)
	default:
		return "other"
	}
}

// capture is the application's decorator around the recommended chain: it
// tags each logical call and records it after the call returns.
type capture struct {
	next llms.ChatClient
	sink *sink
	seq  atomic.Int64
}

func (c *capture) Model() llms.ModelRef { return c.next.Model() }

func (c *capture) Complete(ctx context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
	id := fmt.Sprintf("call-%d", c.seq.Add(1))
	start := time.Now()
	resp, err := c.next.Complete(context.WithValue(ctx, callIDKey{}, id), req)
	latency := time.Since(start).Milliseconds()

	c.sink.mu.Lock()
	defer c.sink.mu.Unlock()
	var provider int64
	for _, a := range c.sink.attempts[id] {
		provider += a.LatencyMS
	}
	c.sink.calls = append(c.sink.calls, callRecord{
		CallID: id, ErrorKind: errorKind(err), Attempts: len(c.sink.attempts[id]),
		LatencyMS: latency, ProviderTimeMS: provider, OtherMS: latency - provider,
	})
	return resp, err
}

// ExampleContextObserver records each provider attempt and each logical call
// as JSON-ready records, correlated through the application's ctx key.
func ExampleContextObserver() {
	var calls atomic.Int32
	fake := &testllm.FakeChatClient{
		ModelRef: llms.ModelRef{Provider: "fake", Name: "fake-1"},
		// The first attempt hits a retryable outage; the retry succeeds.
		Func: func(context.Context, llms.ChatRequest) (llms.ChatResponse, error) {
			if calls.Add(1) == 1 {
				return llms.ChatResponse{}, &llms.ProviderError{Kind: llms.ErrorKindUnavailable}
			}
			return llms.ChatResponse{Text: "ok", Usage: llms.Usage{OutputTokens: 7}}, nil
		},
	}
	s := &sink{attempts: map[string][]attemptRecord{}}
	client := &capture{
		next: middleware.RecommendedChat(fake, middleware.RecommendedConfig{
			Retry:    middleware.RetryConfig{MaxAttempts: 3, InitialDelay: time.Millisecond},
			Observer: s,
		}),
		sink: s,
	}

	if _, err := client.Complete(context.Background(), llms.ChatRequest{
		Messages: []llms.Message{llms.UserText("hi")},
	}); err != nil {
		fmt.Println("error:", err)
		return
	}

	// Timings vary from run to run, so clear them before printing.
	for _, a := range s.attempts["call-1"] {
		a.LatencyMS = 0
		out, _ := json.Marshal(a)
		fmt.Println(string(out))
	}
	for _, c := range s.calls {
		c.LatencyMS, c.ProviderTimeMS, c.OtherMS = 0, 0, 0
		out, _ := json.Marshal(c)
		fmt.Println(string(out))
	}
	// Output:
	// {"call_id":"call-1","model":"fake-1","error_kind":"unavailable","attempt":1,"output_tokens":0,"latency_ms":0}
	// {"call_id":"call-1","model":"fake-1","attempt":2,"output_tokens":7,"latency_ms":0}
	// {"call_id":"call-1","attempts":2,"latency_ms":0,"provider_time_ms":0,"other_ms":0}
}

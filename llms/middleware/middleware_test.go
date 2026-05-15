package middleware

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/llms"
	"github.com/SnapdragonPartners/maestro-llms/llms/ratelimit"
	"github.com/SnapdragonPartners/maestro-llms/llms/testllm"
)

var (
	_ llms.ChatClient      = (*rlChat)(nil)
	_ llms.EmbeddingClient = (*rlEmbedding)(nil)
	_ TokenEstimator       = DefaultEstimator{}
)

// frozenClock yields a constant time so the in-memory limiter never refills
// mid-test, making token assertions exact.
func frozenClock() func() time.Time {
	t := time.Unix(0, 0)
	return func() time.Time { return t }
}

func userReq(s string) llms.ChatRequest {
	return llms.ChatRequest{Messages: []llms.Message{llms.UserText(s)}}
}

func TestChainChatOrderingOutermostFirst(t *testing.T) {
	var order []string
	mw := func(name string) ChatMiddleware {
		return func(next llms.ChatClient) llms.ChatClient {
			return &recordChat{name: name, next: next, log: &order}
		}
	}
	base := &testllm.FakeChatClient{Text: "ok"}
	c := ChainChat(base, mw("A"), mw("B"), mw("C"))
	if _, err := c.Complete(context.Background(), llms.ChatRequest{}); err != nil {
		t.Fatal(err)
	}
	got := order
	want := []string{"A", "B", "C"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("call order = %v, want %v (A is outermost)", got, want)
	}
}

type recordChat struct {
	name string
	next llms.ChatClient
	log  *[]string
}

func (r *recordChat) Model() llms.ModelRef { return r.next.Model() }
func (r *recordChat) Complete(ctx context.Context, req llms.ChatRequest) (llms.ChatResponse, error) {
	*r.log = append(*r.log, r.name)
	return r.next.Complete(ctx, req)
}

func TestChainEmbeddingsOrdering(t *testing.T) {
	var order []string
	mw := func(name string) EmbeddingMiddleware {
		return func(next llms.EmbeddingClient) llms.EmbeddingClient {
			return &recordEmb{name: name, next: next, log: &order}
		}
	}
	base := &testllm.FakeEmbeddingClient{Dims: 4}
	c := ChainEmbeddings(base, mw("X"), mw("Y"))
	if _, err := c.Embed(context.Background(), llms.EmbeddingRequest{
		Inputs: []llms.EmbeddingInput{{ID: "1", Text: "a"}},
	}); err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != "X" || order[1] != "Y" {
		t.Fatalf("order = %v, want [X Y]", order)
	}
}

type recordEmb struct {
	name string
	next llms.EmbeddingClient
	log  *[]string
}

func (r *recordEmb) Model() llms.ModelRef   { return r.next.Model() }
func (r *recordEmb) DefaultDimensions() int { return r.next.DefaultDimensions() }
func (r *recordEmb) Embed(ctx context.Context, req llms.EmbeddingRequest) (llms.EmbeddingResponse, error) {
	*r.log = append(*r.log, r.name)
	return r.next.Embed(ctx, req)
}

func TestRateLimitChatReservesCommitsReleases(t *testing.T) {
	lim := ratelimit.NewInMemoryLimiter(ratelimit.Config{
		TokensPerMinute: 100_000, MaxConcurrency: 2, Clock: frozenClock(),
	})
	// capacity = 90_000.
	fake := &testllm.FakeChatClient{
		ModelRef:  llms.ModelRef{Provider: "anthropic", Name: "claude"},
		Responses: []llms.ChatResponse{{Message: llms.AssistantText("hi"), Text: "hi", Usage: llms.Usage{TotalTokens: 250}}},
	}
	c := RateLimitChat(lim, DefaultEstimator{})(fake)

	if _, err := c.Complete(context.Background(), userReq("hello there")); err != nil {
		t.Fatalf("complete: %v", err)
	}
	st, _ := lim.Stats(context.Background())
	// After reserve(est) + commit reconciling to actual 250, bucket = cap-250.
	if st.AvailableTokens != 90_000-250 {
		t.Fatalf("expected bucket reconciled to actual usage, got %d", st.AvailableTokens)
	}
	if st.ActiveRequests != 0 {
		t.Fatalf("Release did not free the slot: active=%d", st.ActiveRequests)
	}
}

func TestRateLimitChatLimitErrorSkipsInnerCall(t *testing.T) {
	lim := ratelimit.NewInMemoryLimiter(ratelimit.Config{TokensPerMinute: 100}) // cap 90
	fake := &testllm.FakeChatClient{Text: "should not be called"}
	c := RateLimitChat(lim, DefaultEstimator{})(fake)

	// A long prompt estimates well over 90 tokens -> impossible -> LimitError.
	big := ""
	for range 500 {
		big += "word "
	}
	_, err := c.Complete(context.Background(), userReq(big))
	var le *llms.LimitError
	if !errors.As(err, &le) {
		t.Fatalf("want *llms.LimitError, got %v", err)
	}
	if len(fake.Calls()) != 0 {
		t.Fatalf("inner client was called despite limiter rejection: %d calls", len(fake.Calls()))
	}
}

func TestRateLimitChatReleasesAfterContextCancel(t *testing.T) {
	lim := ratelimit.NewInMemoryLimiter(ratelimit.Config{MaxConcurrency: 1}) // token-unlimited
	fake := &testllm.FakeChatClient{Err: context.Canceled}
	c := RateLimitChat(lim, DefaultEstimator{})(fake)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Complete(ctx, llms.ChatRequest{}); err == nil {
		t.Fatal("expected error from canceled inner call")
	}
	// Release must have run on a cancellation-surviving context.
	st, _ := lim.Stats(context.Background())
	if st.ActiveRequests != 0 {
		t.Fatalf("slot leaked after ctx cancel: active=%d", st.ActiveRequests)
	}
	// Slot is free, so a subsequent reservation succeeds.
	fake2 := &testllm.FakeChatClient{Text: "ok"}
	if _, err := RateLimitChat(lim, DefaultEstimator{})(fake2).Complete(context.Background(), llms.ChatRequest{}); err != nil {
		t.Fatalf("slot not actually freed: %v", err)
	}
}

func TestRateLimitEmbeddingsHappyAndReject(t *testing.T) {
	lim := ratelimit.NewInMemoryLimiter(ratelimit.Config{TokensPerMinute: 100_000, MaxConcurrency: 1, Clock: frozenClock()})
	fake := &testllm.FakeEmbeddingClient{Dims: 4, Usage: llms.Usage{EmbeddingTokens: 12}}
	c := RateLimitEmbeddings(lim, DefaultEstimator{})(fake)
	resp, err := c.Embed(context.Background(), llms.EmbeddingRequest{
		Inputs: []llms.EmbeddingInput{{ID: "1", Text: "hello"}},
	})
	if err != nil || len(resp.Vectors) != 1 {
		t.Fatalf("embed failed: %v resp=%+v", err, resp)
	}
	st, _ := lim.Stats(context.Background())
	if st.AvailableTokens != 90_000-12 || st.ActiveRequests != 0 {
		t.Fatalf("embedding reconcile/release wrong: %+v", st)
	}
}

func TestDefaultEstimator(t *testing.T) {
	short := DefaultEstimator{}.EstimateChat(userReq("hi"))
	long := DefaultEstimator{}.EstimateChat(userReq("hi there this is a much longer prompt"))
	if short.InputTokens <= 0 || long.InputTokens <= short.InputTokens {
		t.Fatalf("estimate should grow with text: short=%d long=%d", short.InputTokens, long.InputTokens)
	}
	if short.Requests != 1 || short.TotalTokensMax < short.InputTokens {
		t.Fatalf("bad units: %+v", short)
	}
	emb := DefaultEstimator{}.EstimateEmbeddings(llms.EmbeddingRequest{
		Inputs: []llms.EmbeddingInput{{Text: "alpha"}, {Text: "beta"}},
	})
	if emb.InputTokens <= 0 || emb.OutputTokensMax != 0 || emb.Requests != 1 {
		t.Fatalf("bad embedding units: %+v", emb)
	}
}

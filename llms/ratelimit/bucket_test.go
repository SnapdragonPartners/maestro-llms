package ratelimit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/bucket"
	"github.com/SnapdragonPartners/maestro-llms/bucket/memory"
	"github.com/SnapdragonPartners/maestro-llms/llms"
)

// recordingBucket records what the adapter asks of the bucket.
type recordingBucket struct {
	err      error
	lease    *recordingLease
	key      string
	tokens   int
	acquires int
}

func (b *recordingBucket) Acquire(_ context.Context, key string, tokens int) (bucket.Lease, error) {
	b.acquires++
	b.key, b.tokens = key, tokens
	if b.err != nil {
		return nil, b.err
	}
	b.lease = &recordingLease{}
	return b.lease, nil
}

type recordingLease struct {
	releaseErr error
	adjusted   int
	releases   int
}

func (l *recordingLease) Adjust(_ context.Context, actual int) error { l.adjusted = actual; return nil }
func (l *recordingLease) Release(context.Context) error              { l.releases++; return l.releaseErr }
func (l *recordingLease) Lost() <-chan struct{}                      { return nil }

func bucketReq(units UsageUnits) ReservationRequest {
	return ReservationRequest{Provider: llms.ProviderAnthropic, Model: "m", Operation: OperationChat, EstimatedUnits: units}
}

func TestFromBucketMapsCalls(t *testing.T) {
	b := &recordingBucket{}
	lim := FromBucket(b, func(r ReservationRequest) string { return string(r.Provider) + "/" + r.Model })
	ctx := context.Background()
	res, err := lim.Reserve(ctx, bucketReq(UsageUnits{InputTokens: 100, OutputTokensMax: 50}))
	if err != nil {
		t.Fatal(err)
	}
	if b.key != "anthropic/m" || b.tokens != 150 {
		t.Fatalf("Acquire(%q, %d), want (anthropic/m, 150)", b.key, b.tokens)
	}
	_ = res.Commit(ctx, llms.Usage{InputTokens: 90, OutputTokens: 20})
	if b.lease.adjusted != 110 {
		t.Fatalf("Adjust(%d), want 110", b.lease.adjusted)
	}
	b.lease.releaseErr = bucket.ErrLeaseLost
	if err := res.Release(ctx); !errors.Is(err, bucket.ErrLeaseLost) || b.lease.releases != 1 {
		t.Fatalf("Release = %v (%d calls)", err, b.lease.releases)
	}
}

func TestFromBucketRejectsWeightedSlotsBeforeAcquiring(t *testing.T) {
	b := &recordingBucket{}
	lim := FromBucket(b, func(ReservationRequest) string { return "k" })
	_, err := lim.Reserve(context.Background(), bucketReq(UsageUnits{Requests: 2}))
	if err == nil || b.acquires != 0 {
		t.Fatalf("Reserve(Requests: 2) = %v after %d acquires; want an error before acquiring", err, b.acquires)
	}
	var le *llms.LimitError
	if errors.As(err, &le) || llms.Retryable(err) {
		t.Fatalf("weighted rejection must not be a retryable LimitError: %v", err)
	}
}

func TestFromBucketMapsLimitError(t *testing.T) {
	b := &recordingBucket{err: &bucket.LimitError{Key: "k", Reason: "wait exceeded MaxWait", RetryAfter: 3 * time.Second}}
	lim := FromBucket(b, func(ReservationRequest) string { return "k" })
	_, err := lim.Reserve(context.Background(), bucketReq(UsageUnits{InputTokens: 1}))
	var le *llms.LimitError
	if !errors.As(err, &le) || le.Provider != llms.ProviderAnthropic || le.Model != "m" || le.RetryAfter != 3*time.Second {
		t.Fatalf("LimitError not mapped: %#v", err)
	}
	if le.Permanent || le.Cause != nil || !llms.Retryable(err) {
		t.Fatalf("a temporary bucket rejection must stay retryable: %#v", le)
	}

	// Permanent and Cause carry across (ADR-0033).
	b.err = &bucket.LimitError{Key: "k", Reason: "can never fit", Permanent: true}
	_, err = lim.Reserve(context.Background(), bucketReq(UsageUnits{InputTokens: 1}))
	if !errors.As(err, &le) || !le.Permanent || llms.Retryable(err) {
		t.Fatalf("Permanent not mapped: %#v", err)
	}
	b.err = &bucket.LimitError{Key: "k", Reason: "context done", Cause: context.Canceled}
	_, err = lim.Reserve(context.Background(), bucketReq(UsageUnits{InputTokens: 1}))
	if !errors.As(err, &le) || !errors.Is(err, context.Canceled) || llms.Retryable(err) {
		t.Fatalf("Cause not mapped: %#v", err)
	}

	boom := errors.New("db down")
	b.err = boom
	_, err = lim.Reserve(context.Background(), bucketReq(UsageUnits{InputTokens: 1}))
	if !errors.Is(err, boom) || errors.As(err, &le) {
		t.Fatalf("backend error must pass through, not as LimitError: %v", err)
	}
}

func TestFromBucketOverMemoryBackend(t *testing.T) {
	mb, err := memory.New(memory.Config{
		Limits:  map[string]bucket.Limits{"lunaroute": {MaxConcurrency: 1}},
		MaxWait: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	lim := FromBucket(mb, func(ReservationRequest) string { return "lunaroute" })
	ctx := context.Background()
	held, err := lim.Reserve(ctx, bucketReq(UsageUnits{InputTokens: 1}))
	if err != nil {
		t.Fatal(err)
	}
	var le *llms.LimitError
	if _, err := lim.Reserve(ctx, bucketReq(UsageUnits{InputTokens: 1})); !errors.As(err, &le) {
		t.Fatalf("second slot: %v, want LimitError", err)
	}
	_ = held.Release(ctx)
	if _, err := lim.Reserve(ctx, bucketReq(UsageUnits{InputTokens: 1})); err != nil {
		t.Fatalf("after release: %v", err)
	}
}

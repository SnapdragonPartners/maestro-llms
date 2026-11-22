package remote

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/bucket"
)

const testID = "00000000-0000-4000-8000-000000000001"

// fake is a scripted Backend. Each hook defaults to success; calls are
// counted.
type fake struct {
	acquire  func(ctx context.Context, n int) (bucket.DetachedLease, error)
	renew    func(ctx context.Context, n int) (bucket.Validity, error)
	adjust   func(ctx context.Context, n int) error
	release  func(n int) error
	acquires atomic.Int32
	renews   atomic.Int32
	adjusts  atomic.Int32
	releases atomic.Int32
}

func grant(ttl time.Duration) bucket.DetachedLease {
	return bucket.DetachedLease{ID: testID, Validity: bucket.Validity{TTL: ttl, ExpiresIn: ttl, CapIn: time.Hour}}
}

func (f *fake) TryAcquireDetached(ctx context.Context, _ string, _ int) (bucket.DetachedLease, error) {
	n := int(f.acquires.Add(1))
	if f.acquire != nil {
		return f.acquire(ctx, n)
	}
	return grant(300 * time.Millisecond), nil
}

func (f *fake) RenewLease(ctx context.Context, _, _ string) (bucket.Validity, error) {
	n := int(f.renews.Add(1))
	if f.renew != nil {
		return f.renew(ctx, n)
	}
	return bucket.Validity{TTL: 300 * time.Millisecond, ExpiresIn: 300 * time.Millisecond, CapIn: time.Hour}, nil
}

func (f *fake) AdjustLease(ctx context.Context, _, _ string, _ int) error {
	n := int(f.adjusts.Add(1))
	if f.adjust != nil {
		return f.adjust(ctx, n)
	}
	return nil
}

func (f *fake) ReleaseLease(_ context.Context, _, _ string) error {
	n := int(f.releases.Add(1))
	if f.release != nil {
		return f.release(n)
	}
	return nil
}

func newTest(t *testing.T, f *fake, cfg Config) *Limiter {
	t.Helper()
	if cfg.PollInterval == 0 {
		cfg.PollInterval, cfg.MaxPollInterval = 5*time.Millisecond, 20*time.Millisecond
	}
	l, err := New(f, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func mustAcquire(t *testing.T, l *Limiter) *lease {
	t.Helper()
	got, err := l.Acquire(context.Background(), "k", 10)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	ls := got.(*lease) //nolint:forcetypeassert // test
	t.Cleanup(func() { _ = ls.Release(context.Background()) })
	return ls
}

func isLost(ls *lease) bool {
	select {
	case <-ls.Lost():
		return true
	default:
		return false
	}
}

func TestNewRejectsNilBackend(t *testing.T) {
	if _, err := New(nil, Config{}); err == nil {
		t.Fatal("nil Backend accepted")
	}
}

func TestTemporaryRefusalRetried(t *testing.T) {
	f := &fake{acquire: func(_ context.Context, n int) (bucket.DetachedLease, error) {
		if n < 3 {
			return bucket.DetachedLease{}, &bucket.LimitError{Key: "k", Reason: "full", RetryAfter: time.Millisecond}
		}
		return grant(time.Second), nil
	}}
	mustAcquire(t, newTest(t, f, Config{MaxWait: 2 * time.Second}))
	if n := f.acquires.Load(); n != 3 {
		t.Fatalf("attempts = %d, want 3", n)
	}
}

func TestPermanentRefusalOneAttempt(t *testing.T) {
	f := &fake{acquire: func(context.Context, int) (bucket.DetachedLease, error) {
		return bucket.DetachedLease{}, &bucket.LimitError{Key: "k", Reason: "too big", Permanent: true}
	}}
	_, err := newTest(t, f, Config{MaxWait: time.Second}).Acquire(context.Background(), "k", 1)
	var le *bucket.LimitError
	if !errors.As(err, &le) || !le.Permanent || le.Reason != "too big" {
		t.Fatalf("Acquire = %v, want the Permanent refusal", err)
	}
	if n := f.acquires.Load(); n != 1 {
		t.Fatalf("attempts = %d, want 1", n)
	}
}

// A transport failure's outcome is unknown (the backend may have admitted
// and lost the response), so it fails closed and is never replayed.
func TestTransportErrorFailsClosedWithoutRetry(t *testing.T) {
	boom := errors.New("connection reset")
	f := &fake{acquire: func(context.Context, int) (bucket.DetachedLease, error) { return bucket.DetachedLease{}, boom }}
	ls, err := newTest(t, f, Config{MaxWait: time.Second}).Acquire(context.Background(), "k", 1)
	if ls != nil || !errors.Is(err, boom) {
		t.Fatalf("Acquire = %v, %v; want no lease and the transport error", ls, err)
	}
	if n := f.acquires.Load(); n != 1 {
		t.Fatalf("attempts = %d, want 1", n)
	}
}

// A refusal carrying a Cause (the serving side's context ended) is not a
// capacity refusal: it ends the acquire.
func TestRefusalWithCauseNotRetried(t *testing.T) {
	f := &fake{acquire: func(context.Context, int) (bucket.DetachedLease, error) {
		return bucket.DetachedLease{}, &bucket.LimitError{Key: "k", Reason: "context done", Cause: context.Canceled}
	}}
	if _, err := newTest(t, f, Config{MaxWait: time.Second}).Acquire(context.Background(), "k", 1); err == nil {
		t.Fatal("admitted")
	}
	if n := f.acquires.Load(); n != 1 {
		t.Fatalf("attempts = %d, want 1", n)
	}
}

func TestCancellationEndsWait(t *testing.T) {
	f := &fake{acquire: func(context.Context, int) (bucket.DetachedLease, error) {
		return bucket.DetachedLease{}, &bucket.LimitError{Key: "k", Reason: "full"}
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := newTest(t, f, Config{}).Acquire(ctx, "k", 1)
	var le *bucket.LimitError
	if !errors.As(err, &le) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire = %v, want a LimitError with the context's Cause", err)
	}
}

// MaxWait is a hard deadline across backend calls: a grant that arrives
// after it is released and not returned.
func TestGrantAfterMaxWaitReleased(t *testing.T) {
	f := &fake{acquire: func(context.Context, int) (bucket.DetachedLease, error) {
		time.Sleep(150 * time.Millisecond) // the backend ignores ctx
		return grant(10 * time.Second), nil
	}}
	start := time.Now()
	ls, err := newTest(t, f, Config{MaxWait: 50 * time.Millisecond}).Acquire(context.Background(), "k", 1)
	var le *bucket.LimitError
	if ls != nil || !errors.As(err, &le) || le.Cause != nil || le.Permanent {
		t.Fatalf("Acquire = %v, %v; want a temporary MaxWait refusal", ls, err)
	}
	if f.releases.Load() != 1 {
		t.Fatalf("late grant released %d times, want 1", f.releases.Load())
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("took %v", el)
	}
}

// Validity is measured from when the request was sent: a grant whose
// response took longer than its validity is released and fails closed.
func TestGrantAfterValidityReleased(t *testing.T) {
	f := &fake{acquire: func(context.Context, int) (bucket.DetachedLease, error) {
		time.Sleep(120 * time.Millisecond)
		return grant(100 * time.Millisecond), nil
	}}
	ls, err := newTest(t, f, Config{}).Acquire(context.Background(), "k", 1)
	var le *bucket.LimitError
	if ls != nil || err == nil || errors.As(err, &le) {
		t.Fatalf("Acquire = %v, %v; want no lease and a non-refusal error", ls, err)
	}
	if f.releases.Load() != 1 {
		t.Fatalf("late grant released %d times, want 1", f.releases.Load())
	}
}

func TestInvalidGrantFailsClosed(t *testing.T) {
	for name, g := range map[string]bucket.DetachedLease{
		"no ID":  {Validity: bucket.Validity{TTL: time.Second, ExpiresIn: time.Second, CapIn: time.Hour}},
		"no TTL": {ID: testID, Validity: bucket.Validity{ExpiresIn: time.Second, CapIn: time.Hour}},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fake{acquire: func(context.Context, int) (bucket.DetachedLease, error) { return g, nil }}
			if ls, err := newTest(t, f, Config{}).Acquire(context.Background(), "k", 1); ls != nil || err == nil {
				t.Fatalf("Acquire = %v, %v; want an error", ls, err)
			}
		})
	}
}

func TestRenewsOnSchedule(t *testing.T) {
	f := &fake{}
	ls := mustAcquire(t, newTest(t, f, Config{}))
	time.Sleep(1100 * time.Millisecond) // TTL 300ms: a renewal every 100ms
	if n := f.renews.Load(); n < 6 {
		t.Fatalf("renewals = %d in 1.1s, want about 10", n)
	}
	if isLost(ls) {
		t.Fatal("lost while renewing")
	}
}

func TestRenewLostClosesLost(t *testing.T) {
	f := &fake{renew: func(context.Context, int) (bucket.Validity, error) { return bucket.Validity{}, bucket.ErrLeaseLost }}
	ls := mustAcquire(t, newTest(t, f, Config{}))
	select {
	case <-ls.Lost():
	case <-time.After(time.Second):
		t.Fatal("Lost() not closed on ErrLeaseLost")
	}
}

// A renewal outage closes Lost when the last confirmed validity runs out,
// measured from when that confirmation was requested.
func TestRenewalOutageClosesLostAtValidity(t *testing.T) {
	f := &fake{renew: func(context.Context, int) (bucket.Validity, error) {
		return bucket.Validity{}, errors.New("unreachable")
	}}
	start := time.Now()
	ls := mustAcquire(t, newTest(t, f, Config{}))
	select {
	case <-ls.Lost():
		if el := time.Since(start); el > 300*time.Millisecond+150*time.Millisecond {
			t.Fatalf("Lost() closed after %v, want at the 300ms validity", el)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Lost() not closed when the validity ran out")
	}
	if n := f.renews.Load(); n < 2 {
		t.Fatalf("renewals tried = %d, want retries before the validity ran out", n)
	}
}

// A renewal that hangs is bounded by the remaining validity, so Lost still
// closes on time.
func TestHungRenewalBoundedByValidity(t *testing.T) {
	f := &fake{renew: func(ctx context.Context, _ int) (bucket.Validity, error) {
		<-ctx.Done()
		return bucket.Validity{}, ctx.Err()
	}}
	start := time.Now()
	ls := mustAcquire(t, newTest(t, f, Config{OpTimeout: time.Minute}))
	select {
	case <-ls.Lost():
		if el := time.Since(start); el > 450*time.Millisecond {
			t.Fatalf("Lost() closed after %v, want at the 300ms validity", el)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Lost() not closed")
	}
}

func TestHardCapClosesLost(t *testing.T) {
	ttl := 300 * time.Millisecond
	f := &fake{acquire: func(context.Context, int) (bucket.DetachedLease, error) {
		return bucket.DetachedLease{ID: testID, Validity: bucket.Validity{TTL: ttl, ExpiresIn: ttl, CapIn: 500 * time.Millisecond}}, nil
	}}
	start := time.Now()
	ls := mustAcquire(t, newTest(t, f, Config{}))
	select {
	case <-ls.Lost():
		if el := time.Since(start); el > 650*time.Millisecond {
			t.Fatalf("Lost() closed after %v, want at the 500ms cap", el)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Lost() not closed at the hard cap although renewals succeed")
	}
}

// A renewal that reports a shorter cap than the grant tightens the cap.
func TestRenewalTightensCap(t *testing.T) {
	f := &fake{renew: func(context.Context, int) (bucket.Validity, error) {
		return bucket.Validity{TTL: 300 * time.Millisecond, ExpiresIn: 50 * time.Millisecond, CapIn: 50 * time.Millisecond}, nil
	}}
	start := time.Now()
	ls := mustAcquire(t, newTest(t, f, Config{}))
	select {
	case <-ls.Lost():
		if el := time.Since(start); el > 300*time.Millisecond {
			t.Fatalf("Lost() closed after %v, want soon after the first renewal", el)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Lost() not closed")
	}
}

func TestAdjustSentUntilAnswered(t *testing.T) {
	f := &fake{adjust: func(_ context.Context, n int) error {
		if n == 1 {
			return errors.New("timeout")
		}
		return nil
	}}
	ls := mustAcquire(t, newTest(t, f, Config{}))
	ctx := context.Background()
	if err := ls.Adjust(ctx, 5); err == nil {
		t.Fatal("first Adjust hid the transport error")
	}
	if err := ls.Adjust(ctx, 5); err != nil {
		t.Fatalf("second Adjust: %v", err)
	}
	if err := ls.Adjust(ctx, 5); err != nil {
		t.Fatalf("third Adjust: %v", err)
	}
	if n := f.adjusts.Load(); n != 2 {
		t.Fatalf("adjust calls = %d, want 2 (resent after the error, then no-ops)", n)
	}
}

func TestAdjustLeaseLostClosesLost(t *testing.T) {
	f := &fake{adjust: func(context.Context, int) error { return bucket.ErrLeaseLost }}
	ls := mustAcquire(t, newTest(t, f, Config{}))
	if err := ls.Adjust(context.Background(), 5); !errors.Is(err, bucket.ErrLeaseLost) {
		t.Fatalf("Adjust = %v", err)
	}
	if !isLost(ls) {
		t.Fatal("Lost() open after ErrLeaseLost")
	}
	if err := ls.Adjust(context.Background(), 5); err != nil || f.adjusts.Load() != 1 {
		t.Fatalf("second Adjust = %v after %d calls, want a no-op", err, f.adjusts.Load())
	}
}

func TestAdjustSurvivesCanceledContext(t *testing.T) {
	var sawCanceled atomic.Bool
	f := &fake{adjust: func(ctx context.Context, _ int) error {
		sawCanceled.Store(ctx.Err() != nil)
		return nil
	}}
	ls := mustAcquire(t, newTest(t, f, Config{}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ls.Adjust(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if sawCanceled.Load() {
		t.Fatal("Adjust passed a canceled context to the backend")
	}
}

func TestReleaseOnceStopsRenewal(t *testing.T) {
	f := &fake{}
	ls := mustAcquire(t, newTest(t, f, Config{}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Release survives a canceled context
	if err := ls.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if err := ls.Release(ctx); err != nil {
		t.Fatal(err)
	}
	renews := f.renews.Load()
	time.Sleep(250 * time.Millisecond)
	if f.releases.Load() != 1 || f.renews.Load() != renews {
		t.Fatalf("releases = %d, renewals after release = %d", f.releases.Load(), f.renews.Load()-renews)
	}
	if isLost(ls) {
		t.Fatal("a clean release closed Lost")
	}
}

func TestReleaseLeaseLost(t *testing.T) {
	f := &fake{release: func(int) error { return bucket.ErrLeaseLost }}
	ls := mustAcquire(t, newTest(t, f, Config{}))
	for range 2 {
		if err := ls.Release(context.Background()); !errors.Is(err, bucket.ErrLeaseLost) {
			t.Fatalf("Release = %v, want ErrLeaseLost every time", err)
		}
	}
	if !isLost(ls) {
		t.Fatal("Lost() open after ErrLeaseLost")
	}
}

func TestConcurrentAcquires(t *testing.T) {
	f := &fake{}
	l := newTest(t, f, Config{})
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			ls, err := l.Acquire(context.Background(), "k", 1)
			if err != nil {
				t.Error(err)
				return
			}
			_ = ls.Adjust(context.Background(), 1)
			_ = ls.Release(context.Background())
		})
	}
	wg.Wait()
	if f.releases.Load() != 20 {
		t.Fatalf("releases = %d", f.releases.Load())
	}
}

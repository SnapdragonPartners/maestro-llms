package ratelimit

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/SnapdragonPartners/maestro-llms/llms"
)

var (
	_ Reservation = (*allReservation)(nil)
	_ Rollbacker  = (*allReservation)(nil)
)

// eventLog records limiter and reservation calls across fakes, in order.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (e *eventLog) add(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, s)
}

func (e *eventLog) get() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.events)
}

// fakeLimiter admits or refuses per its err, and hands out fake reservations
// that log every call. rollback selects whether they implement Rollbacker.
type fakeLimiter struct {
	err      error
	log      *eventLog
	opErr    error // returned by Commit/Release/Rollback
	name     string
	gotReq   []ReservationRequest
	rollback bool
}

func (f *fakeLimiter) Reserve(_ context.Context, req ReservationRequest) (Reservation, error) {
	f.gotReq = append(f.gotReq, req)
	f.log.add(f.name + ".reserve")
	if f.err != nil {
		return nil, f.err
	}
	base := &fakeReservation{name: f.name, log: f.log, err: f.opErr}
	if f.rollback {
		return &fakeRollbackReservation{base}, nil
	}
	return base, nil
}

type fakeReservation struct {
	err  error
	log  *eventLog
	name string
}

func (r *fakeReservation) Commit(_ context.Context, _ llms.Usage) error {
	r.log.add(r.name + ".commit")
	return r.err
}

func (r *fakeReservation) Release(ctx context.Context) error {
	r.log.add(r.name + ".release" + canceledSuffix(ctx))
	return r.err
}

type fakeRollbackReservation struct{ *fakeReservation }

func (r *fakeRollbackReservation) Rollback(ctx context.Context) error {
	r.log.add(r.name + ".rollback" + canceledSuffix(ctx))
	return r.err
}

func canceledSuffix(ctx context.Context) string {
	if ctx.Err() != nil {
		return "(canceled)"
	}
	return ""
}

func mustAll(t *testing.T, ls ...Limiter) Limiter {
	t.Helper()
	l, err := All(ls...)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	return l
}

func assertEvents(t *testing.T, log *eventLog, want ...string) {
	t.Helper()
	if got := log.get(); !slices.Equal(got, want) {
		t.Fatalf("events:\n got  %v\n want %v", got, want)
	}
}

func TestAllRejectsBadConfig(t *testing.T) {
	if l, err := All(); err == nil || l != nil {
		t.Fatalf("All(): got (%v, %v), want error", l, err)
	}
	log := &eventLog{}
	if l, err := All(&fakeLimiter{name: "a", log: log}, nil); err == nil || l != nil {
		t.Fatalf("All(a, nil): got (%v, %v), want error", l, err)
	}
	var typedNil *InMemoryLimiter
	if l, err := All(&fakeLimiter{name: "a", log: log}, typedNil); err == nil || l != nil {
		t.Fatalf("All(a, typed nil): got (%v, %v), want error", l, err)
	}
	assertEvents(t, log) // rejected configs never reserve
}

func TestAllAdmitsWhenEveryLimiterAdmits(t *testing.T) {
	log := &eventLog{}
	a := &fakeLimiter{name: "a", log: log}
	b := &fakeLimiter{name: "b", log: log, rollback: true}
	l := mustAll(t, a, b)

	req := chatReq(42)
	res := mustReserve(t, l, req)
	if err := res.Commit(context.Background(), llms.Usage{InputTokens: 1}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := res.Release(context.Background()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// Reserve in order, commit to all, release in reverse order. A
	// successful Release never rolls back, Rollbacker or not.
	assertEvents(t, log, "a.reserve", "b.reserve", "a.commit", "b.commit", "b.release", "a.release")
	for _, f := range []*fakeLimiter{a, b} {
		if len(f.gotReq) != 1 || f.gotReq[0].EstimatedUnits != req.EstimatedUnits {
			t.Fatalf("%s got %+v, want the same request", f.name, f.gotReq)
		}
	}
}

func TestAllRollsBackOnLaterRefusal(t *testing.T) {
	log := &eventLog{}
	refusal := &llms.LimitError{Permanent: true, Reason: "budget"}
	a := &fakeLimiter{name: "a", log: log}                 // no Rollback: released
	b := &fakeLimiter{name: "b", log: log, rollback: true} // Rollback
	c := &fakeLimiter{name: "c", log: log, err: refusal}
	d := &fakeLimiter{name: "d", log: log}
	l := mustAll(t, a, b, c, d)

	res, err := l.Reserve(context.Background(), chatReq(10))
	if res != nil {
		t.Fatalf("Reserve returned a reservation on refusal: %v", res)
	}
	// The refusal passes through unwrapped, so retry/circuit classify it as
	// they would the refusing limiter alone.
	if err != refusal { //nolint:errorlint // asserting identity: All must not wrap
		t.Fatalf("err = %v, want the refusing limiter's error unwrapped", err)
	}
	if llms.Retryable(err) {
		t.Fatal("permanent refusal became retryable")
	}
	// Held reservations roll back in reverse order; the limiter after the
	// refusal is never asked.
	assertEvents(t, log, "a.reserve", "b.reserve", "c.reserve", "b.rollback", "a.release")
}

func TestAllFirstRefusalHoldsNothing(t *testing.T) {
	log := &eventLog{}
	refusal := &llms.LimitError{Reason: "rate"}
	l := mustAll(t, &fakeLimiter{name: "a", log: log, err: refusal}, &fakeLimiter{name: "b", log: log})
	if _, err := l.Reserve(context.Background(), chatReq(10)); err != refusal { //nolint:errorlint // identity
		t.Fatalf("err = %v, want %v", err, refusal)
	}
	if !llms.Retryable(refusal) {
		t.Fatal("test setup: rate refusal should be retryable")
	}
	assertEvents(t, log, "a.reserve")
}

func TestAllRollbackSurvivesCanceledContext(t *testing.T) {
	log := &eventLog{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	l := mustAll(t,
		&fakeLimiter{name: "a", log: log, rollback: true},
		&fakeLimiter{name: "b", log: log},
		&fakeLimiter{name: "c", log: log, err: context.Canceled},
	)
	if _, err := l.Reserve(ctx, chatReq(1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	// Neither rollback nor the Release fallback sees the canceled context.
	assertEvents(t, log, "a.reserve", "b.reserve", "c.reserve", "b.release", "a.rollback")
}

func TestAllRollbackErrorsDoNotMaskRefusal(t *testing.T) {
	log := &eventLog{}
	refusal := errors.New("store down")
	l := mustAll(t,
		&fakeLimiter{name: "a", log: log, rollback: true, opErr: errors.New("rollback failed")},
		&fakeLimiter{name: "b", log: log, err: refusal},
	)
	if _, err := l.Reserve(context.Background(), chatReq(1)); err != refusal { //nolint:errorlint // identity
		t.Fatalf("err = %v, want the refusal", err)
	}
	assertEvents(t, log, "a.reserve", "b.reserve", "a.rollback")
}

func TestAllReservationJoinsErrors(t *testing.T) {
	log := &eventLog{}
	l := mustAll(t,
		&fakeLimiter{name: "a", log: log, opErr: errors.New("a failed")},
		&fakeLimiter{name: "b", log: log},
		&fakeLimiter{name: "c", log: log, opErr: errors.New("c failed")},
	)
	res := mustReserve(t, l, chatReq(1))
	bothJoined := func(op string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "a failed") || !strings.Contains(err.Error(), "c failed") {
			t.Fatalf("%s err = %v, want both failures joined", op, err)
		}
	}
	bothJoined("Commit", res.Commit(context.Background(), llms.Usage{}))
	bothJoined("Release", res.Release(context.Background()))
	// Every reservation was still committed and released despite errors.
	assertEvents(t, log, "a.reserve", "b.reserve", "c.reserve",
		"a.commit", "b.commit", "c.commit", "c.release", "b.release", "a.release")
}

func TestAllReleaseRollbackAndCommitAfterEnd(t *testing.T) {
	t.Run("release then rollback", func(t *testing.T) {
		log := &eventLog{}
		res := mustReserve(t, mustAll(t, &fakeLimiter{name: "a", log: log, rollback: true}), chatReq(1))
		_ = res.Release(context.Background())
		_ = res.Release(context.Background())
		_ = res.(Rollbacker).Rollback(context.Background()) //nolint:forcetypeassert // asserted by var block
		_ = res.Commit(context.Background(), llms.Usage{InputTokens: 1})
		assertEvents(t, log, "a.reserve", "a.release")
	})
	t.Run("rollback then release", func(t *testing.T) {
		log := &eventLog{}
		res := mustReserve(t, mustAll(t, &fakeLimiter{name: "a", log: log, rollback: true}), chatReq(1))
		rb := res.(Rollbacker) //nolint:forcetypeassert // asserted by var block
		_ = rb.Rollback(context.Background())
		_ = rb.Rollback(context.Background())
		_ = res.Release(context.Background())
		_ = res.Commit(context.Background(), llms.Usage{InputTokens: 1})
		assertEvents(t, log, "a.reserve", "a.rollback")
	})
}

func TestAllNestedPassesRollbackThrough(t *testing.T) {
	log := &eventLog{}
	inner := mustAll(t,
		&fakeLimiter{name: "a", log: log, rollback: true},
		&fakeLimiter{name: "b", log: log},
	)
	outer := mustAll(t, inner, &fakeLimiter{name: "c", log: log, err: errors.New("refused")})
	if _, err := outer.Reserve(context.Background(), chatReq(1)); err == nil {
		t.Fatal("want refusal")
	}
	assertEvents(t, log, "a.reserve", "b.reserve", "c.reserve", "b.release", "a.rollback")
}

// TestAllWithInMemoryLimiters checks All against real limiters: a refusal by
// the second rolls back (here: releases) the first's concurrency slot.
func TestAllWithInMemoryLimiters(t *testing.T) {
	first := mustNewLimiter(t, Config{MaxConcurrency: 1})
	second := mustNewLimiter(t, Config{TokensPerMinute: 100}) // capacity 90
	l := mustAll(t, first, second)

	_, err := l.Reserve(context.Background(), chatReq(1000))
	var le *llms.LimitError
	if !errors.As(err, &le) || !le.Permanent {
		t.Fatalf("err = %v, want a permanent *llms.LimitError (impossible request)", err)
	}
	snap, _ := first.Stats(context.Background())
	if snap.ActiveRequests != 0 {
		t.Fatalf("first limiter still holds %d slots after rollback", snap.ActiveRequests)
	}
	res := mustReserve(t, l, chatReq(10))
	_ = res.Release(context.Background())
}

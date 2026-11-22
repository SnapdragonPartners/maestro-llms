package postgres

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"sync"
	"testing"
	"time"

	"github.com/SnapdragonPartners/maestro-llms/bucket"
	"github.com/SnapdragonPartners/maestro-llms/bucket/internal/core"
	"github.com/SnapdragonPartners/maestro-llms/bucket/remote"
)

var _ remote.Backend = (*Limiter)(nil)

// leaseRow is a lease row's state, for proving an operation changed nothing.
type leaseRow struct {
	expiresAt time.Time
	present   bool
	detached  bool
	adjusted  bool
}

func readLease(t *testing.T, db *sql.DB, id string) leaseRow {
	t.Helper()
	var r leaseRow
	err := db.QueryRow(`SELECT expires_at, detached, adjusted FROM bucket.lease WHERE id = $1`, id).
		Scan(&r.expiresAt, &r.detached, &r.adjusted)
	if errors.Is(err, sql.ErrNoRows) {
		return r
	}
	if err != nil {
		t.Fatalf("read lease: %v", err)
	}
	r.present = true
	return r
}

// storedBalance is the bucket's stored (last settled) token balance. An
// operation that commits nothing leaves it exactly as it was.
func storedBalance(t *testing.T, db *sql.DB, name string) float64 {
	t.Helper()
	var b float64
	if err := db.QueryRow(`SELECT token_balance FROM bucket.bucket WHERE name = $1`, name).Scan(&b); err != nil {
		t.Fatalf("read balance: %v", err)
	}
	return b
}

func mustDetach(t *testing.T, l *Limiter, key string, tokens int) bucket.DetachedLease {
	t.Helper()
	dl, err := l.TryAcquireDetached(context.Background(), key, tokens)
	if err != nil {
		t.Fatalf("TryAcquireDetached(%q, %d): %v", key, tokens, err)
	}
	return dl
}

// allLost runs every by-ID operation with key and id and requires
// ErrLeaseLost from each, so none of them took effect.
func allLost(t *testing.T, l *Limiter, key, id string) {
	t.Helper()
	ctx := context.Background()
	if _, err := l.RenewLease(ctx, key, id); !errors.Is(err, bucket.ErrLeaseLost) {
		t.Errorf("RenewLease(%q, %q) = %v, want ErrLeaseLost", key, id, err)
	}
	if err := l.AdjustLease(ctx, key, id, 1); !errors.Is(err, bucket.ErrLeaseLost) {
		t.Errorf("AdjustLease(%q, %q) = %v, want ErrLeaseLost", key, id, err)
	}
	if err := l.ReleaseLease(ctx, key, id); !errors.Is(err, bucket.ErrLeaseLost) {
		t.Errorf("ReleaseLease(%q, %q) = %v, want ErrLeaseLost", key, id, err)
	}
}

func TestTryAcquireDetachedRefusals(t *testing.T) {
	l := newLimiter(t, openMain(t), Config{})
	ctx := context.Background()
	if _, err := l.TryAcquireDetached(ctx, bucketName(t), 0); !errors.Is(err, bucket.ErrUnknownKey) {
		t.Fatalf("unknown key: %v", err)
	}
	tok := bucketName(t)
	ensure(t, l, tok, bucket.Limits{TokensPerMinute: 600, BufferFactor: 1})
	var le *bucket.LimitError
	if _, err := l.TryAcquireDetached(ctx, tok, 601); !errors.As(err, &le) || !le.Permanent {
		t.Fatalf("oversized request: %v, want a Permanent refusal", err)
	}
	mustDetach(t, l, tok, 600)
	if _, err := l.TryAcquireDetached(ctx, tok, 100); !errors.As(err, &le) || le.Permanent || le.Cause != nil ||
		le.RetryAfter < 8*time.Second || le.RetryAfter > 10*time.Second {
		t.Fatalf("empty bucket: %v, want a temporary refusal with RetryAfter about 10s", err)
	}
	slot := bucketName(t)
	ensure(t, l, slot, bucket.Limits{MaxConcurrency: 1})
	mustDetach(t, l, slot, 0)
	if _, err := l.TryAcquireDetached(ctx, slot, 0); !errors.As(err, &le) || le.Permanent || le.Cause != nil || le.RetryAfter != 0 {
		t.Fatalf("full slots: %v, want a temporary refusal with no RetryAfter", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := l.TryAcquireDetached(canceled, tok, 1); !errors.As(err, &le) || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v, want a LimitError with the Cause", err)
	}
}

// TestDetachedLifecycle: a detached lease holds a slot against in-process
// acquires, renews, adjusts from its stored terms, and frees the slot.
func TestDetachedLifecycle(t *testing.T) {
	db := openMain(t)
	cfg := Config{LeaseTTL: 2 * time.Second, MaxLeaseDuration: time.Minute, MaxWait: admitWait}
	l := newLimiter(t, db, cfg)
	name := bucketName(t)
	ensure(t, l, name, bucket.Limits{TokensPerMinute: 600, MaxConcurrency: 1, BufferFactor: 1})
	ctx := context.Background()

	dl := mustDetach(t, l, name, 100)
	if !validLeaseID(dl.ID) || dl.TTL != cfg.LeaseTTL || dl.ExpiresIn > dl.TTL || dl.ExpiresIn < dl.TTL-time.Second ||
		dl.CapIn > cfg.MaxLeaseDuration || dl.CapIn < cfg.MaxLeaseDuration-time.Second {
		t.Fatalf("grant = %+v", dl)
	}
	if r := readLease(t, db, dl.ID); !r.present || !r.detached || r.adjusted {
		t.Fatalf("row = %+v", r)
	}
	mustRefuse(t, l, name) // the detached lease holds the only slot

	v, err := l.RenewLease(ctx, name, dl.ID)
	if err != nil || v.TTL != cfg.LeaseTTL || v.ExpiresIn > v.TTL || v.CapIn > dl.CapIn {
		t.Fatalf("RenewLease = %+v, %v", v, err)
	}
	before := storedBalance(t, db, name) // 600 - 100 at acquire
	if err := l.AdjustLease(ctx, name, dl.ID, 50); err != nil {
		t.Fatal(err)
	}
	if got := storedBalance(t, db, name); got < before+50 || got > before+50+20 {
		t.Fatalf("balance %v after refunding 50 from %v", got, before)
	}
	if err := l.ReleaseLease(ctx, name, dl.ID); err != nil {
		t.Fatal(err)
	}
	if r := readLease(t, db, dl.ID); r.present {
		t.Fatal("row survived release")
	}
	allLost(t, l, name, dl.ID) // a repeat release, and anything else, finds it gone
	_ = mustAcquire(t, l, name, 0).Release(ctx)
}

// TestRenewalUsesAcquireTerms: instance A grants a short lease; instance B,
// configured with a far longer TTL and cap, renews it. B extends it by A's
// TTL only, and never past A's cap.
func TestRenewalUsesAcquireTerms(t *testing.T) {
	db := openMain(t)
	ttl, maxLease := 300*time.Millisecond, 900*time.Millisecond
	a := newLimiter(t, db, Config{LeaseTTL: ttl, MaxLeaseDuration: maxLease})
	b := newLimiter(t, openMain(t), Config{LeaseTTL: 10 * time.Second, MaxLeaseDuration: time.Hour})
	name := bucketName(t)
	ensure(t, a, name, bucket.Limits{MaxConcurrency: 1})
	start := time.Now()
	dl := mustDetach(t, a, name, 0)
	ctx := context.Background()
	lastCap := dl.CapIn
	for {
		time.Sleep(ttl / 3)
		v, err := b.RenewLease(ctx, name, dl.ID)
		if errors.Is(err, bucket.ErrLeaseLost) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if v.TTL != ttl || v.ExpiresIn > ttl || v.ExpiresIn > v.CapIn || v.CapIn > lastCap {
			t.Fatalf("renewal by B = %+v, want A's terms (TTL %v, cap within %v)", v, ttl, lastCap)
		}
		lastCap = v.CapIn
		if time.Since(start) > maxLease+2*time.Second {
			t.Fatal("B kept renewing past A's cap")
		}
	}
	if el := time.Since(start); el < maxLease-ttl/3 {
		t.Fatalf("lease lost after %v, before A's cap %v", el, maxLease)
	}
	// The slot is free right after the cap, though B's own TTL is 10s.
	_ = mustAcquire(t, newLimiter(t, db, Config{MaxWait: admitWait}), name, 0).Release(ctx)
}

// split routes acquires to one instance and everything else to another,
// with an optional delay before the acquire's response is delivered, as a
// load balancer and a slow network would.
type split struct {
	acq, rest *Limiter
	delay     time.Duration
}

func (s split) TryAcquireDetached(ctx context.Context, key string, tokens int) (bucket.DetachedLease, error) {
	dl, err := s.acq.TryAcquireDetached(ctx, key, tokens)
	time.Sleep(s.delay)
	return dl, err
}

func (s split) RenewLease(ctx context.Context, key, id string) (bucket.Validity, error) {
	return s.rest.RenewLease(ctx, key, id)
}

func (s split) AdjustLease(ctx context.Context, key, id string, n int) error {
	return s.rest.AdjustLease(ctx, key, id, n)
}

func (s split) ReleaseLease(ctx context.Context, key, id string) error {
	return s.rest.ReleaseLease(ctx, key, id)
}

// TestRemoteAcrossInstances: the holder-side helper over two differently
// configured instances keeps the slot past several of A's TTLs, adjusts and
// releases through B, and fails closed on a grant whose response arrives
// after its validity.
func TestRemoteAcrossInstances(t *testing.T) {
	db := openMain(t)
	ttl := 600 * time.Millisecond
	a := newLimiter(t, db, Config{LeaseTTL: ttl, MaxLeaseDuration: time.Minute})
	b := newLimiter(t, openMain(t), Config{LeaseTTL: 10 * time.Second, MaxLeaseDuration: time.Hour})
	name := bucketName(t)
	ensure(t, a, name, bucket.Limits{TokensPerMinute: 600, MaxConcurrency: 1, BufferFactor: 1})
	ctx := context.Background()

	rl, err := remote.New(split{acq: a, rest: b, delay: 50 * time.Millisecond}, remote.Config{MaxWait: admitWait})
	if err != nil {
		t.Fatal(err)
	}
	held, err := rl.Acquire(ctx, name, 100)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * ttl)
	mustRefuse(t, a, name) // renewed through B on A's terms
	select {
	case <-held.Lost():
		t.Fatal("lost while renewing")
	default:
	}
	before := storedBalance(t, db, name)
	if err = held.Adjust(ctx, 40); err != nil {
		t.Fatal(err)
	}
	if got := storedBalance(t, db, name); got < before+60 || got > before+60+30 {
		t.Fatalf("balance %v after refunding 60 from %v", got, before)
	}
	if err = held.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if n := liveLeases(t, db, name); n != 0 {
		t.Fatalf("live leases after release = %d", n)
	}

	late, err := remote.New(split{acq: a, rest: b, delay: ttl + 100*time.Millisecond}, remote.Config{MaxWait: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if ls, err := late.Acquire(ctx, name, 0); ls != nil || err == nil {
		t.Fatalf("late grant: %v, %v; want no lease", ls, err)
	}
	if n := liveLeases(t, db, name); n != 0 {
		t.Fatalf("the late grant was not released: %d live", n)
	}
}

// TestConcurrentDuplicateAdjustments: the balance changes once however many
// callers adjust at the same time.
func TestConcurrentDuplicateAdjustments(t *testing.T) {
	db := openMain(t)
	l := newLimiter(t, db, Config{LeaseTTL: 10 * time.Second})
	name := bucketName(t)
	ensure(t, l, name, bucket.Limits{TokensPerMinute: 60, BufferFactor: 1}) // capacity 60, 1/s
	dl := mustDetach(t, l, name, 50)                                        // balance 10
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for range 10 {
		wg.Go(func() { errs <- l.AdjustLease(context.Background(), name, dl.ID, 30) }) // refund 20
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("AdjustLease: %v", err)
		}
	}
	// Once: 10 + 20 = 30 (+1/s refill). Twice would be 50.
	if got, refill := storedBalance(t, db, name), time.Since(start).Seconds()+1; got < 30 || got > 30+refill {
		t.Fatalf("balance = %v, want 30 (+%.1f refill): the refund applied more than once", got, refill)
	}
	if r := readLease(t, db, dl.ID); !r.adjusted {
		t.Fatal("lease not marked adjusted")
	}
}

// TestDetachedExpiry: an expired lease answers the same before and after a
// later acquire reaps its row: every operation is ErrLeaseLost, the
// estimate stands, and a retried adjustment never applies twice.
func TestDetachedExpiry(t *testing.T) {
	db := openMain(t)
	ttl := 300 * time.Millisecond
	l := newLimiter(t, db, Config{LeaseTTL: ttl, MaxWait: admitWait})
	name := bucketName(t)
	ensure(t, l, name, bucket.Limits{TokensPerMinute: 600, BufferFactor: 1})
	t.Run("before reaping", func(t *testing.T) {
		dl := mustDetach(t, l, name, 100)
		if err := l.AdjustLease(context.Background(), name, dl.ID, 50); err != nil { // committed; say its response was lost
			t.Fatal(err)
		}
		time.Sleep(ttl + 100*time.Millisecond)
		if r := readLease(t, db, dl.ID); !r.present {
			t.Fatal("row gone before any reap")
		}
		balance := storedBalance(t, db, name)
		if err := l.AdjustLease(context.Background(), name, dl.ID, 50); !errors.Is(err, bucket.ErrLeaseLost) {
			t.Fatalf("retried AdjustLease after expiry = %v, want ErrLeaseLost", err)
		}
		if _, err := l.RenewLease(context.Background(), name, dl.ID); !errors.Is(err, bucket.ErrLeaseLost) {
			t.Fatalf("RenewLease after expiry = %v", err)
		}
		if got := storedBalance(t, db, name); got != balance {
			t.Fatalf("balance %v -> %v: an operation on an expired lease committed", balance, got)
		}
		if err := l.ReleaseLease(context.Background(), name, dl.ID); !errors.Is(err, bucket.ErrLeaseLost) {
			t.Fatalf("ReleaseLease after expiry = %v", err)
		}
	})
	t.Run("after reaping", func(t *testing.T) {
		dl := mustDetach(t, l, name, 100)
		time.Sleep(ttl + 100*time.Millisecond)
		_ = mustAcquire(t, l, name, 0).Release(context.Background()) // reaps the expired row
		if r := readLease(t, db, dl.ID); r.present {
			t.Fatal("acquire did not reap the expired detached lease")
		}
		balance := storedBalance(t, db, name)
		allLost(t, l, name, dl.ID)
		if got := storedBalance(t, db, name); got != balance {
			t.Fatalf("balance %v -> %v: the estimate did not stand", balance, got)
		}
	})
}

// TestByIDWrongKey: a wrong key, an unknown or malformed ID, or a key a
// renamed bucket no longer has gets ErrLeaseLost and changes nothing; a new
// bucket reusing the old name never gains the lease.
func TestByIDWrongKey(t *testing.T) {
	db := openMain(t)
	l := newLimiter(t, db, Config{LeaseTTL: 10 * time.Second})
	name, other := bucketName(t), bucketName(t)
	lim := bucket.Limits{TokensPerMinute: 600, MaxConcurrency: 2, BufferFactor: 1}
	ensure(t, l, name, lim)
	ensure(t, l, other, lim)
	dl := mustDetach(t, l, name, 100)
	row, balance := readLease(t, db, dl.ID), storedBalance(t, db, name)

	unchanged := func(when, current string) {
		t.Helper()
		if r := readLease(t, db, dl.ID); r != row {
			t.Fatalf("%s: lease %+v -> %+v", when, row, r)
		}
		if b := storedBalance(t, db, current); b != balance {
			t.Fatalf("%s: balance %v -> %v", when, balance, b)
		}
	}
	allLost(t, l, other, dl.ID)
	allLost(t, l, "no-such-bucket", dl.ID)
	allLost(t, l, name, "00000000-0000-4000-8000-000000000000")
	for _, bad := range []string{"", "not-a-uuid", dl.ID + "0", "{" + dl.ID[1:] + "}", "x" + dl.ID[1:]} {
		allLost(t, l, name, bad)
	}
	unchanged("wrong key or ID", name)

	renamed := bucketName(t)
	if _, err := db.Exec(`UPDATE bucket.bucket SET name = $1 WHERE name = $2`, renamed, name); err != nil {
		t.Fatal(err)
	}
	allLost(t, l, name, dl.ID)
	unchanged("old name after rename", renamed)
	ensure(t, l, name, lim) // a new bucket takes the old name
	allLost(t, l, name, dl.ID)
	if r := readLease(t, db, dl.ID); r != row {
		t.Fatalf("new bucket with the old name changed the lease: %+v -> %+v", row, r)
	}

	ctx := context.Background()
	if _, err := l.RenewLease(ctx, renamed, dl.ID); err != nil {
		t.Fatalf("RenewLease under the new name: %v", err)
	}
	if err := l.AdjustLease(ctx, renamed, dl.ID, 100); err != nil {
		t.Fatalf("AdjustLease under the new name: %v", err)
	}
	if err := l.ReleaseLease(ctx, renamed, dl.ID); err != nil {
		t.Fatalf("ReleaseLease under the new name: %v", err)
	}
}

// TestByIDRejectsInProcessLease: by-ID operations can't touch an in-process
// lease, and that lease keeps its own reconciliation: after its row is
// reaped its Go handle still adjusts the balance, exactly once.
func TestByIDRejectsInProcessLease(t *testing.T) {
	db := openMain(t)
	l := newLimiter(t, db, Config{LeaseTTL: 10 * time.Second, MaxWait: admitWait})
	name := bucketName(t)
	ensure(t, l, name, bucket.Limits{TokensPerMinute: 60, BufferFactor: 1}) // capacity 60, 1/s
	ls := mustAcquire(t, l, name, 50)                                       // balance 10
	row, balance := readLease(t, db, ls.id), storedBalance(t, db, name)
	allLost(t, l, name, ls.id)
	if r := readLease(t, db, ls.id); r != row || r.detached || r.adjusted {
		t.Fatalf("by-ID calls changed the in-process lease: %+v -> %+v", row, r)
	}
	if b := storedBalance(t, db, name); b != balance {
		t.Fatalf("by-ID calls changed the balance: %v -> %v", balance, b)
	}

	if _, err := db.Exec(`DELETE FROM bucket.lease WHERE id = $1`, ls.id); err != nil { // reaped
		t.Fatal(err)
	}
	start := time.Now()
	ctx := context.Background()
	if err := ls.Adjust(ctx, 20); err != nil { // refund 30
		t.Fatal(err)
	}
	if err := ls.Adjust(ctx, 20); err != nil {
		t.Fatal(err)
	}
	// Once: 10 + 30 = 40 (+1/s refill). Twice would reach the cap of 60.
	if got, refill := storedBalance(t, db, name), time.Since(start).Seconds()+2; got < 40 || got > 40+refill {
		t.Fatalf("balance = %v, want 40 (+%.1f refill)", got, refill)
	}
	if err := ls.Release(ctx); !errors.Is(err, bucket.ErrLeaseLost) {
		t.Fatalf("Release of the reaped lease = %v, want ErrLeaseLost", err)
	}
}

// TestMixedVersionAdmission: rows a v1 binary writes (no v2 columns) and
// detached leases share one concurrency count, and by-ID calls never touch
// the v1 rows.
func TestMixedVersionAdmission(t *testing.T) {
	db := openMain(t)
	l := newLimiter(t, db, Config{LeaseTTL: 10 * time.Second})
	name := bucketName(t)
	ensure(t, l, name, bucket.Limits{MaxConcurrency: 2})
	var v1 string // exactly the insert a v1 binary's Acquire runs
	if err := db.QueryRow(`
INSERT INTO bucket.lease (bucket_id, acquired_at, expires_at)
VALUES ((SELECT id FROM bucket.bucket WHERE name = $1), clock_timestamp(), clock_timestamp() + interval '1 minute')
RETURNING id`, name).Scan(&v1); err != nil {
		t.Fatal(err)
	}
	dl := mustDetach(t, l, name, 0)
	var le *bucket.LimitError
	if _, err := l.TryAcquireDetached(context.Background(), name, 0); !errors.As(err, &le) || le.Permanent {
		t.Fatalf("third lease: %v, want a temporary refusal", err)
	}
	mustRefuse(t, l, name)
	row := readLease(t, db, v1)
	allLost(t, l, name, v1)
	if r := readLease(t, db, v1); r != row || r.detached {
		t.Fatalf("by-ID calls changed the v1 row: %+v -> %+v", row, r)
	}
	if err := l.ReleaseLease(context.Background(), name, dl.ID); err != nil {
		t.Fatal(err)
	}
	_ = mustAcquire(t, newLimiter(t, db, Config{MaxWait: admitWait}), name, 0).Release(context.Background())
}

// TestSchemaV1Rejected: a database still at v1 fails New with the version it
// found, and v2's down migration returns it to exactly that.
func TestSchemaV1Rejected(t *testing.T) {
	requireDB(t)
	dsn, drop, err := createDatabase("bucket_test_v1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(drop)
	db, _ := sql.Open("pgx", dsn)
	t.Cleanup(func() { db.Close() })
	run := func(name string) {
		t.Helper()
		body, err := fs.ReadFile(Migrations(), name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(body)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	ctx := context.Background()
	var se *SchemaError
	run("000001_init.up.sql")
	if _, err := New(ctx, db, Config{}); !errors.As(err, &se) || se.Found != 1 || se.Required != 2 {
		t.Fatalf("v1 database: %v, want SchemaError found v1", err)
	}
	run("000002_detached_leases.up.sql")
	l := newLimiter(t, db, Config{})
	ensure(t, l, "k", bucket.Limits{MaxConcurrency: 1})
	mustDetach(t, l, "k", 0)
	run("000002_detached_leases.down.sql")
	if _, err := New(ctx, db, Config{}); !errors.As(err, &se) || se.Found != 1 {
		t.Fatalf("after down: %v, want SchemaError found v1", err)
	}
	var cols int
	if err := db.QueryRow(`SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'bucket' AND table_name = 'lease'`).Scan(&cols); err != nil || cols != 4 {
		t.Fatalf("lease columns after down = %d (%v), want v1's 4", cols, err)
	}
	run("000002_detached_leases.up.sql") // and back up over the leftover row
	if err := CheckSchema(ctx, db); err != nil {
		t.Fatalf("re-applied v2: %v", err)
	}
}

func TestValidLeaseID(t *testing.T) {
	for id, want := range map[string]bool{
		"0123abcd-ef01-4abc-89de-ABCDEF012345": true,
		"0123abcd-ef01-4abc-89de-abcdef01234":  false,
		"0123abcd-ef01-4abc-89de_abcdef012345": false,
		"0123abcdXef01-4abc-89de-abcdef012345": false,
		"0123abcg-ef01-4abc-89de-abcdef012345": false,
		"0123abcdef014abc89deabcdef012345":     false,
		"":                                     false,
	} {
		if got := validLeaseID(id); got != want {
			t.Errorf("validLeaseID(%q) = %v, want %v", id, got, want)
		}
	}
}

// TestRenewAfterLockWaitPastExpiry: a renewal that waits on the lease row's
// lock until after the lease expired must find it lost, not extend it. A
// lock-only holder (AdjustLease's FOR UPDATE) rolling back doesn't make
// PostgreSQL recheck a predicate evaluated before the wait, so liveness is
// checked on a clock read after the lock.
func TestRenewAfterLockWaitPastExpiry(t *testing.T) {
	db := openMain(t)
	ttl := 300 * time.Millisecond
	l := newLimiter(t, db, Config{LeaseTTL: ttl})
	name := bucketName(t)
	ensure(t, l, name, bucket.Limits{MaxConcurrency: 1})
	dl := mustDetach(t, l, name, 0)

	holder := begin(t, openMain(t))
	if _, err := holder.Exec(`SELECT 1 FROM bucket.lease WHERE id = $1 FOR UPDATE`, dl.ID); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := l.RenewLease(context.Background(), name, dl.ID)
		result <- err
	}()
	time.Sleep(ttl + 200*time.Millisecond) // the renewal waits; the lease expires
	if err := holder.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, bucket.ErrLeaseLost) {
		t.Fatalf("RenewLease after a lock wait past expiry = %v, want ErrLeaseLost", err)
	}
	if r := readLease(t, db, dl.ID); r.present && r.expiresAt.After(time.Now()) {
		t.Fatalf("expired lease was extended to %v", r.expiresAt)
	}
}

// A failure while ctx is done is a refusal only when nothing committed; a
// failed COMMIT (marked unknown by inTx) stays a plain error.
func TestDetachedAcquireErrClassification(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	dbErr := errors.New("connection lost")
	var le *bucket.LimitError
	if err := detachedAcquireErr(canceled, "k", dbErr); !errors.As(err, &le) || !errors.Is(err, context.Canceled) {
		t.Fatalf("rolled back while canceled = %v, want a refusal with the Cause", err)
	}
	if err := detachedAcquireErr(canceled, "k", core.Unknown(dbErr)); errors.As(err, &le) || !errors.Is(err, dbErr) {
		t.Fatalf("failed COMMIT while canceled = %v, want the plain error", err)
	}
	if err := detachedAcquireErr(context.Background(), "k", dbErr); err != dbErr { //nolint:errorlint // identity
		t.Fatalf("live ctx = %v, want the error as is", err)
	}
}

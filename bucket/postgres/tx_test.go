package postgres

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/SnapdragonPartners/maestro-llms/bucket"
)

// --- caller-owned transactions (ADR-0046) ---------------------------------

// failTx fails the test on any use: EnsureTx must validate before touching
// the caller's transaction.
type failTx struct{ t *testing.T }

func (f failTx) Exec(context.Context, string, ...any) (int64, error) {
	f.t.Fatal("Exec called")
	return 0, nil
}

func (f failTx) QueryRow(context.Context, string, ...any) Row {
	f.t.Fatal("QueryRow called")
	return nil
}

// TestEnsureTxValidatesFirst runs without Postgres.
func TestEnsureTxValidatesFirst(t *testing.T) {
	l := &Limiter{}
	ctx := context.Background()
	// Errors name EnsureTx, not Ensure.
	if err := l.EnsureTx(ctx, failTx{t}, "", bucket.Limits{MaxConcurrency: 1}, true); err == nil || !strings.Contains(err.Error(), "EnsureTx") {
		t.Fatalf("empty name: %v, want an EnsureTx error", err)
	}
	if err := l.EnsureTx(ctx, failTx{t}, "k", bucket.Limits{TokensPerMinute: -1}, true); err == nil || !strings.Contains(err.Error(), "EnsureTx") {
		t.Fatalf("invalid limits: %v, want an EnsureTx error", err)
	}
	if err := l.Ensure(ctx, "", bucket.Limits{MaxConcurrency: 1}, true); err == nil || strings.Contains(err.Error(), "EnsureTx") {
		t.Fatalf("Ensure empty name: %v, want an Ensure error", err)
	}
	// A nil Tx, including SQLTx(nil), is an error, not a panic.
	for _, tx := range []Tx{nil, SQLTx(nil)} {
		if err := l.EnsureTx(ctx, tx, "k", bucket.Limits{MaxConcurrency: 1}, true); err == nil {
			t.Fatal("nil Tx accepted")
		}
		if _, err := l.LockLimits(ctx, tx, "k"); err == nil {
			t.Fatal("LockLimits accepted a nil Tx")
		}
	}
}

type bucketState struct {
	lastRefill time.Time
	limits     bucket.Limits
	balance    float64
}

func readBucket(t *testing.T, db *sql.DB, name string) bucketState {
	t.Helper()
	var s bucketState
	if err := db.QueryRow(`SELECT tokens_per_minute, max_concurrency, buffer_factor, token_balance, last_refill FROM bucket.bucket WHERE name = $1`, name).Scan(
		&s.limits.TokensPerMinute, &s.limits.MaxConcurrency, &s.limits.BufferFactor, &s.balance, &s.lastRefill); err != nil {
		t.Fatalf("read bucket %q: %v", name, err)
	}
	return s
}

func begin(t *testing.T, db *sql.DB) *sql.Tx {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

// TestEnsureTxMatchesEnsure: committed, EnsureTx leaves the same row as
// Ensure for every balance rule.
func TestEnsureTxMatchesEnsure(t *testing.T) {
	db := openMain(t)
	l := newLimiter(t, db, Config{})
	ctx := context.Background()
	cases := []struct {
		setup       string // SQL run on each bucket before the change; $1 is the name
		name        string
		from, to    bucket.Limits
		wantBalance float64
	}{
		{name: "raised", from: bucket.Limits{TokensPerMinute: 1000, MaxConcurrency: 2}, to: bucket.Limits{TokensPerMinute: 2000, MaxConcurrency: 2}, wantBalance: 900},
		{name: "lowered", from: bucket.Limits{TokensPerMinute: 1000}, to: bucket.Limits{TokensPerMinute: 100}, wantBalance: 90},
		{name: "token limiting on", from: bucket.Limits{MaxConcurrency: 5}, to: bucket.Limits{TokensPerMinute: 100, BufferFactor: 1}, wantBalance: 100},
		{name: "token limiting off", from: bucket.Limits{TokensPerMinute: 100}, to: bucket.Limits{MaxConcurrency: 1}, wantBalance: 0},
		{
			name:        "debt kept",
			setup:       `UPDATE bucket.bucket SET token_balance = -600, last_refill = clock_timestamp() - interval '60 seconds' WHERE name = $1`,
			from:        bucket.Limits{TokensPerMinute: 60, BufferFactor: 1},
			to:          bucket.Limits{TokensPerMinute: 600, BufferFactor: 1},
			wantBalance: -540,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, b := bucketName(t), bucketName(t)
			for _, n := range []string{a, b} {
				if err := l.Ensure(ctx, n, tc.from, true); err != nil {
					t.Fatal(err)
				}
				if tc.setup != "" {
					if _, err := db.Exec(tc.setup, n); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := l.Ensure(ctx, a, tc.to, true); err != nil {
				t.Fatal(err)
			}
			tx := begin(t, db)
			if err := l.EnsureTx(ctx, SQLTx(tx), b, tc.to, true); err != nil {
				t.Fatalf("EnsureTx: %v", err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			got, want := readBucket(t, db, b), readBucket(t, db, a)
			if got.limits != want.limits {
				t.Fatalf("EnsureTx limits = %+v, Ensure = %+v", got.limits, want.limits)
			}
			if math.Abs(got.balance-tc.wantBalance) > 2 || math.Abs(want.balance-tc.wantBalance) > 2 {
				t.Fatalf("balance EnsureTx = %v, Ensure = %v, want about %v", got.balance, want.balance, tc.wantBalance)
			}
		})
	}

	// A missing bucket is created full, and overwrite=false leaves an
	// existing one alone.
	name := bucketName(t)
	tx := begin(t, db)
	if err := l.EnsureTx(ctx, SQLTx(tx), name, bucket.Limits{TokensPerMinute: 1000}, false); err != nil {
		t.Fatal(err)
	}
	if err := l.EnsureTx(ctx, SQLTx(tx), name, bucket.Limits{TokensPerMinute: 5}, false); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if s := readBucket(t, db, name); s.limits.TokensPerMinute != 1000 || s.balance != 900 {
		t.Fatalf("created bucket = %+v, want TPM 1000 starting full at 900", s)
	}
}

// TestEnsureTxRollsBack: rolling back the caller's transaction undoes both
// an overwrite and a create, and EnsureTx itself never commits.
func TestEnsureTxRollsBack(t *testing.T) {
	db := openMain(t)
	l := newLimiter(t, db, Config{})
	ctx := context.Background()
	existing, created := bucketName(t), bucketName(t)
	ensure(t, l, existing, bucket.Limits{TokensPerMinute: 1000, MaxConcurrency: 2})
	before := readBucket(t, db, existing)

	tx := begin(t, db)
	if err := l.EnsureTx(ctx, SQLTx(tx), existing, bucket.Limits{TokensPerMinute: 10, MaxConcurrency: 9}, true); err != nil {
		t.Fatal(err)
	}
	if err := l.EnsureTx(ctx, SQLTx(tx), created, bucket.Limits{MaxConcurrency: 1}, false); err != nil {
		t.Fatal(err)
	}
	// Not visible outside tx until the caller commits.
	if _, err := l.Limits(ctx, created); !errors.Is(err, bucket.ErrUnknownKey) {
		t.Fatalf("uncommitted bucket visible: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	after := readBucket(t, db, existing)
	if after.limits != before.limits || after.balance != before.balance || !after.lastRefill.Equal(before.lastRefill) {
		t.Fatalf("row after rollback = %+v, want %+v", after, before)
	}
	if _, err := l.Limits(ctx, created); !errors.Is(err, bucket.ErrUnknownKey) {
		t.Fatalf("rolled-back bucket exists: %v", err)
	}
}

// TestEnsureTxLeavesCallerSettings: EnsureTx sets no lock_timeout in the
// caller's transaction.
func TestEnsureTxLeavesCallerSettings(t *testing.T) {
	db := openMain(t)
	l := newLimiter(t, db, Config{})
	name := bucketName(t)
	ensure(t, l, name, bucket.Limits{MaxConcurrency: 1})
	tx := begin(t, db)
	if _, err := tx.Exec(`SET LOCAL lock_timeout = '7s'`); err != nil {
		t.Fatal(err)
	}
	if err := l.EnsureTx(context.Background(), SQLTx(tx), name, bucket.Limits{MaxConcurrency: 2}, true); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := tx.QueryRow(`SHOW lock_timeout`).Scan(&got); err != nil || got != "7s" {
		t.Fatalf("lock_timeout after EnsureTx = %q (%v), want the caller's 7s", got, err)
	}
}

// TestLimitsAndLockLimits: both read the stored limits (the effective
// BufferFactor, never 0) and agree; a missing bucket is an unknown key and
// leaves the caller's transaction usable.
func TestLimitsAndLockLimits(t *testing.T) {
	db := openMain(t)
	l := newLimiter(t, db, Config{})
	ctx := context.Background()
	name := bucketName(t)
	ensure(t, l, name, bucket.Limits{TokensPerMinute: 1000, MaxConcurrency: 2})
	want := bucket.Limits{TokensPerMinute: 1000, MaxConcurrency: 2, BufferFactor: bucket.DefaultBufferFactor}
	got, err := l.Limits(ctx, name)
	if err != nil || got != want {
		t.Fatalf("Limits = %+v, %v; want %+v", got, err, want)
	}
	tx := begin(t, db)
	locked, err := l.LockLimits(ctx, SQLTx(tx), name)
	if err != nil || locked != got {
		t.Fatalf("LockLimits = %+v, %v; want Limits' %+v", locked, err, got)
	}

	missing := bucketName(t)
	if _, err := l.Limits(ctx, missing); !errors.Is(err, bucket.ErrUnknownKey) {
		t.Fatalf("Limits on a missing bucket = %v, want ErrUnknownKey", err)
	}
	if _, err := l.LockLimits(ctx, SQLTx(tx), missing); !errors.Is(err, bucket.ErrUnknownKey) {
		t.Fatalf("LockLimits on a missing bucket = %v, want ErrUnknownKey", err)
	}
	var one int
	if err := tx.QueryRow(`SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("caller's transaction unusable after a missing bucket: %v", err)
	}
}

// TestLockLimitsBlocksAcquire: the row lock serializes a save against
// Acquire until the caller commits.
func TestLockLimitsBlocksAcquire(t *testing.T) {
	db := openMain(t)
	l := newLimiter(t, db, Config{MaxWait: 10 * time.Second})
	name := bucketName(t)
	ensure(t, l, name, bucket.Limits{MaxConcurrency: 1})
	tx := begin(t, db)
	if _, err := l.LockLimits(context.Background(), SQLTx(tx), name); err != nil {
		t.Fatal(err)
	}
	type result struct {
		lease bucket.Lease
		err   error
	}
	got := make(chan result, 1)
	go func() {
		ls, err := l.Acquire(context.Background(), name, 0)
		got <- result{ls, err}
	}()
	select {
	case r := <-got:
		t.Fatalf("Acquire returned under the lock: %v", r.err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := l.EnsureTx(context.Background(), SQLTx(tx), name, bucket.Limits{MaxConcurrency: 2}, true); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	r := <-got
	if r.err != nil {
		t.Fatalf("Acquire after commit: %v", r.err)
	}
	_ = r.lease.Release(context.Background())
}

// TestStaleSaveDetected is the admin-page flow: read, another admin saves,
// then lock, compare and refuse.
func TestStaleSaveDetected(t *testing.T) {
	db := openMain(t)
	l := newLimiter(t, db, Config{})
	ctx := context.Background()
	name := bucketName(t)
	ensure(t, l, name, bucket.Limits{TokensPerMinute: 1000, MaxConcurrency: 2})
	drawn, err := l.Limits(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	ensure(t, l, name, bucket.Limits{TokensPerMinute: 500, MaxConcurrency: 2}) // the other admin
	tx := begin(t, db)
	cur, err := l.LockLimits(ctx, SQLTx(tx), name)
	if err != nil {
		t.Fatal(err)
	}
	// Field by field, as a consumer must: bucket.Limits is not promised
	// comparable.
	if cur.TokensPerMinute == drawn.TokensPerMinute && cur.MaxConcurrency == drawn.MaxConcurrency && cur.BufferFactor == drawn.BufferFactor {
		t.Fatalf("locked limits %+v equal the drawn ones after a concurrent save", cur)
	}
}

// pgxTx is the adapter from Tx's doc comment, over a native pgx.Tx.
type pgxTx struct{ tx pgx.Tx }

func (t pgxTx) Exec(ctx context.Context, q string, args ...any) (int64, error) {
	tag, err := t.tx.Exec(ctx, q, args...)
	return tag.RowsAffected(), err
}

func (t pgxTx) QueryRow(ctx context.Context, q string, args ...any) Row {
	return t.tx.QueryRow(ctx, q, args...)
}

// TestNativePgxTx: the documented pgx adapter works, including a missing
// bucket (pgx.ErrNoRows wraps sql.ErrNoRows).
func TestNativePgxTx(t *testing.T) {
	db := openMain(t)
	l := newLimiter(t, db, Config{})
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, mainDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	ptx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ptx.Rollback(ctx) }()
	tx := pgxTx{ptx}
	name := bucketName(t)
	if _, err = l.LockLimits(ctx, tx, name); !errors.Is(err, bucket.ErrUnknownKey) {
		t.Fatalf("LockLimits on a missing bucket over pgx = %v, want ErrUnknownKey", err)
	}
	if err = l.EnsureTx(ctx, tx, name, bucket.Limits{TokensPerMinute: 1000}, false); err != nil {
		t.Fatal(err)
	}
	if err = l.EnsureTx(ctx, tx, name, bucket.Limits{TokensPerMinute: 100, MaxConcurrency: 3}, true); err != nil {
		t.Fatal(err)
	}
	locked, err := l.LockLimits(ctx, tx, name)
	if err != nil {
		t.Fatal(err)
	}
	if err = ptx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := l.Limits(ctx, name)
	if err != nil || got != locked || got.TokensPerMinute != 100 || got.MaxConcurrency != 3 {
		t.Fatalf("Limits after a pgx save = %+v, %v; LockLimits saw %+v", got, err, locked)
	}
	if s := readBucket(t, db, name); s.balance != 90 {
		t.Fatalf("balance = %v, want 90 (capped at the new capacity)", s.balance)
	}
}

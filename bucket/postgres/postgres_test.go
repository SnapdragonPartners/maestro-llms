package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // test-only driver

	"github.com/SnapdragonPartners/maestro-llms/bucket"
)

// The Postgres tests run against a real server and skip unless
// BUCKET_POSTGRES_DSN is set to a URL-form DSN for a role that may create
// databases (CI uses a service container). Each run creates its own
// databases and drops them afterwards. Locally, for example:
//
//	docker run -d --name bucket-pg -e POSTGRES_PASSWORD=bucket -p 127.0.0.1:55499:5432 postgres:17
//	BUCKET_POSTGRES_DSN='postgres://postgres:bucket@127.0.0.1:55499/postgres?sslmode=disable' make test

var _ bucket.Limiter = (*Limiter)(nil)

var (
	adminDSN string
	mainDSN  string
)

func TestMain(m *testing.M) {
	adminDSN = os.Getenv("BUCKET_POSTGRES_DSN")
	if adminDSN == "" {
		// CI sets BUCKET_POSTGRES_REQUIRED so a broken service config fails
		// instead of silently skipping every Postgres test.
		if os.Getenv("BUCKET_POSTGRES_REQUIRED") != "" {
			fmt.Fprintln(os.Stderr, "bucket postgres tests: BUCKET_POSTGRES_REQUIRED is set but BUCKET_POSTGRES_DSN is empty")
			os.Exit(1)
		}
		os.Exit(m.Run())
	}
	dsn, drop, err := createDatabase("bucket_test_main")
	if err != nil {
		fmt.Fprintln(os.Stderr, "bucket postgres tests:", err)
		os.Exit(1)
	}
	mainDSN = dsn
	if err := withDB(dsn, applyMigrations); err != nil {
		fmt.Fprintln(os.Stderr, "bucket postgres tests: migrate:", err)
		drop()
		os.Exit(1)
	}
	code := m.Run()
	drop()
	os.Exit(code)
}

// createDatabase creates a fresh, uniquely named database and returns its
// DSN and a func that drops it.
func createDatabase(prefix string) (string, func(), error) {
	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		return "", nil, err
	}
	name := fmt.Sprintf("%s_%d_%d", prefix, os.Getpid(), time.Now().UnixNano())
	if _, err = admin.Exec("CREATE DATABASE " + name); err != nil {
		admin.Close()
		return "", nil, err
	}
	u, err := url.Parse(adminDSN)
	if err != nil {
		admin.Close()
		return "", nil, err
	}
	u.Path = "/" + name
	drop := func() {
		_, _ = admin.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
		admin.Close()
	}
	return u.String(), drop, nil
}

func withDB(dsn string, fn func(*sql.DB) error) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	return fn(db)
}

// applyMigrations applies every up migration in order, as an app's
// migration tool would.
func applyMigrations(db *sql.DB) error {
	ups, err := fs.Glob(Migrations(), "*.up.sql")
	if err != nil {
		return err
	}
	sort.Strings(ups)
	for _, name := range ups {
		body, err := fs.ReadFile(Migrations(), name)
		if err != nil {
			return err
		}
		if _, err := db.Exec(string(body)); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

func requireDB(t *testing.T) {
	t.Helper()
	if adminDSN == "" {
		t.Skip("BUCKET_POSTGRES_DSN not set")
	}
}

// openMain opens a new handle on the shared, migrated test database; each
// handle stands in for one app instance.
func openMain(t *testing.T) *sql.DB {
	t.Helper()
	requireDB(t)
	db, err := sql.Open("pgx", mainDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newLimiter(t *testing.T, db *sql.DB, cfg Config) *Limiter {
	t.Helper()
	l, err := New(context.Background(), db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

var nameSeq atomic.Int64

// bucketName is a unique bucket key per call, so tests sharing the database
// (including -count reruns) never touch each other's rows.
func bucketName(t *testing.T) string {
	base := regexp.MustCompile(`[^A-Za-z0-9]+`).ReplaceAllString(t.Name(), "_")
	return fmt.Sprintf("%s_%d", base, nameSeq.Add(1))
}

func ensure(t *testing.T, l *Limiter, name string, lim bucket.Limits) {
	t.Helper()
	if err := l.Ensure(context.Background(), name, lim, true); err != nil {
		t.Fatal(err)
	}
}

func mustAcquire(t *testing.T, l *Limiter, key string, tokens int) *lease {
	t.Helper()
	got, err := l.Acquire(context.Background(), key, tokens)
	if err != nil {
		t.Fatalf("Acquire(%q, %d): %v", key, tokens, err)
	}
	return got.(*lease) //nolint:forcetypeassert // test
}

func isLimitErr(err error) bool {
	var le *bucket.LimitError
	return errors.As(err, &le)
}

// --- schema --------------------------------------------------------------

func TestMigrationsMatchSchemaVersion(t *testing.T) {
	ups, err := fs.Glob(Migrations(), "*.up.sql")
	if err != nil || len(ups) == 0 {
		t.Fatalf("no up migrations: %v", err)
	}
	highest := 0
	for _, name := range ups {
		n, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil {
			t.Fatalf("%s: not golang-migrate naming: %v", name, err)
		}
		highest = max(highest, n)
		down := strings.TrimSuffix(name, ".up.sql") + ".down.sql"
		if _, err := fs.Stat(Migrations(), down); err != nil {
			t.Errorf("%s has no %s", name, down)
		}
		body, _ := fs.ReadFile(Migrations(), name)
		want := fmt.Sprintf("INSERT INTO bucket.schema_version (version) VALUES (%d);", n)
		if !strings.HasSuffix(strings.TrimSpace(string(body)), want) {
			t.Errorf("%s must end by recording its version: %q", name, want)
		}
	}
	if highest != SchemaVersion {
		t.Fatalf("SchemaVersion = %d but the highest migration is %d", SchemaVersion, highest)
	}
}

func TestNewChecksSchema(t *testing.T) {
	requireDB(t)
	dsn, drop, err := createDatabase("bucket_test_schema")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(drop)
	db, _ := sql.Open("pgx", dsn)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()

	var se *SchemaError
	if _, err := New(ctx, db, Config{}); !errors.As(err, &se) || se.Found != 0 || se.Required != SchemaVersion {
		t.Fatalf("missing schema: %v", err)
	}
	if err := applyMigrations(db); err != nil {
		t.Fatal(err)
	}
	if err := CheckSchema(ctx, db); err != nil {
		t.Fatalf("migrated: %v", err)
	}
	// A newer schema is accepted (migrations are expand-only).
	if _, err := db.Exec(`INSERT INTO bucket.schema_version (version) VALUES ($1)`, SchemaVersion+1); err != nil {
		t.Fatal(err)
	}
	if _, err := New(ctx, db, Config{}); err != nil {
		t.Fatalf("newer schema rejected: %v", err)
	}
	// No version rows at all reads as missing.
	if _, err := db.Exec(`DELETE FROM bucket.schema_version`); err != nil {
		t.Fatal(err)
	}
	if err := CheckSchema(ctx, db); !errors.As(err, &se) {
		t.Fatalf("empty version table: %v", err)
	}
}

func TestAcquireMapsMissingTableToSchemaError(t *testing.T) {
	requireDB(t)
	dsn, drop, err := createDatabase("bucket_test_drift")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(drop)
	db, _ := sql.Open("pgx", dsn)
	t.Cleanup(func() { db.Close() })
	if err := applyMigrations(db); err != nil {
		t.Fatal(err)
	}
	l := newLimiter(t, db, Config{})
	ensure(t, l, "k", bucket.Limits{MaxConcurrency: 1})
	if _, err := db.Exec(`DROP TABLE bucket.lease`); err != nil {
		t.Fatal(err)
	}
	var se *SchemaError
	if _, err := l.Acquire(context.Background(), "k", 0); !errors.As(err, &se) || se.Found != -1 {
		t.Fatalf("Acquire on a dropped table: %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	if err := (Config{LeaseTTL: time.Minute, MaxLeaseDuration: time.Second}).withDefaults().validate(); err == nil {
		t.Fatal("MaxLeaseDuration <= LeaseTTL accepted")
	}
	if _, err := New(context.Background(), nil, Config{}); err == nil {
		t.Fatal("nil db accepted")
	}
}

// --- admission -----------------------------------------------------------

func TestUnknownKeyAndImpossible(t *testing.T) {
	db := openMain(t)
	l := newLimiter(t, db, Config{})
	name := bucketName(t)
	if _, err := l.Acquire(context.Background(), name, 1); !errors.Is(err, bucket.ErrUnknownKey) {
		t.Fatalf("unknown key: %v", err)
	}
	ensure(t, l, name, bucket.Limits{TokensPerMinute: 100, BufferFactor: 1})
	start := time.Now()
	if _, err := l.Acquire(context.Background(), name, 101); !isLimitErr(err) || time.Since(start) > time.Second {
		t.Fatalf("impossible: %v after %v", err, time.Since(start))
	}
}

// TestConcurrencyCapAcrossInstances: many goroutines across several handles
// (instances) never hold more than MaxConcurrency leases at once.
func TestConcurrencyCapAcrossInstances(t *testing.T) {
	name := bucketName(t)
	limiters := make([]*Limiter, 4)
	for i := range limiters {
		limiters[i] = newLimiter(t, openMain(t), Config{PollInterval: 5 * time.Millisecond, MaxPollInterval: 20 * time.Millisecond})
	}
	ensure(t, limiters[0], name, bucket.Limits{MaxConcurrency: 2})
	var inFlight, peak, done atomic.Int32
	var wg sync.WaitGroup
	for i := range 24 {
		l := limiters[i%len(limiters)]
		wg.Go(func() {
			ls, err := l.Acquire(context.Background(), name, 0)
			if err != nil {
				t.Error(err)
				return
			}
			n := inFlight.Add(1)
			for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
			}
			time.Sleep(15 * time.Millisecond)
			inFlight.Add(-1)
			if err := ls.Release(context.Background()); err != nil {
				t.Error(err)
			}
			done.Add(1)
		})
	}
	wg.Wait()
	if p := peak.Load(); p > 2 || p == 0 || done.Load() != 24 {
		t.Fatalf("peak %d, done %d; want peak 1..2 and 24 done", p, done.Load())
	}
}

func TestTokenRefillAndAdjust(t *testing.T) {
	l := newLimiter(t, openMain(t), Config{MaxWait: 50 * time.Millisecond})
	name := bucketName(t)
	ensure(t, l, name, bucket.Limits{TokensPerMinute: 600, BufferFactor: 1}) // capacity 600, 10/s
	ctx := context.Background()
	first := mustAcquire(t, l, name, 600)
	_, err := l.Acquire(ctx, name, 100)
	var le *bucket.LimitError
	if !errors.As(err, &le) || le.RetryAfter < 8*time.Second || le.RetryAfter > 10*time.Second {
		t.Fatalf("empty bucket: %v", err)
	}
	if err := first.Adjust(ctx, 300); err != nil { // refund 300
		t.Fatal(err)
	}
	second := mustAcquire(t, l, name, 100)
	if err := second.Adjust(ctx, 1000); err != nil { // charge 900 more: debt
		t.Fatal(err)
	}
	var balance float64
	if err := openMain(t).QueryRow(`SELECT token_balance FROM bucket.bucket WHERE name = $1`, name).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if balance > -690 || balance < -720 {
		t.Fatalf("balance = %v, want about -700", balance)
	}
	_ = first.Release(ctx)
	_ = second.Release(ctx)
}

// --- leases --------------------------------------------------------------

func TestRenewalKeepsLeaseAlive(t *testing.T) {
	l := newLimiter(t, openMain(t), Config{LeaseTTL: 600 * time.Millisecond, MaxWait: 100 * time.Millisecond})
	name := bucketName(t)
	ensure(t, l, name, bucket.Limits{MaxConcurrency: 1})
	held := mustAcquire(t, l, name, 0)
	time.Sleep(2 * time.Second) // several TTLs
	if _, err := l.Acquire(context.Background(), name, 0); !isLimitErr(err) {
		t.Fatalf("slot reclaimed while renewed: %v", err)
	}
	if err := held.Release(context.Background()); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

// TestCrashRecovery: a holder whose renewal stops (the process died) has its
// slot reclaimed after one TTL.
func TestCrashRecovery(t *testing.T) {
	ttl := 600 * time.Millisecond
	l := newLimiter(t, openMain(t), Config{LeaseTTL: ttl, MaxWait: 5 * time.Second, PollInterval: 20 * time.Millisecond, MaxPollInterval: 50 * time.Millisecond})
	name := bucketName(t)
	ensure(t, l, name, bucket.Limits{MaxConcurrency: 1})
	crashed := mustAcquire(t, l, name, 0)
	crashed.cancel() // the process dies: renewal stops, nothing is deleted
	<-crashed.done
	start := time.Now()
	next := mustAcquire(t, l, name, 0)
	if el := time.Since(start); el < ttl/3 || el > ttl+time.Second {
		t.Fatalf("reclaimed after %v, want about one TTL (%v)", el, ttl)
	}
	_ = next.Release(context.Background())
}

func TestLostLease(t *testing.T) {
	ttl := 600 * time.Millisecond
	db := openMain(t)
	l := newLimiter(t, db, Config{LeaseTTL: ttl})
	name := bucketName(t)
	ensure(t, l, name, bucket.Limits{MaxConcurrency: 1})
	ls := mustAcquire(t, l, name, 0)
	if _, err := db.Exec(`DELETE FROM bucket.lease WHERE id = $1`, ls.id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ls.Lost():
	case <-time.After(2 * ttl):
		t.Fatal("Lost() not closed after the row disappeared")
	}
	if err := ls.Release(context.Background()); !errors.Is(err, bucket.ErrLeaseLost) {
		t.Fatalf("Release = %v, want ErrLeaseLost", err)
	}
	if err := ls.Release(context.Background()); !errors.Is(err, bucket.ErrLeaseLost) {
		t.Fatalf("second Release = %v, want the same result", err)
	}
}

// TestMaxLeaseDuration: a holder that never releases loses its slot after
// MaxLeaseDuration plus one TTL, not never.
func TestMaxLeaseDuration(t *testing.T) {
	ttl, maxLease := 300*time.Millisecond, 900*time.Millisecond
	l := newLimiter(t, openMain(t), Config{LeaseTTL: ttl, MaxLeaseDuration: maxLease, MaxWait: 5 * time.Second, PollInterval: 20 * time.Millisecond, MaxPollInterval: 50 * time.Millisecond})
	name := bucketName(t)
	ensure(t, l, name, bucket.Limits{MaxConcurrency: 1})
	start := time.Now()
	leaked := mustAcquire(t, l, name, 0) // never released
	next := mustAcquire(t, l, name, 0)
	if el := time.Since(start); el < maxLease-ttl || el > maxLease+ttl+time.Second {
		t.Fatalf("leaked slot reclaimed after %v, want about %v", el, maxLease+ttl)
	}
	_ = next.Release(context.Background())
	_ = leaked.Release(context.Background())
}

// TestAcquireAfterLongLockWait: an acquire that waits on the bucket lock for
// longer than LeaseTTL still returns a live lease (time is sampled after the
// lock, not at transaction start).
func TestAcquireAfterLongLockWait(t *testing.T) {
	ttl := 500 * time.Millisecond
	db := openMain(t)
	l := newLimiter(t, db, Config{LeaseTTL: ttl, MaxWait: 100 * time.Millisecond})
	name := bucketName(t)
	ensure(t, l, name, bucket.Limits{MaxConcurrency: 1})

	blocker, err := openMain(t).Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(`SELECT 1 FROM bucket.bucket WHERE name = $1 FOR UPDATE`, name); err != nil {
		t.Fatal(err)
	}
	// The acquire's transaction starts now, then waits on the lock.
	got := make(chan *lease, 1)
	go func() { got <- mustAcquire(t, l, name, 0) }()
	time.Sleep(3 * ttl)
	if err := blocker.Commit(); err != nil {
		t.Fatal(err)
	}
	ls := <-got
	var live bool
	if err := db.QueryRow(`SELECT expires_at > clock_timestamp() + interval '200 milliseconds' FROM bucket.lease WHERE id = $1`, ls.id).Scan(&live); err != nil || !live {
		t.Fatalf("lease after a long lock wait is not live: %v", err)
	}
	if _, err := l.Acquire(context.Background(), name, 0); !isLimitErr(err) {
		t.Fatalf("slot of a fresh lease was reclaimed: %v", err)
	}
	_ = ls.Release(context.Background())
}

// TestOpTimeoutBoundsOperations: Adjust and Release return within about
// OpTimeout when the rows they need are locked, even on a context with no
// deadline; a renewal blocked for less than a TTL is transient.
func TestOpTimeoutBoundsOperations(t *testing.T) {
	op := 300 * time.Millisecond
	db := openMain(t)
	l := newLimiter(t, db, Config{LeaseTTL: 3 * time.Second, OpTimeout: op})
	name := bucketName(t)
	ensure(t, l, name, bucket.Limits{TokensPerMinute: 1000, MaxConcurrency: 2})
	ls := mustAcquire(t, l, name, 10)
	ctx := context.WithoutCancel(context.Background())

	lockRow := func(query string, arg any) *sql.Tx {
		tx, err := openMain(t).Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(query, arg); err != nil {
			t.Fatal(err)
		}
		return tx
	}

	tx := lockRow(`SELECT 1 FROM bucket.bucket WHERE name = $1 FOR UPDATE`, name)
	start := time.Now()
	if err := ls.Adjust(ctx, 5); err == nil || time.Since(start) > 3*op {
		t.Fatalf("Adjust under a held lock: %v after %v", err, time.Since(start))
	}
	_ = tx.Rollback()

	tx = lockRow(`SELECT 1 FROM bucket.lease WHERE id = $1 FOR UPDATE`, ls.id)
	start = time.Now()
	if err := ls.Release(ctx); err == nil || time.Since(start) > 3*op {
		t.Fatalf("Release under a held lock: %v after %v", err, time.Since(start))
	}
	_ = tx.Rollback()

	// Renewal: TTL 900ms renews every 300ms with a 200ms timeout. Block the
	// row for 500ms (two blocked ticks at most) and the lease survives.
	l2 := newLimiter(t, db, Config{LeaseTTL: 900 * time.Millisecond, OpTimeout: 200 * time.Millisecond})
	renewed := mustAcquire(t, l2, name, 0)
	tx = lockRow(`SELECT 1 FROM bucket.lease WHERE id = $1 FOR UPDATE`, renewed.id)
	time.Sleep(500 * time.Millisecond)
	_ = tx.Rollback()
	time.Sleep(1200 * time.Millisecond)
	select {
	case <-renewed.Lost():
		t.Fatal("transiently blocked renewal reported the lease lost")
	default:
	}
	if err := renewed.Release(context.Background()); err != nil {
		t.Fatalf("Release after transient block: %v", err)
	}
}

// --- Ensure --------------------------------------------------------------

func TestEnsure(t *testing.T) {
	db := openMain(t)
	l := newLimiter(t, db, Config{})
	name := bucketName(t)
	ctx := context.Background()
	read := func() (tpm, conc int, balance float64) {
		t.Helper()
		if err := db.QueryRow(`SELECT tokens_per_minute, max_concurrency, token_balance FROM bucket.bucket WHERE name = $1`, name).Scan(&tpm, &conc, &balance); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := l.Ensure(ctx, name, bucket.Limits{TokensPerMinute: 1000, MaxConcurrency: 2}, false); err != nil {
		t.Fatal(err)
	}
	if tpm, conc, bal := read(); tpm != 1000 || conc != 2 || bal != 900 {
		t.Fatalf("new bucket = %d/%d/%v, want 1000/2/900 (starts full)", tpm, conc, bal)
	}
	if err := l.Ensure(ctx, name, bucket.Limits{TokensPerMinute: 5, MaxConcurrency: 9}, false); err != nil {
		t.Fatal(err)
	}
	if tpm, conc, _ := read(); tpm != 1000 || conc != 2 {
		t.Fatalf("overwrite=false changed limits to %d/%d", tpm, conc)
	}
	if err := l.Ensure(ctx, name, bucket.Limits{TokensPerMinute: 100, MaxConcurrency: 3}, true); err != nil {
		t.Fatal(err)
	}
	if tpm, conc, bal := read(); tpm != 100 || conc != 3 || bal != 90 {
		t.Fatalf("overwrite = %d/%d/%v, want 100/3/90 (capped at the new capacity)", tpm, conc, bal)
	}
	if err := l.Ensure(ctx, name, bucket.Limits{TokensPerMinute: -1}, true); err == nil {
		t.Fatal("invalid limits accepted")
	}
}

// TestEnsureSettlesBeforeRateChange is ADR-0025's example: -600 at 60 TPM,
// raised to 600 TPM sixty seconds later, is about -540, not 0.
func TestEnsureSettlesBeforeRateChange(t *testing.T) {
	db := openMain(t)
	l := newLimiter(t, db, Config{})
	name := bucketName(t)
	ensure(t, l, name, bucket.Limits{TokensPerMinute: 60, BufferFactor: 1})
	if _, err := db.Exec(`UPDATE bucket.bucket SET token_balance = -600, last_refill = clock_timestamp() - interval '60 seconds' WHERE name = $1`, name); err != nil {
		t.Fatal(err)
	}
	ensure(t, l, name, bucket.Limits{TokensPerMinute: 600, BufferFactor: 1})
	var bal float64
	if err := db.QueryRow(`SELECT token_balance FROM bucket.bucket WHERE name = $1`, name).Scan(&bal); err != nil {
		t.Fatal(err)
	}
	if math.Abs(bal-(-540)) > 2 {
		t.Fatalf("balance = %v, want about -540", bal)
	}
}

func TestLimitChangeSeenByEveryInstance(t *testing.T) {
	a := newLimiter(t, openMain(t), Config{MaxWait: 100 * time.Millisecond})
	b := newLimiter(t, openMain(t), Config{MaxWait: 100 * time.Millisecond})
	name := bucketName(t)
	ensure(t, a, name, bucket.Limits{MaxConcurrency: 1})
	held := mustAcquire(t, a, name, 0)
	if _, err := b.Acquire(context.Background(), name, 0); !isLimitErr(err) {
		t.Fatalf("over the limit: %v", err)
	}
	ensure(t, a, name, bucket.Limits{MaxConcurrency: 2})
	other := mustAcquire(t, b, name, 0)
	_ = other.Release(context.Background())
	_ = held.Release(context.Background())
}

func TestDeleteCascadesToLeases(t *testing.T) {
	db := openMain(t)
	l := newLimiter(t, db, Config{})
	name := bucketName(t)
	ensure(t, l, name, bucket.Limits{MaxConcurrency: 1})
	ls := mustAcquire(t, l, name, 0)
	if _, err := db.Exec(`DELETE FROM bucket.bucket WHERE name = $1`, name); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM bucket.lease WHERE id = $1`, ls.id).Scan(&n); err != nil || n != 0 {
		t.Fatalf("lease rows after cascade = %d (%v)", n, err)
	}
	if err := ls.Release(context.Background()); !errors.Is(err, bucket.ErrLeaseLost) {
		t.Fatalf("Release = %v, want ErrLeaseLost", err)
	}
	if _, err := l.Acquire(context.Background(), name, 0); !errors.Is(err, bucket.ErrUnknownKey) {
		t.Fatalf("Acquire on a deleted bucket: %v", err)
	}
}

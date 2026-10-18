package postgres

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
)

// SchemaVersion is the bucket schema version this release requires. A
// release that raises it must open its release notes with a "Database
// migration required" section (ADR-0025). A test keeps it equal to the
// highest migration file number.
const SchemaVersion = 1

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migrations returns the versioned schema migrations, named in
// golang-migrate's format (000001_init.up.sql / .down.sql). Apply them with
// your own migration tool; the package never runs DDL. Each up file records
// its version in bucket.schema_version as its last statement.
func Migrations() fs.FS {
	sub, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		// The embed pattern guarantees the directory exists.
		panic(fmt.Sprintf("postgres: embedded migrations: %v", err))
	}
	return sub
}

// SchemaError means the database's bucket schema is missing or older than
// SchemaVersion. Nothing is admitted until the migrations are applied.
type SchemaError struct {
	// Cause is the underlying database error, when there is one.
	Cause error
	// Required is SchemaVersion.
	Required int
	// Found is the version the database reports; 0 when the schema is
	// missing, -1 when unknown (a table or column disappeared at runtime).
	Found int
}

// Error implements error.
func (e *SchemaError) Error() string {
	found := fmt.Sprintf("found v%d", e.Found)
	switch e.Found {
	case 0:
		found = "found none"
	case -1:
		found = "a bucket table or column is missing"
	}
	return fmt.Sprintf("bucket: schema v%d required, %s; apply the files from postgres.Migrations() with your migration tool", e.Required, found)
}

// Unwrap returns the underlying database error.
func (e *SchemaError) Unwrap() error { return e.Cause }

// CheckSchema reports whether db has the bucket schema at SchemaVersion or
// newer. It returns *SchemaError when the schema is missing or older, and
// the database error when the check itself fails. Use it in readiness probes
// and integration tests: unit tests that use fakes pass without the
// migration.
func CheckSchema(ctx context.Context, db *sql.DB) error {
	var v sql.NullInt64
	err := db.QueryRowContext(ctx, `SELECT max(version) FROM bucket.schema_version`).Scan(&v)
	if err != nil {
		if isSchemaMissing(err) {
			return &SchemaError{Required: SchemaVersion, Found: 0, Cause: err}
		}
		return fmt.Errorf("bucket: checking schema version: %w", err)
	}
	if !v.Valid || v.Int64 < SchemaVersion {
		return &SchemaError{Required: SchemaVersion, Found: int(v.Int64)}
	}
	return nil
}

// sqlState is implemented by pgx's *pgconn.PgError and lib/pq's *pq.Error,
// so the package recognizes SQLSTATEs without importing a driver.
type sqlState interface{ SQLState() string }

// isSchemaMissing reports an undefined table, column or schema.
func isSchemaMissing(err error) bool {
	var s sqlState
	if !errors.As(err, &s) {
		return false
	}
	switch s.SQLState() {
	case "42P01", "42703", "3F000": // undefined_table, undefined_column, invalid_schema_name
		return true
	}
	return false
}

// schemaErr maps a missing table or column at runtime to *SchemaError, and
// returns any other error unchanged.
func schemaErr(err error) error {
	if err != nil && isSchemaMissing(err) {
		return &SchemaError{Required: SchemaVersion, Found: -1, Cause: err}
	}
	return err
}

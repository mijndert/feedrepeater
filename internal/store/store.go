// Package store owns the SQLite database: schema, migrations, and every query.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"net/url"
	"time"

	"modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrations are applied in order; the applied count is tracked in SQLite's
// user_version. Never edit an applied entry, only append.
var migrations = []string{
	schemaSQL,
	mustRead("migrations/002_feed_destinations.sql"),
	mustRead("migrations/003_instance_redirect_uri.sql"),
	mustRead("migrations/004_feed_changed_at.sql"),
	mustRead("migrations/005_one_destination_per_kind.sql"),
}

func mustRead(name string) string {
	b, err := migrationFS.ReadFile(name)
	if err != nil {
		panic("store: missing migration " + name + ": " + err.Error())
	}
	return string(b)
}

var ErrNotFound = errors.New("store: not found")

// ErrDuplicateKind reports that the account already has a destination of the
// kind being created. An account gets one of each.
var ErrDuplicateKind = errors.New("store: destination of that kind already exists")

type Store struct {
	db *sql.DB
}

// Open opens the database, applies pragmas and migrations.
//
// The pool is capped at a single connection. At beta scale that removes every
// SQLITE_BUSY path and any chance of a write starving behind a reader, at the
// cost of serialising requests — an acceptable trade until it isn't.
func Open(path string) (*Store, error) {
	dsn := "file:" + url.PathEscape(path) + "?" +
		"_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(on)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_txlock=immediate"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}

	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// sqliteConstraintUnique is SQLITE_CONSTRAINT_UNIQUE, the extended result code
// for a write refused by a unique index.
const sqliteConstraintUnique = 2067

// isUniqueViolation reports whether err is a unique-index violation, so a
// caller can turn one into a sentinel the handlers understand.
func isUniqueViolation(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code() == sqliteConstraintUnique
}

// DB exposes the pool for health checks.
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("store: read user_version: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("store: database is at version %d, binary knows %d", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migration %d: %w", i+1, err)
		}
		// PRAGMA does not accept a bound parameter; i is loop-controlled, not input.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// tx runs fn in a transaction, rolling back on error or panic.
func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Unix()
}

func scanTime(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.Unix(v.Int64, 0).UTC()
	return &t
}

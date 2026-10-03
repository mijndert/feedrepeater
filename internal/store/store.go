// Package store owns the SQLite database: schema, migrations, and every query.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"net/url"
	"runtime"
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
	mustRead("migrations/006_user_preferences.sql"),
	mustRead("migrations/007_shared_feeds.sql"),
	mustRead("migrations/008_mastodon_only.sql"),
	mustRead("migrations/009_many_feeds_and_destinations.sql"),
}

func mustRead(name string) string {
	b, err := migrationFS.ReadFile(name)
	if err != nil {
		panic("store: missing migration " + name + ": " + err.Error())
	}
	return string(b)
}

var ErrNotFound = errors.New("store: not found")

// Per-account limits. They are counts rather than schema constraints, and they
// are enforced inside the transaction that inserts — the single writer
// connection takes the write lock at BEGIN, so a count read there cannot be
// stale by the time the row goes in.
const (
	// MaxFeedsPerAccount is how many feeds one account may follow.
	MaxFeedsPerAccount = 5
	// MaxDestinationsPerAccount is how many destinations one account may hold,
	// of any mix of kinds.
	MaxDestinationsPerAccount = 10
)

// ErrFeedLimit reports that the account already follows MaxFeedsPerAccount
// feeds.
var ErrFeedLimit = errors.New("store: feed limit reached")

// ErrAlreadySubscribed reports that the account already follows the feed being
// added. Adding it twice would be two rows delivering the same entries twice.
var ErrAlreadySubscribed = errors.New("store: already subscribed to that feed")

// ErrDestinationLimit reports that the account already holds
// MaxDestinationsPerAccount destinations.
var ErrDestinationLimit = errors.New("store: destination limit reached")

type Store struct {
	// rw is the writer: exactly one connection, so no write ever waits on
	// another and SQLITE_BUSY cannot happen between our own statements.
	rw *sql.DB
	// ro is the reader pool. WAL lets readers run against the last committed
	// snapshot while a write is in flight, so the only reason they were ever
	// serialised was sharing the single connection. Dashboard reads, the stats
	// scan and the poll queue now overlap with each other and with writing.
	ro *sql.DB
}

// readerConns bounds the reader pool. SQLite readers are cheap — a file handle
// and a page cache each — but every one of them is a page cache, so this tracks
// cores rather than being set large on the theory that more is better.
func readerConns() int {
	n := runtime.NumCPU()
	return min(max(n, 4), 16)
}

func dsn(path string, write bool) string {
	// A writer that waits its turn politely deadlocks against itself under
	// upgrade; immediate takes the write lock at BEGIN. Readers never upgrade,
	// so deferred is both correct and what lets them share the snapshot.
	txlock := "deferred"
	if write {
		txlock = "immediate"
	}
	return "file:" + url.PathEscape(path) + "?" +
		"_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(on)" +
		"&_pragma=synchronous(NORMAL)" +
		// Left unbounded the WAL grows to whatever the busiest moment needed and
		// never gives it back, and Litestream reads the whole file. Checkpoint on
		// the usual page count, then truncate.
		"&_pragma=wal_autocheckpoint(1000)" +
		"&_pragma=journal_size_limit(67108864)" +
		// Negative is KiB rather than pages. The working set here is small and
		// entirely index pages; 16 MiB holds all of it.
		"&_pragma=cache_size(-16000)" +
		"&_txlock=" + txlock
}

// Open opens the database, applies pragmas and migrations.
//
// Two pools are opened over the one file: a single writer and a pool of
// readers. Serialising writes is deliberate and costs nothing at this size;
// serialising reads behind them, which one shared connection also did, is what
// turned a public /stats scan or one slow dashboard into a queue for everybody.
func Open(path string) (*Store, error) {
	rw, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		return nil, err
	}
	rw.SetMaxOpenConns(1)
	rw.SetMaxIdleConns(1)
	rw.SetConnMaxLifetime(0)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := rw.PingContext(ctx); err != nil {
		rw.Close()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}

	s := &Store{rw: rw}
	// Migrations run before the readers exist. Nothing else may be looking at
	// the file while a table is being rebuilt underneath it.
	if err := s.migrate(ctx); err != nil {
		rw.Close()
		return nil, err
	}

	ro, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		rw.Close()
		return nil, err
	}
	ro.SetMaxOpenConns(readerConns())
	ro.SetMaxIdleConns(readerConns())
	ro.SetConnMaxLifetime(0)
	if err := ro.PingContext(ctx); err != nil {
		rw.Close()
		ro.Close()
		return nil, fmt.Errorf("store: open %s for reading: %w", path, err)
	}
	s.ro = ro
	return s, nil
}

func (s *Store) Close() error {
	var first error
	if s.ro != nil {
		first = s.ro.Close()
	}
	if err := s.rw.Close(); err != nil && first == nil {
		first = err
	}
	return first
}

// sqliteConstraintUnique is SQLITE_CONSTRAINT_UNIQUE, the extended result code
// for a write refused by a unique index.
const sqliteConstraintUnique = 2067

// isUniqueViolation reports whether err is a unique-index violation, so a
// caller can turn one into a sentinel the handlers understand.
func isUniqueViolation(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code() == sqliteConstraintUnique
}

// DB exposes the writer for health checks. It is the pool worth probing: a
// reader can be handed out while the writer is wedged, so checking a reader
// would report healthy exactly when the service cannot record anything.
func (s *Store) DB() *sql.DB { return s.rw }

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.rw.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("store: read user_version: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("store: database is at version %d, binary knows %d", version, len(migrations))
	}
	if version == len(migrations) {
		return nil
	}

	// Foreign keys go off for the duration.
	//
	// Rebuilding a table means dropping the old one, and with enforcement on,
	// DROP TABLE runs an implicit delete that fires ON DELETE CASCADE on every
	// child — so dropping `feeds` to give it a new shape would take the items
	// and routes with it. This is SQLite's own documented procedure for altering
	// a table, and the pragma is a no-op inside a transaction, which is why it
	// is set out here rather than at the top of the migration that needs it.
	//
	// legacy_alter_table goes with it: without it, RENAME TO rewrites the
	// foreign-key clauses of other tables to follow the rename. During a rebuild
	// those clauses already name the table being restored, and having them
	// helpfully repointed at the staging name is exactly wrong.
	if _, err := s.rw.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		return err
	}
	if _, err := s.rw.ExecContext(ctx, "PRAGMA legacy_alter_table = ON"); err != nil {
		return err
	}
	defer func() {
		_, _ = s.rw.ExecContext(ctx, "PRAGMA legacy_alter_table = OFF")
		_, _ = s.rw.ExecContext(ctx, "PRAGMA foreign_keys = ON")
	}()

	for i := version; i < len(migrations); i++ {
		if err := s.applyMigration(ctx, i); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) applyMigration(ctx context.Context, i int) error {
	tx, err := s.rw.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
		return fmt.Errorf("store: migration %d: %w", i+1, err)
	}
	// PRAGMA does not accept a bound parameter; i is loop-controlled, not input.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
		return err
	}

	// Enforcement was off while the tables were rebuilt, so nothing has checked
	// that the result hangs together. Ask before committing: a migration that
	// stranded a row is a bug to find here, not six months later when a delete
	// cascades into nothing.
	if err := foreignKeyCheck(ctx, tx); err != nil {
		return fmt.Errorf("store: migration %d: %w", i+1, err)
	}
	return tx.Commit()
}

// foreignKeyCheck reports the first orphaned row the database can find.
func foreignKeyCheck(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		var table, parent string
		var rowid sql.NullInt64
		var fkid int
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return fmt.Errorf("foreign key check failed: %w", err)
		}
		return fmt.Errorf("foreign key check failed: %s row %d has no %s", table, rowid.Int64, parent)
	}
	return rows.Err()
}

// Optimize lets SQLite refresh the statistics its query planner runs on.
//
// Without it the planner works from whatever the table looked like when the
// indexes were built, which for a service that starts empty means it plans
// every query as though nothing has any rows in it. PRAGMA optimize is the
// documented way to ask, and is cheap precisely because it decides for itself
// whether anything is worth re-analysing.
func (s *Store) Optimize(ctx context.Context) error {
	_, err := s.rw.ExecContext(ctx, "PRAGMA optimize")
	return err
}

// tx runs fn in a transaction, rolling back on error or panic.
func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.rw.BeginTx(ctx, nil)
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

package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// openAtVersion builds a database carrying only the first n migrations, so a
// test can write rows in the shape a real deployment had before the migration
// under test and then let Open bring it forward.
func openAtVersion(t *testing.T, path string, n int) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	for i := range n {
		if _, err := db.Exec(migrations[i]); err != nil {
			t.Fatalf("migration %d: %v", i+1, err)
		}
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", n)); err != nil {
		t.Fatal(err)
	}
	return db
}

// Two accounts following the same address were two feed rows, two schedules and
// two copies of every entry. The migration has to fold them onto one shared feed
// without either account losing its history or its send record.
func TestMigrationFoldsDuplicateFeeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db := openAtVersion(t, path, 6)

	const url = "https://example.com/popular.xml"
	for i, acct := range []string{"alice", "bob"} {
		if _, err := db.Exec(`
			INSERT INTO users (id, host, remote_id, acct, access_token, created_at, last_login_at)
			VALUES (?, 'example.social', ?, ?, x'00', 0, 0)`, i+1, acct+"-id", acct); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`
			INSERT INTO feeds (id, user_id, url, title, primed, next_fetch_at, created_at, changed_at)
			VALUES (?, ?, ?, 'Popular', 1, 0, 0, 0)`, i+1, i+1, url); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`
			INSERT INTO destinations (id, user_id, kind, label, created_at)
			VALUES (?, ?, 'mastodon', 'toots', 0)`, i+1, i+1); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`
			INSERT INTO feed_destinations (feed_id, destination_id, user_id, created_at)
			VALUES (?, ?, ?, 0)`, i+1, i+1, i+1); err != nil {
			t.Fatal(err)
		}
	}

	// Both accounts hold the same two entries, under their own feed. Alice also
	// has a third that Bob's copy never picked up.
	items := []struct {
		id, feed int
		guid     string
	}{
		{1, 1, "g1"}, {2, 1, "g2"}, {3, 1, "g3"},
		{4, 2, "g1"}, {5, 2, "g2"},
	}
	for _, it := range items {
		if _, err := db.Exec(`
			INSERT INTO items (id, feed_id, guid, title, seen_at) VALUES (?, ?, ?, 'Entry', 0)`,
			it.id, it.feed, it.guid); err != nil {
			t.Fatal(err)
		}
	}
	// One delivery each, against the copy of "g1" that each account owned.
	for i, itemID := range []int64{1, 4} {
		if _, err := db.Exec(`
			INSERT INTO deliveries (user_id, item_id, destination_id, status, next_attempt_at, created_at, updated_at)
			VALUES (?, ?, ?, 'sent', 0, 0, 0)`, i+1, itemID, i+1); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()

	// One feed, one schedule, both accounts subscribed.
	var feeds, states, subs int
	row := st.ro.QueryRowContext(ctx, `
		SELECT (SELECT count(*) FROM feeds),
		       (SELECT count(*) FROM feed_state),
		       (SELECT count(*) FROM subscriptions)`)
	if err := row.Scan(&feeds, &states, &subs); err != nil {
		t.Fatal(err)
	}
	if feeds != 1 || states != 1 || subs != 2 {
		t.Fatalf("got %d feeds, %d states, %d subscriptions; want 1, 1, 2", feeds, states, subs)
	}

	// Duplicates folded, and the entry only one account had survived.
	var guids int
	if err := st.ro.QueryRowContext(ctx, `SELECT count(*) FROM items`).Scan(&guids); err != nil {
		t.Fatal(err)
	}
	if guids != 3 {
		t.Errorf("got %d items after folding, want 3 (g1, g2, g3)", guids)
	}

	// Neither account lost its send record, and both still point at a real entry.
	for _, u := range []int64{1, 2} {
		list, err := st.RecentDeliveries(ctx, u, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 {
			t.Errorf("user %d has %d deliveries after migration, want 1", u, len(list))
		}
	}

	// And one fetch now serves both.
	due, err := st.DueFeeds(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("got %d due feeds, want 1", len(due))
	}
	if !due[0].HasRoutes {
		t.Error("routes did not survive the fold")
	}
}

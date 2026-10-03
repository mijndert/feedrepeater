package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
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
	for _, u := range []int64{1, 2} {
		if routes, err := st.FeedRoutes(ctx, u, due[0].Feed.ID); err != nil {
			t.Fatal(err)
		} else if len(routes) != 1 {
			t.Errorf("user %d: routes did not survive the fold: %v", u, routes)
		}
	}
}

// Lifting the two beta rules is one migration: the UNIQUE on
// subscriptions.user_id goes with a table rebuild, the per-kind index on
// destinations goes in a statement, and oauth_states learns what a flow is
// for. A database from before it has to come out with its rows intact and the
// new freedoms working — a rebuild that lost a subscription, or a route that
// stopped resolving, would be a feed silently going quiet on deploy.
func TestMigrationLiftsTheOneFeedAndOnePerKindRules(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v8.db")
	db := openAtVersion(t, path, 8)

	if _, err := db.Exec(`
		INSERT INTO users (id, host, remote_id, acct, access_token, created_at, last_login_at)
		VALUES (1, 'example.social', 'alice-id', 'alice', x'00', 0, 0);
		INSERT INTO feeds (id, url, title, created_at) VALUES (1, 'https://example.com/one.xml', 'One', 0);
		INSERT INTO feed_state (feed_id, next_fetch_at) VALUES (1, 0);
		INSERT INTO subscriptions (id, user_id, feed_id, paused, prime_item_id, primed, created_at)
		VALUES (1, 1, 1, 1, 0, 1, 0);
		INSERT INTO destinations (id, user_id, kind, label, config, credentials, created_at)
		VALUES (1, 1, 'mastodon', '@alice@example.social', '{"host":"example.social","acct":"alice","visibility":"public"}', x'0102', 0);
		INSERT INTO feed_destinations (feed_id, destination_id, user_id, created_at) VALUES (1, 1, 1, 0);
		INSERT INTO items (id, feed_id, guid, title, seen_at) VALUES (1, 1, 'g1', 'Entry', 0);
		INSERT INTO deliveries (user_id, item_id, destination_id, status, next_attempt_at, created_at, updated_at)
		VALUES (1, 1, 1, 'sent', 0, 0, 0);`); err != nil {
		t.Fatal(err)
	}
	// The rules being lifted really were in force before the migration, or the
	// test proves nothing about it.
	if _, err := db.Exec(`INSERT INTO subscriptions (user_id, feed_id, created_at) VALUES (1, 2, 0)`); err == nil {
		t.Fatal("precondition: a second subscription was accepted at version 8")
	}
	if _, err := db.Exec(`INSERT INTO destinations (user_id, kind, label, created_at) VALUES (1, 'mastodon', 'x', 0)`); err == nil {
		t.Fatal("precondition: a second mastodon destination was accepted at version 8")
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()

	// Everything the account had is still there, including the per-subscriber
	// columns the rebuild had to copy across.
	feeds, err := st.FeedsByUser(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(feeds) != 1 || feeds[0].ID != 1 || feeds[0].SubscriptionID != 1 {
		t.Fatalf("feeds after migration = %+v, want the one subscription with its id", feeds)
	}
	if !feeds[0].Paused || !feeds[0].Primed {
		t.Errorf("subscription lost its flags in the rebuild: paused %v primed %v", feeds[0].Paused, feeds[0].Primed)
	}
	if routes, err := st.FeedRoutes(ctx, 1, 1); err != nil || len(routes) != 1 || routes[0] != 1 {
		t.Errorf("routes after migration = %v, %v; want [1]", routes, err)
	}
	if list, err := st.RecentDeliveries(ctx, 1, 10); err != nil || len(list) != 1 {
		t.Errorf("deliveries after migration = %d, %v; want 1", len(list), err)
	}

	// The account can now follow a second feed...
	state := FetchState{NextFetchAt: time.Now().UTC()}
	second, _, err := st.Subscribe(ctx, 1, "https://example.com/two.xml", "Two", nil, state)
	if err != nil {
		t.Fatalf("second feed after migration: %v", err)
	}
	if feeds, _ := st.FeedsByUser(ctx, 1); len(feeds) != 2 {
		t.Errorf("account lists %d feeds, want 2", len(feeds))
	}
	// ...and the UNIQUE that matters, one subscription per (user, feed), is
	// still in force rather than having gone with the rebuild.
	if _, _, err := st.Subscribe(ctx, 1, "https://example.com/two.xml", "Two", nil, state); !errors.Is(err, ErrAlreadySubscribed) {
		t.Errorf("subscribing twice after migration = %v, want ErrAlreadySubscribed", err)
	}
	// The new feed picked up the migrated destination, so it posts somewhere.
	if routes, _ := st.FeedRoutes(ctx, 1, second.FeedID); len(routes) != 1 || routes[0] != 1 {
		t.Errorf("new feed routes = %v, want the existing destination", routes)
	}

	// A legacy Mastodon row recorded only its host. Matching is now exact on
	// the account id, so the migration has to fill it in from the user the row
	// belongs to, or the account's own destination would stop being recognised
	// as its own and a second copy would be made at the next sign-in.
	list, err := st.DestinationsByUser(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("account holds %d destinations after migration, want 1", len(list))
	}
	var cfg struct {
		Host       string `json:"host"`
		Acct       string `json:"acct"`
		RemoteID   string `json:"remote_id"`
		Visibility string `json:"visibility"`
	}
	if err := json.Unmarshal([]byte(list[0].Config), &cfg); err != nil {
		t.Fatalf("migrated config %q: %v", list[0].Config, err)
	}
	if cfg.RemoteID != "alice-id" {
		t.Errorf("migrated config carries remote_id %q, want the user's %q", cfg.RemoteID, "alice-id")
	}
	if cfg.Host != "example.social" || cfg.Acct != "alice" || cfg.Visibility != "public" {
		t.Errorf("backfilling remote_id disturbed the rest of the config: %+v", cfg)
	}

	// And hold a second destination of the same kind.
	other := &Destination{UserID: 1, Kind: "mastodon", Label: "@alice@other.social"}
	if err := st.CreateDestination(ctx, other); err != nil {
		t.Fatalf("second mastodon destination after migration: %v", err)
	}
	if list, _ := st.DestinationsByUser(ctx, 1); len(list) != 2 {
		t.Errorf("account holds %d destinations, want 2", len(list))
	}

	// oauth_states gained its purpose and owner, and both round-trip.
	in := OAuthState{Host: "other.social", Purpose: OAuthConnect, UserID: 1}
	if err := st.PutOAuthState(ctx, "hash", in, []byte("v"), time.Minute); err != nil {
		t.Fatalf("PutOAuthState on the migrated table: %v", err)
	}
	got, verifier, err := st.TakeOAuthState(ctx, "hash")
	if err != nil {
		t.Fatalf("TakeOAuthState on the migrated table: %v", err)
	}
	if got != in || string(verifier) != "v" {
		t.Errorf("got %+v %q, want %+v", got, verifier, in)
	}
}

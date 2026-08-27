package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// newDestination adds the account's destination. There is one kind and one per
// account, so a test wanting two of them wants two accounts.
func newDestination(t *testing.T, st *Store, userID int64, label string) *Destination {
	t.Helper()
	d := &Destination{UserID: userID, Kind: "mastodon", Label: label, Template: "{{title}}"}
	if err := st.CreateDestination(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	return d
}

// The zero-configuration path: destinations added after a feed exists start
// connected, so nothing has to be wired up by hand.
func TestNewDestinationIsRoutedToExistingFeeds(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	alice := testUser(t, st, "alice")

	f, _, err := st.Subscribe(ctx, alice.ID, "https://example.com/feed.xml", "", nil, FetchState{NextFetchAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	d := newDestination(t, st, alice.ID, "hook")

	routes, err := st.FeedRoutes(ctx, alice.ID, f.FeedID)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0] != d.ID {
		t.Errorf("routes = %v, want [%d]", routes, d.ID)
	}
}

// And the other order: a feed added later picks up the destination that is
// already there. Both orders happen — the destination is written at sign-in,
// and again when a feed is added if that never worked — so both have to end in
// a routed feed.
func TestNewFeedIsRoutedToExistingDestinations(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	alice := testUser(t, st, "alice")

	d := newDestination(t, st, alice.ID, "@alice@example.social")
	f, _, err := st.Subscribe(ctx, alice.ID, "https://example.com/feed.xml", "", nil, FetchState{NextFetchAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}

	routes, err := st.FeedRoutes(ctx, alice.ID, f.FeedID)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0] != d.ID {
		t.Errorf("routes = %v, want [%d]", routes, d.ID)
	}
}

// A shared feed fans out per subscriber: each account's entry goes to that
// account's own destination and to nobody else's.
func TestQueueDeliveriesFollowsSubscriptions(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	alice := testUser(t, st, "alice")
	bob := testUser(t, st, "bob")

	const url = "https://example.com/popular.xml"
	aliceDest := newDestination(t, st, alice.ID, "@alice@example.social")
	bobDest := newDestination(t, st, bob.ID, "@bob@example.social")
	f, _, _ := st.Subscribe(ctx, alice.ID, url, "", nil, FetchState{NextFetchAt: time.Now().UTC()})
	if _, _, err := st.Subscribe(ctx, bob.ID, url, "", nil, FetchState{NextFetchAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	item := &Item{FeedID: f.FeedID, GUID: "g1", Title: "One"}
	if _, err := st.InsertItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	n, err := st.QueueDeliveries(ctx, f.FeedID, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("queued %d deliveries, want one per subscriber", n)
	}
	for _, want := range []struct {
		user *User
		dest *Destination
	}{{alice, aliceDest}, {bob, bobDest}} {
		views, err := st.RecentDeliveries(ctx, want.user.ID, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(views) != 1 || views[0].DestinationID != want.dest.ID {
			t.Errorf("%s: delivery went to %+v, want destination %d", want.user.Acct, views, want.dest.ID)
		}
	}
}

// An account whose destination could not be written — the instance was
// unreachable at sign-in, and it has not added a feed since — records its
// entries and sends nothing. It must not be an error, and it must not queue a
// delivery with nowhere to go.
func TestFeedWithNoDestinationQueuesNothing(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	alice := testUser(t, st, "alice")

	f, _, _ := st.Subscribe(ctx, alice.ID, "https://example.com/feed.xml", "", nil, FetchState{NextFetchAt: time.Now().UTC()})
	item := &Item{FeedID: f.FeedID, GUID: "g1", Title: "One"}
	if _, err := st.InsertItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	n, err := st.QueueDeliveries(ctx, f.FeedID, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("queued %d deliveries for an account with no destination, want 0", n)
	}
}

// Deleting an account must not leave a dangling route behind. It is the only
// path that removes a destination now — there is no disconnect — and the route
// has to go with it or the feed would be polled for a subscriber that no longer
// exists.
func TestDeletingAnAccountClearsItsRoutes(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	alice := testUser(t, st, "alice")

	f, _, _ := st.Subscribe(ctx, alice.ID, "https://example.com/feed.xml", "", nil, FetchState{NextFetchAt: time.Now().UTC()})
	newDestination(t, st, alice.ID, "@alice@example.social")
	if routes, _ := st.FeedRoutes(ctx, alice.ID, f.FeedID); len(routes) != 1 {
		t.Fatalf("precondition: routes = %v, want one", routes)
	}
	if err := st.DeleteUser(ctx, alice.ID); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, st, "feed_destinations"); n != 0 {
		t.Errorf("%d rows left in feed_destinations", n)
	}
	if n := countRows(t, st, "destinations"); n != 0 {
		t.Errorf("%d rows left in destinations", n)
	}
}

// The upgrade path: a database created before routing existed must come out
// with its existing destinations connected, or every user's feed would go
// silent on deploy.
func TestMigrationBackfillsExistingRouting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")
	ctx := context.Background()

	// Build a database at version 1 only.
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, migrations[0]); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Unix()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (id, host, remote_id, acct, access_token, created_at, last_login_at)
		VALUES (1, 'example.social', 'r1', 'alice', x'00', ?, ?);
		INSERT INTO feeds (id, user_id, url, next_fetch_at, created_at) VALUES (1, 1, 'https://e.com/f.xml', ?, ?);
		INSERT INTO destinations (id, user_id, kind, label, created_at) VALUES (1, 1, 'mastodon', 'old account', ?);
		PRAGMA user_version = 1;`, now, now, now, now, now); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Opening applies migration 2.
	st, err := Open(path)
	if err != nil {
		t.Fatalf("upgrade failed: %v", err)
	}
	defer st.Close()

	routes, err := st.FeedRoutes(ctx, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0] != 1 {
		t.Errorf("routes after upgrade = %v, want [1]; existing deliveries would have stopped", routes)
	}
}

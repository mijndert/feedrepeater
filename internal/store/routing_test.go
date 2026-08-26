package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// newDestination adds a destination for a user. An account gets one per kind,
// so a test wanting two of them gets two different services rather than two
// webhooks.
func newDestination(t *testing.T, st *Store, userID int64, label string) *Destination {
	t.Helper()
	kinds := []string{"webhook", "mastodon", "bluesky"}
	existing, err := st.DestinationsByUser(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(existing) >= len(kinds) {
		t.Fatalf("no kind left for a %d destination account", len(existing)+1)
	}
	d := &Destination{UserID: userID, Kind: kinds[len(existing)], Label: label, Template: "{{title}}"}
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

// And the other order: a feed added later picks up existing destinations.
func TestNewFeedIsRoutedToExistingDestinations(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	alice := testUser(t, st, "alice")

	a := newDestination(t, st, alice.ID, "one")
	b := newDestination(t, st, alice.ID, "two")
	f, _, err := st.Subscribe(ctx, alice.ID, "https://example.com/feed.xml", "", nil, FetchState{NextFetchAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}

	routes, err := st.FeedRoutes(ctx, alice.ID, f.FeedID)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 2 {
		t.Fatalf("routes = %v, want both destinations", routes)
	}
	if routes[0] != a.ID || routes[1] != b.ID {
		t.Errorf("routes = %v, want [%d %d]", routes, a.ID, b.ID)
	}
}

func TestSetFeedRoutesReplacesTheSet(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	alice := testUser(t, st, "alice")

	a := newDestination(t, st, alice.ID, "one")
	b := newDestination(t, st, alice.ID, "two")
	f, _, _ := st.Subscribe(ctx, alice.ID, "https://example.com/feed.xml", "", nil, FetchState{NextFetchAt: time.Now().UTC()})

	if err := st.SetFeedRoutes(ctx, alice.ID, f.FeedID, []int64{b.ID}); err != nil {
		t.Fatal(err)
	}
	routes, _ := st.FeedRoutes(ctx, alice.ID, f.FeedID)
	if len(routes) != 1 || routes[0] != b.ID {
		t.Errorf("routes = %v, want [%d]", routes, b.ID)
	}

	// Clearing the set is allowed: the feed then posts nowhere.
	if err := st.SetFeedRoutes(ctx, alice.ID, f.FeedID, nil); err != nil {
		t.Fatal(err)
	}
	if routes, _ := st.FeedRoutes(ctx, alice.ID, f.FeedID); len(routes) != 0 {
		t.Errorf("routes = %v, want none", routes)
	}

	if err := st.SetFeedRoutes(ctx, alice.ID, f.FeedID, []int64{a.ID, b.ID}); err != nil {
		t.Fatal(err)
	}
	if routes, _ := st.FeedRoutes(ctx, alice.ID, f.FeedID); len(routes) != 2 {
		t.Errorf("routes = %v, want both", routes)
	}
}

// Submitting another account's destination id must attach nothing. This is the
// IDOR that routing introduces, so it is checked directly.
func TestSetFeedRoutesIgnoresForeignDestinations(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	alice := testUser(t, st, "alice")
	mallory := testUser(t, st, "mallory")

	f, _, _ := st.Subscribe(ctx, mallory.ID, "https://example.com/mallory.xml", "", nil, FetchState{NextFetchAt: time.Now().UTC()})
	victim := newDestination(t, st, alice.ID, "Alice's hook")

	if err := st.SetFeedRoutes(ctx, mallory.ID, f.FeedID, []int64{victim.ID}); err != nil {
		t.Fatal(err)
	}
	if routes, _ := st.FeedRoutes(ctx, mallory.ID, f.FeedID); len(routes) != 0 {
		t.Errorf("attached another account's destination: %v", routes)
	}

	// And the feed itself must belong to the caller.
	aliceFeed, _, _ := st.Subscribe(ctx, alice.ID, "https://example.com/alice.xml", "", nil, FetchState{NextFetchAt: time.Now().UTC()})
	if err := st.SetFeedRoutes(ctx, mallory.ID, aliceFeed.FeedID, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("routing another account's feed = %v, want ErrNotFound", err)
	}
	if routes, _ := st.FeedRoutes(ctx, alice.ID, aliceFeed.FeedID); len(routes) != 1 {
		t.Errorf("another account cleared the routes: %v", routes)
	}
}

// Only routed destinations receive entries.
func TestQueueDeliveriesFollowsRouting(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	alice := testUser(t, st, "alice")

	f, _, _ := st.Subscribe(ctx, alice.ID, "https://example.com/feed.xml", "", nil, FetchState{NextFetchAt: time.Now().UTC()})
	wanted := newDestination(t, st, alice.ID, "wanted")
	newDestination(t, st, alice.ID, "not wanted")

	if err := st.SetFeedRoutes(ctx, alice.ID, f.FeedID, []int64{wanted.ID}); err != nil {
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
	if n != 1 {
		t.Fatalf("queued %d deliveries, want 1", n)
	}

	views, err := st.RecentDeliveries(ctx, alice.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].DestinationID != wanted.ID {
		t.Errorf("delivery went to the wrong destination: %+v", views)
	}
}

// A destination that is not routed anywhere receives nothing at all.
func TestUnroutedDestinationReceivesNothing(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	alice := testUser(t, st, "alice")

	f, _, _ := st.Subscribe(ctx, alice.ID, "https://example.com/feed.xml", "", nil, FetchState{NextFetchAt: time.Now().UTC()})
	newDestination(t, st, alice.ID, "hook")
	if err := st.SetFeedRoutes(ctx, alice.ID, f.FeedID, nil); err != nil {
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
	if n != 0 {
		t.Errorf("queued %d deliveries for an unrouted feed, want 0", n)
	}
}

func TestDestinationsForFeedMarksRouting(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	alice := testUser(t, st, "alice")

	f, _, _ := st.Subscribe(ctx, alice.ID, "https://example.com/feed.xml", "", nil, FetchState{NextFetchAt: time.Now().UTC()})
	on := newDestination(t, st, alice.ID, "on")
	off := newDestination(t, st, alice.ID, "off")
	if err := st.SetFeedRoutes(ctx, alice.ID, f.FeedID, []int64{on.ID}); err != nil {
		t.Fatal(err)
	}

	list, err := st.DestinationsForFeed(ctx, alice.ID, f.FeedID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d destinations, want 2", len(list))
	}
	for _, d := range list {
		switch d.ID {
		case on.ID:
			if !d.Routed {
				t.Error("routed destination not marked")
			}
		case off.ID:
			if d.Routed {
				t.Error("unrouted destination marked as routed")
			}
		}
	}

	// Another account's feed id must not reveal or mark anything.
	mallory := testUser(t, st, "mallory")
	other, err := st.DestinationsForFeed(ctx, mallory.ID, f.FeedID)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Errorf("listed %d destinations for another account", len(other))
	}
}

// Deleting a destination must not leave a dangling route behind.
func TestDeletingADestinationClearsItsRoutes(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	alice := testUser(t, st, "alice")

	f, _, _ := st.Subscribe(ctx, alice.ID, "https://example.com/feed.xml", "", nil, FetchState{NextFetchAt: time.Now().UTC()})
	d := newDestination(t, st, alice.ID, "hook")
	if err := st.DeleteDestination(ctx, alice.ID, d.ID); err != nil {
		t.Fatal(err)
	}
	if routes, _ := st.FeedRoutes(ctx, alice.ID, f.FeedID); len(routes) != 0 {
		t.Errorf("routes survived destination deletion: %v", routes)
	}
	if n := countRows(t, st, "feed_destinations"); n != 0 {
		t.Errorf("%d rows left in feed_destinations", n)
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
		INSERT INTO destinations (id, user_id, kind, label, created_at) VALUES (1, 1, 'webhook', 'old hook', ?);
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

package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func testUser(t *testing.T, st *Store, acct string) *User {
	t.Helper()
	u, err := st.UpsertUser(context.Background(), &User{
		Host: "example.social", RemoteID: acct + "-id", Acct: acct,
		AccessToken: []byte("sealed"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestUpsertUserIsIdentifiedByHostAndRemoteID(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	a := testUser(t, st, "alice")
	again, err := st.UpsertUser(ctx, &User{
		Host: "example.social", RemoteID: "alice-id", Acct: "alice-renamed",
		AccessToken: []byte("new"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != a.ID {
		t.Errorf("same account produced a second user: %d vs %d", again.ID, a.ID)
	}

	// The same username on a different instance is a different person.
	other, err := st.UpsertUser(ctx, &User{
		Host: "other.social", RemoteID: "alice-id", Acct: "alice", AccessToken: []byte("x"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if other.ID == a.ID {
		t.Error("accounts on different instances collapsed into one user")
	}
}

// Databases written before the one-per-kind rule can hold duplicates, and the
// unique index cannot be created over them. The migration keeps the oldest of
// each kind, which is the one the account has been using.
func TestMigrationDedupesDestinationsPerKind(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	// A database at the version before the rule existed, holding two webhooks
	// for one account.
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range migrations[:4] {
		if _, err := db.Exec(m); err != nil {
			t.Fatalf("migration %d: %v", i+1, err)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version = 4`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO users (id, host, remote_id, acct, access_token, created_at, last_login_at)
		VALUES (1, 'example.social', 'alice-id', 'alice', x'00', unixepoch(), unixepoch());
		INSERT INTO destinations (id, user_id, kind, label, created_at)
		VALUES (1, 1, 'webhook', 'first', unixepoch()),
		       (2, 1, 'webhook', 'second', unixepoch()),
		       (3, 1, 'mastodon', 'toots', unixepoch());`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("migrating a database with duplicates: %v", err)
	}
	defer st.Close()

	list, err := st.DestinationsByUser(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("account holds %d destinations after the migration, want 2", len(list))
	}
	for _, d := range list {
		if d.Kind == "webhook" && d.Label != "first" {
			t.Errorf("the migration kept %q rather than the oldest webhook", d.Label)
		}
	}
}

// One destination per kind has to hold in the database, not only in the
// handler that checks before it: the handler's check and its insert are
// separated by a webhook verification round trip, so concurrent requests all
// pass it. Two accounts connecting the same service must still be fine.
func TestOneDestinationPerKindIsEnforcedByTheDatabase(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	alice := testUser(t, st, "alice")
	bob := testUser(t, st, "bob")

	first := &Destination{UserID: alice.ID, Kind: "webhook", Label: "hook"}
	if err := st.CreateDestination(ctx, first); err != nil {
		t.Fatal(err)
	}

	second := &Destination{UserID: alice.ID, Kind: "webhook", Label: "another hook"}
	if err := st.CreateDestination(ctx, second); !errors.Is(err, ErrDuplicateKind) {
		t.Errorf("second webhook for one account = %v, want ErrDuplicateKind", err)
	}

	// A different service for the same account, and the same service for a
	// different account, are both untouched by the rule.
	if err := st.CreateDestination(ctx, &Destination{UserID: alice.ID, Kind: "mastodon", Label: "toots"}); err != nil {
		t.Errorf("second kind for one account: %v", err)
	}
	if err := st.CreateDestination(ctx, &Destination{UserID: bob.ID, Kind: "webhook", Label: "bob's hook"}); err != nil {
		t.Errorf("same kind for another account: %v", err)
	}

	// The loser of a race leaves nothing behind.
	list, err := st.DestinationsByUser(ctx, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Errorf("account holds %d destinations, want 2", len(list))
	}
}

// The central multi-tenant property: one user's identifiers must not resolve
// against another user's rows.
func TestDestinationsAreScopedToTheirOwner(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	alice := testUser(t, st, "alice")
	mallory := testUser(t, st, "mallory")

	d := &Destination{UserID: alice.ID, Kind: "webhook", Label: "Alice's hook", Template: "{{title}}"}
	if err := st.CreateDestination(ctx, d); err != nil {
		t.Fatal(err)
	}

	if _, err := st.Destination(ctx, mallory.ID, d.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-tenant read succeeded: %v", err)
	}
	if err := st.DeleteDestination(ctx, mallory.ID, d.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-tenant delete succeeded: %v", err)
	}
	stolen := &Destination{ID: d.ID, UserID: mallory.ID, Kind: "webhook", Label: "taken"}
	if err := st.UpdateDestination(ctx, stolen); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-tenant update succeeded: %v", err)
	}

	got, err := st.Destination(ctx, alice.ID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Label != "Alice's hook" {
		t.Errorf("owner's destination was modified: %q", got.Label)
	}
}

func TestFeedIsOnePerUserAndScoped(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	alice := testUser(t, st, "alice")
	mallory := testUser(t, st, "mallory")

	if _, err := st.SetFeed(ctx, alice.ID, "https://example.com/one.xml"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetFeed(ctx, alice.ID, "https://example.com/two.xml"); err != nil {
		t.Fatal(err)
	}
	f, err := st.FeedByUser(ctx, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if f.URL != "https://example.com/two.xml" {
		t.Errorf("feed not replaced: %q", f.URL)
	}
	if _, err := st.FeedByID(ctx, mallory.ID, f.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-tenant feed read succeeded: %v", err)
	}
}

func TestInsertItemDeduplicatesByGUID(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	alice := testUser(t, st, "alice")
	f, err := st.SetFeed(ctx, alice.ID, "https://example.com/feed.xml")
	if err != nil {
		t.Fatal(err)
	}

	first, err := st.InsertItem(ctx, &Item{FeedID: f.ID, GUID: "g1", Title: "One"})
	if err != nil || !first {
		t.Fatalf("first insert: fresh=%v err=%v", first, err)
	}
	second, err := st.InsertItem(ctx, &Item{FeedID: f.ID, GUID: "g1", Title: "One again"})
	if err != nil {
		t.Fatal(err)
	}
	if second {
		t.Error("duplicate GUID reported as a new entry, which would repost it")
	}
}

func TestQueueDeliveriesSkipsPausedAndIsIdempotent(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	alice := testUser(t, st, "alice")
	f, _ := st.SetFeed(ctx, alice.ID, "https://example.com/feed.xml")

	active := &Destination{UserID: alice.ID, Kind: "webhook", Label: "on"}
	paused := &Destination{UserID: alice.ID, Kind: "mastodon", Label: "off"}
	if err := st.CreateDestination(ctx, active); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateDestination(ctx, paused); err != nil {
		t.Fatal(err)
	}
	paused.Paused = true
	if err := st.UpdateDestination(ctx, paused); err != nil {
		t.Fatal(err)
	}

	item := &Item{FeedID: f.ID, GUID: "g1", Title: "One"}
	if _, err := st.InsertItem(ctx, item); err != nil {
		t.Fatal(err)
	}

	n, err := st.QueueDeliveries(ctx, alice.ID, f.ID, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("queued %d deliveries, want 1 (paused destination should be skipped)", n)
	}
	again, err := st.QueueDeliveries(ctx, alice.ID, f.ID, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Errorf("re-queueing created %d duplicates", again)
	}
}

func TestOAuthStateIsSingleUse(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	if err := st.PutOAuthState(ctx, "hash1", "example.social", []byte("verifier"), time.Minute); err != nil {
		t.Fatal(err)
	}
	host, verifier, err := st.TakeOAuthState(ctx, "hash1")
	if err != nil {
		t.Fatal(err)
	}
	if host != "example.social" || string(verifier) != "verifier" {
		t.Errorf("got %q %q", host, verifier)
	}
	if _, _, err := st.TakeOAuthState(ctx, "hash1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("state was replayable: %v", err)
	}
}

func TestExpiredOAuthStateIsRejected(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.PutOAuthState(ctx, "hash2", "example.social", []byte("v"), -time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.TakeOAuthState(ctx, "hash2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired state accepted: %v", err)
	}
}

func TestSessionExpiryAndRevocation(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	alice := testUser(t, st, "alice")

	if err := st.CreateSession(ctx, "live", alice.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, "stale", alice.ID, -time.Hour); err != nil {
		t.Fatal(err)
	}

	if _, err := st.SessionUser(ctx, "live"); err != nil {
		t.Errorf("live session rejected: %v", err)
	}
	if _, err := st.SessionUser(ctx, "stale"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired session accepted: %v", err)
	}
	if err := st.DeleteSession(ctx, "live"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SessionUser(ctx, "live"); !errors.Is(err, ErrNotFound) {
		t.Errorf("session survived logout: %v", err)
	}
}

// Deleting an account must take everything with it, since that is the promise
// made on the settings page.
func TestDeleteUserCascades(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	alice := testUser(t, st, "alice")

	f, _ := st.SetFeed(ctx, alice.ID, "https://example.com/feed.xml")
	d := &Destination{UserID: alice.ID, Kind: "webhook", Label: "hook"}
	if err := st.CreateDestination(ctx, d); err != nil {
		t.Fatal(err)
	}
	item := &Item{FeedID: f.ID, GUID: "g1", Title: "One"}
	if _, err := st.InsertItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	if _, err := st.QueueDeliveries(ctx, alice.ID, f.ID, item.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession(ctx, "sess", alice.ID, time.Hour); err != nil {
		t.Fatal(err)
	}

	if err := st.DeleteUser(ctx, alice.ID); err != nil {
		t.Fatal(err)
	}

	for name, count := range map[string]func() int{
		"feeds":        func() int { return countRows(t, st, "feeds") },
		"items":        func() int { return countRows(t, st, "items") },
		"destinations": func() int { return countRows(t, st, "destinations") },
		"deliveries":   func() int { return countRows(t, st, "deliveries") },
		"sessions":     func() int { return countRows(t, st, "sessions") },
	} {
		if n := count(); n != 0 {
			t.Errorf("%s left %d rows after account deletion", name, n)
		}
	}
}

func countRows(t *testing.T, st *Store, table string) int {
	t.Helper()
	var n int
	// table is a literal from the test, not input.
	if err := st.db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDeliveryTargetsRejectsOwnerMismatch(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	alice := testUser(t, st, "alice")
	mallory := testUser(t, st, "mallory")

	f, _ := st.SetFeed(ctx, alice.ID, "https://example.com/feed.xml")
	item := &Item{FeedID: f.ID, GUID: "g1", Title: "One"}
	if _, err := st.InsertItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	d := &Destination{UserID: mallory.ID, Kind: "webhook", Label: "mallory's"}
	if err := st.CreateDestination(ctx, d); err != nil {
		t.Fatal(err)
	}

	// A delivery claiming Alice owns Mallory's destination must not resolve.
	dl := &Delivery{UserID: alice.ID, ItemID: item.ID, DestinationID: d.ID}
	if _, _, err := st.DeliveryTargets(ctx, dl); err == nil {
		t.Error("delivery resolved across owners")
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	for range 3 {
		st, err := Open(path)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		st.Close()
	}
}

// Re-authorising must find the existing record so the token it replaces can be
// retired rather than left live at the instance.
func TestUserByRemote(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	alice := testUser(t, st, "alice")

	got, err := st.UserByRemote(ctx, "example.social", "alice-id")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != alice.ID {
		t.Errorf("got user %d, want %d", got.ID, alice.ID)
	}
	if string(got.AccessToken) != "sealed" {
		t.Errorf("access token = %q", got.AccessToken)
	}

	// The same account id on another instance is a different person.
	if _, err := st.UserByRemote(ctx, "other.social", "alice-id"); !errors.Is(err, ErrNotFound) {
		t.Errorf("matched across instances: %v", err)
	}
	if _, err := st.UserByRemote(ctx, "example.social", "nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("matched an unknown account: %v", err)
	}
}

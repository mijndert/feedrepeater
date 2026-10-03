package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

// feedExists reports whether a feed row survives, for the lifecycle assertions
// below. Deliberately a test helper rather than a store method: nothing the
// service serves needs to look a feed up by URL without an owner in hand, and
// the one handler that did leaked which feeds the userbase reads.
func feedExists(t *testing.T, st *Store, url string) bool {
	t.Helper()
	var n int
	if err := st.ro.QueryRow(`SELECT count(*) FROM feeds WHERE url = ?`, url).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
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

// A database written before either rule can hold two webhooks for one account,
// which the unique index cannot be created over, and rows of kinds this binary
// no longer builds. Opening it has to survive the first and clear out the
// second, in that order: dedupe, index, then delete what is left of the other
// services.
func TestMigrationLeavesOnlyMastodon(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

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
		INSERT INTO destinations (id, user_id, kind, label, credentials, created_at)
		VALUES (1, 1, 'webhook', 'first', x'0102', unixepoch()),
		       (2, 1, 'webhook', 'second', x'0304', unixepoch()),
		       (3, 1, 'mastodon', 'toots', x'0506', unixepoch());`); err != nil {
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
	if len(list) != 1 {
		t.Fatalf("account holds %d destinations after the migration, want only the Mastodon one", len(list))
	}
	if list[0].Kind != "mastodon" || list[0].Label != "toots" {
		t.Errorf("survivor is %s/%q, want mastodon/\"toots\"", list[0].Kind, list[0].Label)
	}
}

// An account holds at most MaxDestinationsPerAccount destinations, and the
// count is read inside the insert's transaction: a handler that counted first
// would have a verification round trip between its count and the insert, so
// concurrent requests would all find room. Two of one kind is fine now — two
// Mastodon accounts is the case the limit replaced the per-kind rule for — and
// another account's rows do not count against this one.
func TestDestinationLimitIsEnforcedByTheDatabase(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	alice := testUser(t, st, "alice")
	bob := testUser(t, st, "bob")

	for i := range MaxDestinationsPerAccount {
		kind := "mastodon"
		if i%2 == 1 {
			kind = "discord"
		}
		if err := st.CreateDestination(ctx, &Destination{UserID: alice.ID, Kind: kind, Label: "one more"}); err != nil {
			t.Fatalf("destination %d: %v", i+1, err)
		}
	}
	over := &Destination{UserID: alice.ID, Kind: "mastodon", Label: "too many"}
	if err := st.CreateDestination(ctx, over); !errors.Is(err, ErrDestinationLimit) {
		t.Errorf("destination %d for one account = %v, want ErrDestinationLimit", MaxDestinationsPerAccount+1, err)
	}
	if err := st.CreateDestination(ctx, &Destination{UserID: bob.ID, Kind: "mastodon", Label: "@bob@example.social"}); err != nil {
		t.Errorf("another account's first destination: %v", err)
	}

	list, err := st.DestinationsByUser(ctx, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != MaxDestinationsPerAccount {
		t.Errorf("account holds %d destinations, want %d", len(list), MaxDestinationsPerAccount)
	}
}

// The central multi-tenant property: one user's identifiers must not resolve
// against another user's rows.
func TestDestinationsAreScopedToTheirOwner(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	alice := testUser(t, st, "alice")
	mallory := testUser(t, st, "mallory")

	d := &Destination{UserID: alice.ID, Kind: "mastodon", Label: "@alice@example.social", Template: "{{title}}"}
	if err := st.CreateDestination(ctx, d); err != nil {
		t.Fatal(err)
	}

	if _, err := st.Destination(ctx, mallory.ID, d.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-tenant read succeeded: %v", err)
	}
	stolen := &Destination{ID: d.ID, UserID: mallory.ID, Kind: "mastodon", Label: "taken"}
	if err := st.UpdateDestination(ctx, stolen); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-tenant update succeeded: %v", err)
	}

	got, err := st.Destination(ctx, alice.ID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Label != "@alice@example.social" {
		t.Errorf("owner's destination was modified: %q", got.Label)
	}
}

// An account follows up to MaxFeedsPerAccount feeds, each once, and reads
// only its own. The count is decided inside the insert's transaction for the
// same reason the destination count is.
func TestFeedsAreCappedAndScoped(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	alice := testUser(t, st, "alice")
	mallory := testUser(t, st, "mallory")
	state := FetchState{NextFetchAt: time.Now().UTC()}

	var first *Subscription
	for i := range MaxFeedsPerAccount {
		sub, _, err := st.Subscribe(ctx, alice.ID, fmt.Sprintf("https://example.com/%d.xml", i), "", nil, state)
		if err != nil {
			t.Fatalf("feed %d: %v", i+1, err)
		}
		if first == nil {
			first = sub
		}
	}
	if _, _, err := st.Subscribe(ctx, alice.ID, "https://example.com/one-too-many.xml", "", nil, state); !errors.Is(err, ErrFeedLimit) {
		t.Errorf("feed %d = %v, want ErrFeedLimit", MaxFeedsPerAccount+1, err)
	}
	if feedExists(t, st, "https://example.com/one-too-many.xml") {
		t.Error("a refused feed was still created")
	}

	feeds, err := st.FeedsByUser(ctx, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(feeds) != MaxFeedsPerAccount {
		t.Fatalf("account lists %d feeds, want %d", len(feeds), MaxFeedsPerAccount)
	}
	if feeds[0].ID != first.FeedID {
		t.Errorf("feeds are not listed oldest first: %d, want %d", feeds[0].ID, first.FeedID)
	}

	// Mallory has no subscription, so there is no feed to read — the question
	// "is this feed yours" is a question about the subscription, which is the
	// only per-account row there is.
	if _, err := st.FeedByUser(ctx, mallory.ID, first.FeedID); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-tenant feed read succeeded: %v", err)
	}
	if err := st.Unsubscribe(ctx, mallory.ID, first.FeedID); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-tenant unsubscribe = %v, want ErrNotFound", err)
	}
	if err := st.SetSubscriptionPaused(ctx, mallory.ID, first.FeedID, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-tenant pause = %v, want ErrNotFound", err)
	}
	if f, err := st.FeedByUser(ctx, alice.ID, first.FeedID); err != nil || f.Paused {
		t.Errorf("another account's pause reached alice's feed: %v %v", f, err)
	}

	// Removing one frees a slot, and a feed with no other subscriber is
	// collected rather than polled for nobody.
	if err := st.Unsubscribe(ctx, alice.ID, first.FeedID); err != nil {
		t.Fatal(err)
	}
	if feedExists(t, st, "https://example.com/0.xml") {
		t.Error("orphaned feed survived")
	}
	if _, _, err := st.Subscribe(ctx, alice.ID, "https://example.com/replacement.xml", "", nil, state); err != nil {
		t.Errorf("adding a feed after removing one: %v", err)
	}
}

// Following the same address twice would be two subscriptions delivering the
// same entries twice, so it is refused, and refused by the database rather than
// by a check two tabs could both pass.
func TestSubscribingTwiceToOneFeedIsRefused(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	alice := testUser(t, st, "alice")
	state := FetchState{NextFetchAt: time.Now().UTC()}

	const url = "https://example.com/feed.xml"
	if _, _, err := st.Subscribe(ctx, alice.ID, url, "", nil, state); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Subscribe(ctx, alice.ID, url, "", nil, state); !errors.Is(err, ErrAlreadySubscribed) {
		t.Errorf("second subscription to one feed = %v, want ErrAlreadySubscribed", err)
	}
	if feeds, _ := st.FeedsByUser(ctx, alice.ID); len(feeds) != 1 {
		t.Errorf("account lists %d feeds, want 1", len(feeds))
	}
}

// The point of the whole arrangement: two accounts following one address share
// one feed row, and therefore one fetch.
func TestSubscribingToAKnownFeedSharesIt(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	alice := testUser(t, st, "alice")
	bob := testUser(t, st, "bob")

	const url = "https://example.com/popular.xml"
	a, _, err := st.Subscribe(ctx, alice.ID, url, "Popular", nil, FetchState{NextFetchAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := st.Subscribe(ctx, bob.ID, url, "", nil, FetchState{NextFetchAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if a.FeedID != b.FeedID {
		t.Fatalf("feed not shared: alice %d, bob %d", a.FeedID, b.FeedID)
	}

	due, err := st.DueFeeds(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("two subscribers produced %d due feeds, want 1", len(due))
	}

	// Bob leaving must not take the feed Alice is still reading.
	if err := st.Unsubscribe(ctx, bob.ID, b.FeedID); err != nil {
		t.Fatal(err)
	}
	if !feedExists(t, st, url) {
		t.Error("feed removed while still subscribed")
	}
	if err := st.Unsubscribe(ctx, alice.ID, a.FeedID); err != nil {
		t.Fatal(err)
	}
	if feedExists(t, st, url) {
		t.Error("feed survived its last subscriber")
	}
}

func TestInsertItemDeduplicatesByGUID(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	alice := testUser(t, st, "alice")
	f, _, err := st.Subscribe(ctx, alice.ID, "https://example.com/feed.xml", "", nil, FetchState{NextFetchAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}

	first, err := st.InsertItem(ctx, &Item{FeedID: f.FeedID, GUID: "g1", Title: "One"})
	if err != nil || !first {
		t.Fatalf("first insert: fresh=%v err=%v", first, err)
	}
	second, err := st.InsertItem(ctx, &Item{FeedID: f.FeedID, GUID: "g1", Title: "One again"})
	if err != nil {
		t.Fatal(err)
	}
	if second {
		t.Error("duplicate GUID reported as a new entry, which would repost it")
	}
}

// Pausing a destination stops posting without stopping reading, and queueing
// the same entry twice must not post it twice. Two accounts on one feed, since
// an account has one destination: alice's is paused, bob's is not.
func TestQueueDeliveriesSkipsPausedAndIsIdempotent(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	alice := testUser(t, st, "alice")
	bob := testUser(t, st, "bob")

	const url = "https://example.com/feed.xml"
	f, _, _ := st.Subscribe(ctx, alice.ID, url, "", nil, FetchState{NextFetchAt: time.Now().UTC()})
	if _, _, err := st.Subscribe(ctx, bob.ID, url, "", nil, FetchState{NextFetchAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	paused := &Destination{UserID: alice.ID, Kind: "mastodon", Label: "off"}
	if err := st.CreateDestination(ctx, paused); err != nil {
		t.Fatal(err)
	}
	paused.Paused = true
	if err := st.UpdateDestination(ctx, paused); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateDestination(ctx, &Destination{UserID: bob.ID, Kind: "mastodon", Label: "on"}); err != nil {
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
		t.Errorf("queued %d deliveries, want 1 (the paused destination should be skipped)", n)
	}
	again, err := st.QueueDeliveries(ctx, f.FeedID, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Errorf("re-queueing created %d duplicates", again)
	}
	if views, _ := st.RecentDeliveries(ctx, alice.ID, 10); len(views) != 0 {
		t.Errorf("the paused account received %d deliveries", len(views))
	}
}

func TestOAuthStateIsSingleUse(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()

	if err := st.PutOAuthState(ctx, "hash1", OAuthState{Host: "example.social"}, []byte("verifier"), time.Minute); err != nil {
		t.Fatal(err)
	}
	got, verifier, err := st.TakeOAuthState(ctx, "hash1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Host != "example.social" || string(verifier) != "verifier" {
		t.Errorf("got %+v %q", got, verifier)
	}
	// A flow that says nothing about itself is a sign-in.
	if got.Purpose != OAuthLogin || got.UserID != 0 {
		t.Errorf("default purpose = %+v, want a login for nobody in particular", got)
	}
	if _, _, err := st.TakeOAuthState(ctx, "hash1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("state was replayable: %v", err)
	}
}

func TestExpiredOAuthStateIsRejected(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if err := st.PutOAuthState(ctx, "hash2", OAuthState{Host: "example.social"}, []byte("v"), -time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.TakeOAuthState(ctx, "hash2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired state accepted: %v", err)
	}
}

// A connect flow remembers whose destination it is for, so the callback can
// insist the same account finishes it rather than taking that from a parameter.
func TestOAuthStateCarriesItsPurpose(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	alice := testUser(t, st, "alice")

	in := OAuthState{Host: "other.social", Purpose: OAuthConnect, UserID: alice.ID}
	if err := st.PutOAuthState(ctx, "hash3", in, []byte("v"), time.Minute); err != nil {
		t.Fatal(err)
	}
	got, _, err := st.TakeOAuthState(ctx, "hash3")
	if err != nil {
		t.Fatal(err)
	}
	if got != in {
		t.Errorf("got %+v, want %+v", got, in)
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

	f, _, _ := st.Subscribe(ctx, alice.ID, "https://example.com/feed.xml", "", nil, FetchState{NextFetchAt: time.Now().UTC()})
	d := &Destination{UserID: alice.ID, Kind: "mastodon", Label: "@alice@example.social"}
	if err := st.CreateDestination(ctx, d); err != nil {
		t.Fatal(err)
	}
	item := &Item{FeedID: f.FeedID, GUID: "g1", Title: "One"}
	if _, err := st.InsertItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	if _, err := st.QueueDeliveries(ctx, f.FeedID, item.ID); err != nil {
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
	if err := st.rw.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestDeliveryTargetsRejectsOwnerMismatch(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	alice := testUser(t, st, "alice")
	mallory := testUser(t, st, "mallory")

	f, _, _ := st.Subscribe(ctx, alice.ID, "https://example.com/feed.xml", "", nil, FetchState{NextFetchAt: time.Now().UTC()})
	item := &Item{FeedID: f.FeedID, GUID: "g1", Title: "One"}
	if _, err := st.InsertItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	d := &Destination{UserID: mallory.ID, Kind: "mastodon", Label: "mallory's"}
	if err := st.CreateDestination(ctx, d); err != nil {
		t.Fatal(err)
	}

	// A delivery claiming Alice owns Mallory's destination. Nothing routes one
	// into existence, so it is written directly — the point is what the sender
	// is handed if corruption or a bug ever produces one.
	now := time.Now().UTC().Unix()
	if _, err := st.rw.ExecContext(ctx, `
		INSERT INTO deliveries (user_id, item_id, destination_id, status, next_attempt_at, created_at, updated_at)
		VALUES (?, ?, ?, 'pending', ?, ?, ?)`,
		alice.ID, item.ID, d.ID, now, now, now); err != nil {
		t.Fatal(err)
	}

	// The row still comes back — a delivery that silently stayed pending
	// forever would be worse — but it carries both owners, which is what lets
	// the publisher refuse it instead of sending under the wrong credentials.
	due, err := st.DueDeliveries(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("got %d due deliveries, want 1", len(due))
	}
	if due[0].Destination.UserID == due[0].UserID {
		t.Error("owner mismatch was not visible to the sender")
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

// A poll holds a feed for as long as the fetch takes, and pausing is a click
// that lands in the middle of one. What the worker read must not be written
// back over the account's own column, or the dashboard says "Paused" while the
// feed keeps posting.
//
// The split makes this structural rather than a rule to remember: pausing is a
// column on the subscription and RecordFetch only ever writes feed_state, so
// there is no longer a statement that could overwrite it.
func TestRecordFetchLeavesPauseAlone(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	alice := testUser(t, st, "alice")
	if _, _, err := st.Subscribe(ctx, alice.ID, "https://example.com/feed.xml", "", nil, FetchState{NextFetchAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	// What the worker claims at the start of a poll.
	due, err := st.DueFeeds(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("got %d due feeds, want 1", len(due))
	}
	inflight := due[0].FetchState

	// What the account does while that fetch is in the air.
	if err := st.SetSubscriptionPaused(ctx, alice.ID, due[0].Feed.ID, true); err != nil {
		t.Fatal(err)
	}

	// The poll finishes and records its result.
	now := time.Now().UTC()
	inflight.LastFetchAt = &now
	inflight.NextFetchAt = now.Add(15 * time.Minute)
	if err := st.RecordFetch(ctx, &inflight); err != nil {
		t.Fatal(err)
	}

	after, err := st.FeedByUser(ctx, alice.ID, due[0].Feed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Paused {
		t.Error("recording a fetch un-paused the feed")
	}
	if after.LastFetchAt == nil {
		t.Error("the fetch was not recorded")
	}

	// A paused subscription is the only one, so nothing wants the feed and it
	// is not polled at all.
	if due, err := st.DueFeeds(ctx, 10); err != nil {
		t.Fatal(err)
	} else if len(due) != 0 {
		t.Errorf("polled a feed every subscriber had paused (%d due)", len(due))
	}
}

// The dead-feed rule stops a document for everyone, which is the difference
// between it and a subscriber's own pause: what is missing is the feed, not one
// account's interest in it.
func TestDisableFeedStopsItForEverySubscriber(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	alice := testUser(t, st, "alice")
	bob := testUser(t, st, "bob")

	const url = "https://example.com/feed.xml"
	f, _, err := st.Subscribe(ctx, alice.ID, url, "", nil, FetchState{NextFetchAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Subscribe(ctx, bob.ID, url, "", nil, FetchState{NextFetchAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	if err := st.DisableFeed(ctx, f.FeedID); err != nil {
		t.Fatal(err)
	}
	for _, u := range []*User{alice, bob} {
		after, err := st.FeedByUser(ctx, u.ID, f.FeedID)
		if err != nil {
			t.Fatal(err)
		}
		if !after.Disabled {
			t.Errorf("feed still running for user %d", u.ID)
		}
		if after.Paused {
			t.Errorf("stopping a feed silently paused user %d's subscription", u.ID)
		}
	}

	due, err := st.DueFeeds(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Errorf("a stopped feed is still due for polling: %d", len(due))
	}
}

// Joining a feed somebody else already follows must not replay its history. The
// entries are already in the table — they are not "new" to the feed, only to
// the account — so the watermark is the only thing standing between a new
// subscriber and every post the feed has ever made.
func TestNewSubscriberDoesNotReceiveTheBacklog(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	alice := testUser(t, st, "alice")
	bob := testUser(t, st, "bob")

	const url = "https://example.com/popular.xml"
	a, _, err := st.Subscribe(ctx, alice.ID, url, "Popular", nil, FetchState{NextFetchAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateDestination(ctx, &Destination{
		UserID: alice.ID, Kind: "mastodon", Label: "alice's",
	}); err != nil {
		t.Fatal(err)
	}

	// Three entries arrive while only Alice is subscribed.
	backlog := []Item{
		{GUID: "g1", Title: "One"}, {GUID: "g2", Title: "Two"}, {GUID: "g3", Title: "Three"},
	}
	if _, err := st.RecordEntries(ctx, a.FeedID, backlog, true, 10); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.PendingCount(ctx, alice.ID); n != 3 {
		t.Fatalf("alice has %d pending, want 3", n)
	}

	// Bob subscribes now, and connects a destination.
	if _, _, err := st.Subscribe(ctx, bob.ID, url, "", nil, FetchState{NextFetchAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateDestination(ctx, &Destination{
		UserID: bob.ID, Kind: "mastodon", Label: "bob's",
	}); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.PendingCount(ctx, bob.ID); n != 0 {
		t.Errorf("bob was sent %d entries that predate his subscription, want 0", n)
	}

	// What arrives afterwards goes to both.
	if _, err := st.RecordEntries(ctx, a.FeedID, []Item{{GUID: "g4", Title: "Four"}}, true, 10); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.PendingCount(ctx, bob.ID); n != 1 {
		t.Errorf("bob has %d pending after a new entry, want 1", n)
	}
	if n, _ := st.PendingCount(ctx, alice.ID); n != 4 {
		t.Errorf("alice has %d pending after a new entry, want 4", n)
	}
}

// One entry, one transaction, and the burst cap applied with the inserted ids
// in hand rather than by a caller guessing which were new.
func TestRecordEntriesCapsABurstAndStaysIdempotent(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	alice := testUser(t, st, "alice")
	sub, _, err := st.Subscribe(ctx, alice.ID, "https://example.com/feed.xml", "", nil, FetchState{NextFetchAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateDestination(ctx, &Destination{
		UserID: alice.ID, Kind: "mastodon", Label: "@alice@example.social",
	}); err != nil {
		t.Fatal(err)
	}

	burst := make([]Item, 20)
	for i := range burst {
		burst[i] = Item{GUID: fmt.Sprintf("g%02d", i), Title: "Entry"}
	}
	res, err := st.RecordEntries(ctx, sub.FeedID, burst, true, 5)
	if err != nil {
		t.Fatal(err)
	}
	if res.New != 20 {
		t.Errorf("recorded %d entries, want 20", res.New)
	}
	if res.Queued != 5 || res.Skipped != 15 {
		t.Errorf("queued %d and skipped %d, want 5 and 15", res.Queued, res.Skipped)
	}

	again, err := st.RecordEntries(ctx, sub.FeedID, burst, true, 5)
	if err != nil {
		t.Fatal(err)
	}
	if again.New != 0 || again.Queued != 0 {
		t.Errorf("re-ingest recorded %d and queued %d, want 0 and 0", again.New, again.Queued)
	}
}

// Stopping a feed is permanent unless something clears it, and with the feed
// shared nothing else can: a second subscriber keeps the row alive through an
// unsubscribe, so re-adding the address rejoins the same stopped state. Without
// this, a popular blog that went down for a fortnight is unreachable for every
// subscriber, for good.
func TestSubscribingRevivesAStoppedFeed(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	alice := testUser(t, st, "alice")
	bob := testUser(t, st, "bob")
	carol := testUser(t, st, "carol")

	const url = "https://example.com/outage.xml"
	state := FetchState{NextFetchAt: time.Now().UTC()}
	a, _, err := st.Subscribe(ctx, alice.ID, url, "Outage", nil, state)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Subscribe(ctx, bob.ID, url, "", nil, state); err != nil {
		t.Fatal(err)
	}

	// The dead-feed rule fires.
	if err := st.DisableFeed(ctx, a.FeedID); err != nil {
		t.Fatal(err)
	}
	if due, err := st.DueFeeds(ctx, 10); err != nil {
		t.Fatal(err)
	} else if len(due) != 0 {
		t.Fatalf("a stopped feed is still polled: %d", len(due))
	}

	// Alice leaves. Bob keeps the row alive, so Alice re-adding cannot get a
	// fresh one — this is the case that had no way back.
	if err := st.Unsubscribe(ctx, alice.ID, a.FeedID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Subscribe(ctx, alice.ID, url, "", nil, state); err != nil {
		t.Fatal(err)
	}

	after, err := st.FeedByUser(ctx, alice.ID, a.FeedID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Disabled {
		t.Error("re-subscribing left the feed stopped")
	}
	if after.Failures != 0 {
		t.Errorf("failure count survived revival: %d", after.Failures)
	}
	if due, err := st.DueFeeds(ctx, 10); err != nil {
		t.Fatal(err)
	} else if len(due) != 1 {
		t.Errorf("revived feed is not due for polling: %d", len(due))
	}

	// Bob, who never left, gets the working feed back too — it is one document.
	if b, err := st.FeedByUser(ctx, bob.ID, a.FeedID); err != nil {
		t.Fatal(err)
	} else if b.Disabled {
		t.Error("bob is still on the stopped feed")
	}

	// A brand-new subscriber to a healthy feed must not disturb its schedule.
	before, err := st.DueFeeds(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Subscribe(ctx, carol.ID, url, "", nil,
		FetchState{NextFetchAt: time.Now().UTC().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	againDue, err := st.DueFeeds(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(againDue) != len(before) {
		t.Errorf("a new subscriber changed the poll schedule of a healthy feed")
	}
}

// Reviving a shared feed is the one write that reaches across accounts, so what
// it does to a bystander's dashboard matters. It must put the feed back in the
// queue without telling an existing subscriber the problem went away: until a
// fetch actually succeeds, the explanation they were given is still the truth.
func TestRevivingAFeedKeepsTheExplanationForOtherSubscribers(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	alice := testUser(t, st, "alice")
	bob := testUser(t, st, "bob")

	const url = "https://example.com/gone.xml"
	state := FetchState{NextFetchAt: time.Now().UTC()}
	a, _, err := st.Subscribe(ctx, alice.ID, url, "Gone", nil, state)
	if err != nil {
		t.Fatal(err)
	}

	// The feed dies under Alice.
	failing := time.Now().UTC().Add(-20 * 24 * time.Hour)
	if err := st.RecordFetch(ctx, &FetchState{
		FeedID: a.FeedID, NextFetchAt: time.Now().UTC(),
		LastError: "Stopped after repeated failures. feed returned 410",
		Failures:  99, FailingSince: &failing,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.DisableFeed(ctx, a.FeedID); err != nil {
		t.Fatal(err)
	}

	// Bob subscribes, which revives it for everyone.
	if _, _, err := st.Subscribe(ctx, bob.ID, url, "", nil, state); err != nil {
		t.Fatal(err)
	}

	view, err := st.FeedByUser(ctx, alice.ID, a.FeedID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Disabled {
		t.Error("feed was not revived")
	}
	if view.LastError == "" {
		t.Error("a stranger's subscribe erased the reason Alice's feed stopped")
	}
	if view.Failures != 0 {
		t.Errorf("failure count survived revival: %d", view.Failures)
	}

	// And the clock genuinely restarted, so the fortnight is measured from the
	// new attempt rather than resuming a streak that is already three weeks old.
	due, err := st.DueFeeds(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("revived feed is not due: %d", len(due))
	}
	if due[0].FailingSince != nil {
		t.Error("the failure streak was carried over into the retry")
	}

	// A successful fetch is what clears the message.
	st2 := due[0].FetchState
	st2.LastError, st2.Failures, st2.FailingSince = "", 0, nil
	st2.NextFetchAt = time.Now().UTC().Add(time.Hour)
	if err := st.RecordFetch(ctx, &st2); err != nil {
		t.Fatal(err)
	}
	if view, err := st.FeedByUser(ctx, alice.ID, a.FeedID); err != nil {
		t.Fatal(err)
	} else if view.LastError != "" {
		t.Errorf("a successful fetch left the old error in place: %q", view.LastError)
	}
}

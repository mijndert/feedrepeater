package feed

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"feedrepeater.com/internal/store"
)

func newStore(t *testing.T) (*store.Store, int64, int64) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	ctx := context.Background()
	user, err := st.UpsertUser(ctx, &store.User{
		Host: "example.social", RemoteID: "u1", Acct: "alice", AccessToken: []byte("x"),
	})
	if err != nil {
		t.Fatal(err)
	}
	f, _, err := st.Subscribe(ctx, user.ID, "https://example.com/feed.xml", "", nil, store.FetchState{NextFetchAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	// A destination must exist, or nothing would be queued regardless.
	if err := st.CreateDestination(ctx, &store.Destination{
		UserID: user.ID, Kind: "webhook", Label: "hook", Template: "{{title}}",
	}); err != nil {
		t.Fatal(err)
	}
	return st, f.ID, user.ID
}

func entries(n int) []Entry {
	out := make([]Entry, n)
	for i := range out {
		out[i] = Entry{GUID: "g" + string(rune('a'+i)), Title: "Entry", URL: "https://example.com/x"}
	}
	return out
}

// The behaviour that keeps a new feed from flooding a timeline: adding a feed
// records its backlog as seen and delivers none of it.
func TestPrimingDeliversNothing(t *testing.T) {
	st, feedID, userID := newStore(t)
	ctx := context.Background()

	res, err := Ingest(ctx, st, feedID, entries(20), true, 5)
	if err != nil {
		t.Fatal(err)
	}
	if res.New != 20 {
		t.Errorf("recorded %d entries, want 20", res.New)
	}
	if res.Queued != 0 {
		t.Errorf("priming queued %d deliveries, want 0", res.Queued)
	}
	if n, _ := st.PendingCount(ctx, userID); n != 0 {
		t.Errorf("%d deliveries pending after priming, want 0", n)
	}
}

// Entries that arrive after priming are delivered, and the primed backlog is
// not re-delivered along with them.
func TestEntriesAfterPrimingAreDelivered(t *testing.T) {
	st, feedID, userID := newStore(t)
	ctx := context.Background()

	if _, err := Ingest(ctx, st, feedID, entries(20), true, 5); err != nil {
		t.Fatal(err)
	}

	// The same 20 entries plus one new one, as a later poll would see them.
	later := append(entries(20), Entry{GUID: "new-1", Title: "Fresh", URL: "https://example.com/new"})
	res, err := Ingest(ctx, st, feedID, later, false, 5)
	if err != nil {
		t.Fatal(err)
	}
	if res.New != 1 {
		t.Errorf("saw %d new entries, want 1", res.New)
	}
	if res.Queued != 1 {
		t.Errorf("queued %d deliveries, want 1", res.Queued)
	}
	if n, _ := st.PendingCount(ctx, userID); n != 1 {
		t.Errorf("%d deliveries pending, want 1", n)
	}
}

func TestBurstIsCapped(t *testing.T) {
	st, feedID, _ := newStore(t)
	ctx := context.Background()

	if _, err := Ingest(ctx, st, feedID, nil, true, 5); err != nil {
		t.Fatal(err)
	}
	res, err := Ingest(ctx, st, feedID, entries(20), false, 5)
	if err != nil {
		t.Fatal(err)
	}
	if res.Queued != 5 {
		t.Errorf("queued %d deliveries, want the cap of 5", res.Queued)
	}
	if res.Skipped != 15 {
		t.Errorf("skipped %d, want 15", res.Skipped)
	}
	// The skipped entries must still be recorded, or the next poll would treat
	// them as new and deliver them after all.
	again, err := Ingest(ctx, st, feedID, entries(20), false, 5)
	if err != nil {
		t.Fatal(err)
	}
	if again.New != 0 {
		t.Errorf("%d entries seen as new on the second pass, want 0", again.New)
	}
}

func TestReIngestIsIdempotent(t *testing.T) {
	st, feedID, userID := newStore(t)
	ctx := context.Background()

	if _, err := Ingest(ctx, st, feedID, nil, true, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := Ingest(ctx, st, feedID, entries(3), false, 5); err != nil {
		t.Fatal(err)
	}
	res, err := Ingest(ctx, st, feedID, entries(3), false, 5)
	if err != nil {
		t.Fatal(err)
	}
	if res.Queued != 0 {
		t.Errorf("re-ingest queued %d duplicates", res.Queued)
	}
	if n, _ := st.PendingCount(ctx, userID); n != 3 {
		t.Errorf("%d pending after re-ingest, want 3", n)
	}
}

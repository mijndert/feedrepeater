package publisher

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"feedrepeater.com/internal/destination"
	"feedrepeater.com/internal/mastodon"
	"feedrepeater.com/internal/safehttp"
	"feedrepeater.com/internal/secret"
	"feedrepeater.com/internal/store"
)

// queued builds one pending delivery and returns it with the publisher that
// owns it. Nothing here reaches the network: the interest is in what the retry
// schedule writes, not in sending.
func queued(t *testing.T) (*Publisher, *store.Store, *store.DueDelivery) {
	t.Helper()
	ctx := context.Background()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	keys, err := secret.NewKeyring(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	hc := safehttp.New(safehttp.Options{UserAgent: "test"})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := New(st, keys, hc, mastodon.New(hc), log)

	user, err := st.UpsertUser(ctx, &store.User{
		Host: "example.social", RemoteID: "alice-id", Acct: "alice", AccessToken: []byte("x"),
	})
	if err != nil {
		t.Fatal(err)
	}
	f, _, err := st.Subscribe(ctx, user.ID, "https://example.com/feed.xml", "", nil, store.FetchState{NextFetchAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateDestination(ctx, &store.Destination{
		UserID: user.ID, Kind: "discord", Label: "Team channel",
	}); err != nil {
		t.Fatal(err)
	}
	item := &store.Item{FeedID: f.FeedID, GUID: "g1", Title: "An entry", URL: "https://example.com/1"}
	if _, err := st.InsertItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	if _, err := st.QueueDeliveries(ctx, f.FeedID, item.ID); err != nil {
		t.Fatal(err)
	}
	due, err := st.DueDeliveries(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("queued %d deliveries, want 1", len(due))
	}
	return p, st, due[0]
}

// nextAttempt reads back when a delivery is scheduled to be tried again.
func nextAttempt(t *testing.T, st *store.Store, userID int64) time.Time {
	t.Helper()
	views, err := st.RecentDeliveries(context.Background(), userID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 {
		t.Fatalf("found %d deliveries", len(views))
	}
	return views[0].NextAttemptAt
}

// A service that says how long to wait gets what it asked for. Retrying sooner
// than a rate limit allows is how the limit turns into a ban.
func TestRetryHonoursTheDelayAServiceAsksFor(t *testing.T) {
	p, st, dl := queued(t)

	failure := destination.WithRetryAfter(errors.New("discord returned 429"), 40*time.Minute)
	p.retry(context.Background(), dl, "rate limited", failure)

	got := time.Until(nextAttempt(t, st, dl.UserID))
	if got < 39*time.Minute || got > 41*time.Minute {
		t.Errorf("next attempt in %s, want the 40m Discord asked for", got.Round(time.Second))
	}
}

// The request is a floor, not a ceiling: when our own backoff is already longer
// than what was asked, the longer wait stands.
func TestRetryKeepsItsOwnBackoffWhenItIsLonger(t *testing.T) {
	p, st, dl := queued(t)

	dl.Attempts = 4 // backoff is already well past a minute here
	failure := destination.WithRetryAfter(errors.New("discord returned 429"), time.Second)
	p.retry(context.Background(), dl, "rate limited", failure)

	if got := time.Until(nextAttempt(t, st, dl.UserID)); got < time.Minute {
		t.Errorf("next attempt in %s, want the publisher's own backoff", got.Round(time.Second))
	}
}

// A failure that says nothing is scheduled the way it always was.
func TestRetryFallsBackToBackoff(t *testing.T) {
	p, st, dl := queued(t)

	p.retry(context.Background(), dl, "temporary error", errors.New("connection reset"))

	got := time.Until(nextAttempt(t, st, dl.UserID))
	if want := backoff(dl.Attempts + 1); got < want-time.Minute || got > want+time.Minute {
		t.Errorf("next attempt in %s, want about %s", got.Round(time.Second), want)
	}
}

// Every kind the UI offers has to be one this can build a client for. A kind
// added to the registry and not to the switch above would connect, store
// credentials, queue entries, and then fail every delivery with "unknown
// destination type" — which is the one failure a person cannot act on.
func TestEveryRegisteredKindHasATarget(t *testing.T) {
	p, _, _ := queued(t)
	sealed, err := p.keys.Encrypt(secret.PurposeDestination, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}

	for _, kind := range destination.Kinds {
		// Empty configuration, so most kinds refuse this row: the interest is only
		// in whether the kind is recognised at all.
		_, err := p.Target(&store.Destination{Kind: kind.Name, Config: "{}", Credentials: sealed})
		if err != nil && strings.Contains(err.Error(), "unknown destination type") {
			t.Errorf("%s is offered in the UI but the publisher cannot build it", kind.Name)
		}
	}

	if _, err := p.Target(&store.Destination{Kind: "not-a-service", Credentials: sealed}); err == nil {
		t.Error("an unknown kind built a target")
	}
}

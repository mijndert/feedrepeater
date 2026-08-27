package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Subscribe attaches an account to the feed at feedURL, replacing whatever it
// was subscribed to before. It reports whether it had to create the feed.
//
// The feed is shared. If somebody already follows this URL the row, its entries
// and its polling schedule are all already there, so subscribing costs one
// insert and no request at all — which is the point of the whole arrangement,
// since the twentieth account to follow a popular blog used to add a twentieth
// fetch of it every interval.
//
// seed and state are only used when this is the first subscriber: the entries
// and validators the caller already has from the fetch it made to check the
// address. They are ignored when the feed exists, because it has better ones.
// The returned flag says which happened, so a caller that expected to join an
// existing feed and finds it created one instead can go and fetch after all.
//
// The watermark is taken inside the transaction. Between reading the entries and
// writing the subscription there is no moment when the poll loop could ingest
// the same feed and treat this account as a subscriber entitled to the backlog.
func (s *Store) Subscribe(ctx context.Context, userID int64, feedURL, title string, seed []Item, state FetchState) (*Subscription, bool, error) {
	now := time.Now().UTC()
	sub := &Subscription{UserID: userID, Primed: true, CreatedAt: now}
	fresh := false

	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := unsubscribe(ctx, tx, userID); err != nil {
			return err
		}

		// The feed may or may not exist. DO NOTHING plus a follow-up read is one
		// statement more than DO UPDATE and does not touch the row when it is
		// already there, which matters because every touched row is a page the
		// replica ships.
		var feedID int64
		err := tx.QueryRowContext(ctx, `
			INSERT INTO feeds (url, title, created_at) VALUES (?, ?, ?)
			ON CONFLICT (url) DO NOTHING
			RETURNING id`, feedURL, title, now.Unix()).Scan(&feedID)
		fresh = err == nil
		if errors.Is(err, sql.ErrNoRows) {
			err = tx.QueryRowContext(ctx, `SELECT id FROM feeds WHERE url = ?`, feedURL).Scan(&feedID)
		}
		if err != nil {
			return err
		}

		if fresh {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO feed_state (feed_id, etag, last_modified, body_hash, next_fetch_at)
				VALUES (?, ?, ?, ?, ?)`,
				feedID, state.ETag, state.LastModified, state.BodyHash,
				state.NextFetchAt.Unix()); err != nil {
				return err
			}
			for i := range seed {
				seed[i].FeedID = feedID
				if _, err := insertItem(ctx, tx, &seed[i]); err != nil {
					return err
				}
			}
		} else if err := reviveFeed(ctx, tx, feedID, now); err != nil {
			return err
		}

		// Everything recorded so far is this account's history, whether it was
		// just seeded or has been accumulating for someone else since March.
		var watermark int64
		if err := tx.QueryRowContext(ctx,
			`SELECT coalesce(max(id), 0) FROM items WHERE feed_id = ?`, feedID).Scan(&watermark); err != nil {
			return err
		}

		if err := tx.QueryRowContext(ctx, `
			INSERT INTO subscriptions (user_id, feed_id, prime_item_id, primed, created_at)
			VALUES (?, ?, ?, 1, ?) RETURNING id`,
			userID, feedID, watermark, now.Unix()).Scan(&sub.ID); err != nil {
			return err
		}
		sub.FeedID, sub.PrimeItemID = feedID, watermark

		// A new feed starts connected to everything the account already has, so
		// the common case needs no routing decisions. Narrowing it is a choice
		// the user makes afterwards.
		_, err = tx.ExecContext(ctx, `
			INSERT INTO feed_destinations (feed_id, destination_id, user_id, created_at)
			SELECT ?, d.id, ?, ? FROM destinations d WHERE d.user_id = ?
			ON CONFLICT DO NOTHING`, feedID, userID, now.Unix(), userID)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return sub, fresh, nil
}

// reviveFeed clears the dead-feed rule when somebody subscribes afresh.
//
// Nothing else ever clears it. When a feed belonged to one account, stopping it
// and pausing it were the same column, so the dashboard's Resume button was the
// way back and the rule healed itself. Now that stopping is a property of the
// shared document, a feed with two subscribers survives either of them leaving,
// and re-adding it would rejoin the same permanently stopped row — a popular
// blog with a fortnight's outage would be unreachable for everyone, for good,
// without direct SQL.
//
// A new subscriber is a fresh assertion that the URL is worth reading, so it is
// the right moment to try again. The schedule is pulled in but never pushed
// out: a feed already due sooner is due for a reason.
//
// last_error is deliberately left alone. This is the one write in the design
// that reaches across accounts, and clearing the message would take an existing
// subscriber's explanation away on an action they took no part in and cannot
// see — their dashboard would go from "Stopped after repeated failures", with
// the reason, to a confident "Active" for a feed that has not actually
// succeeded at anything yet. The message stays until a fetch clears it, which
// RecordFetch does on the first success. Until then the dashboard reads
// "Failing", which is the truth.
//
// The guard is what keeps this from being a general force-poll button reachable
// by anyone: a healthy feed matches neither condition, so its schedule cannot be
// pulled forward by a stranger subscribing.
func reviveFeed(ctx context.Context, tx *sql.Tx, feedID int64, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE feed_state
		SET disabled = 0, failures = 0, failing_since = NULL,
			next_fetch_at = min(next_fetch_at, ?)
		WHERE feed_id = ? AND (disabled = 1 OR failures > 0)`, now.Unix(), feedID)
	return err
}

const feedViewColumns = `
	SELECT sub.id, f.id, sub.user_id, f.url, f.title, sub.paused, st.disabled, sub.primed,
		st.next_fetch_at, st.last_fetch_at, st.changed_at, st.last_error, st.failures, sub.created_at,
		(SELECT count(*) FROM subscriptions x WHERE x.feed_id = f.id)
	FROM subscriptions sub
	JOIN feeds f ON f.id = sub.feed_id
	JOIN feed_state st ON st.feed_id = f.id`

// FeedByUser returns the account's feed as the dashboard shows it.
func (s *Store) FeedByUser(ctx context.Context, userID int64) (*FeedView, error) {
	return scanFeedView(s.ro.QueryRowContext(ctx, feedViewColumns+` WHERE sub.user_id = ?`, userID))
}

func scanFeedView(row *sql.Row) (*FeedView, error) {
	var v FeedView
	var paused, disabled, primed int
	var next, created int64
	var last, changed sql.NullInt64
	err := row.Scan(&v.SubscriptionID, &v.ID, &v.UserID, &v.URL, &v.Title, &paused, &disabled, &primed,
		&next, &last, &changed, &v.LastError, &v.Failures, &created, &v.Subscribers)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	v.Paused, v.Disabled, v.Primed = paused == 1, disabled == 1, primed == 1
	v.NextFetchAt = time.Unix(next, 0).UTC()
	v.CreatedAt = time.Unix(created, 0).UTC()
	v.LastFetchAt, v.ChangedAt = scanTime(last), scanTime(changed)
	return &v, nil
}

// Unsubscribe drops the account's subscription and, if it was the last one, the
// feed itself.
func (s *Store) Unsubscribe(ctx context.Context, userID int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error { return unsubscribe(ctx, tx, userID) })
}

// unsubscribe removes one account's link to its feed and collects the feed if
// nobody else is left. It runs inside the caller's transaction because a feed
// that briefly has no subscribers and is not yet deleted would be polled for
// nobody, and one that is deleted while another account still holds a route
// would take that account's history with it.
func unsubscribe(ctx context.Context, tx *sql.Tx, userID int64) error {
	var feedID int64
	err := tx.QueryRowContext(ctx,
		`DELETE FROM subscriptions WHERE user_id = ? RETURNING feed_id`, userID).Scan(&feedID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// Routes are per account, so only this account's go.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM feed_destinations WHERE feed_id = ? AND user_id = ?`, feedID, userID); err != nil {
		return err
	}
	// The feed, its state and its entries go with the last subscriber. Anything
	// else is a document nobody reads being polled forever.
	return deleteOrphanFeeds(ctx, tx)
}

// SetSubscriptionPaused pauses or resumes one account's subscription. A feed
// every subscriber has paused stops being polled; one that another account
// still wants keeps going, and this account simply stops receiving it.
func (s *Store) SetSubscriptionPaused(ctx context.Context, userID int64, paused bool) error {
	_, err := s.rw.ExecContext(ctx,
		`UPDATE subscriptions SET paused = ? WHERE user_id = ?`, boolInt(paused), userID)
	return err
}

// FetchNow schedules the account's feed for immediate polling.
//
// The feed is shared, so this pulls it forward for every subscriber. That is
// harmless — it is one fetch either way — but it is also why the button is rate
// limited per account rather than per feed.
func (s *Store) FetchNow(ctx context.Context, userID int64) error {
	_, err := s.rw.ExecContext(ctx, `
		UPDATE feed_state SET next_fetch_at = ?
		WHERE feed_id = (SELECT feed_id FROM subscriptions WHERE user_id = ?)
		  AND next_fetch_at > ?`,
		time.Now().UTC().Unix(), userID, time.Now().UTC().Unix())
	return err
}

// DueFeed is a feed ready to be polled.
type DueFeed struct {
	Feed
	FetchState
	// HasRoutes reports whether any live subscription actually leads somewhere.
	// A feed whose subscribers have all unticked their destinations is still
	// recorded, but nobody is waiting on it, so it is polled far less often.
	HasRoutes bool
}

// DueFeeds returns feeds whose next fetch time has passed, soonest first.
//
// A feed with no unpaused subscription is not returned at all. Under the old
// per-account rows a paused feed was simply a paused row; now the question is
// whether anyone at all still wants the document, and if nobody does there is
// nothing to fetch it for.
func (s *Store) DueFeeds(ctx context.Context, limit int) ([]*DueFeed, error) {
	now := time.Now().UTC().Unix()
	rows, err := s.ro.QueryContext(ctx, `
		SELECT f.id, f.url, f.title, f.created_at,
			st.etag, st.last_modified, st.body_hash, st.next_fetch_at, st.last_fetch_at,
			st.changed_at, st.last_error, st.failures, st.failing_since, st.disabled,
			EXISTS (
				SELECT 1 FROM subscriptions sub
				JOIN feed_destinations fd ON fd.feed_id = sub.feed_id AND fd.user_id = sub.user_id
				JOIN destinations d ON d.id = fd.destination_id AND d.paused = 0
				WHERE sub.feed_id = f.id AND sub.paused = 0
			) AS has_routes
		FROM feed_state st
		JOIN feeds f ON f.id = st.feed_id
		WHERE st.disabled = 0 AND st.next_fetch_at <= ?
		  AND EXISTS (SELECT 1 FROM subscriptions sub WHERE sub.feed_id = f.id AND sub.paused = 0)
		ORDER BY st.next_fetch_at
		LIMIT ?`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*DueFeed
	for rows.Next() {
		var d DueFeed
		var hasRoutes, disabled int
		var created, next int64
		var last, changed, failingSince sql.NullInt64
		if err := rows.Scan(&d.Feed.ID, &d.URL, &d.Title, &created,
			&d.ETag, &d.LastModified, &d.BodyHash, &next, &last,
			&changed, &d.LastError, &d.Failures, &failingSince, &disabled,
			&hasRoutes); err != nil {
			return nil, err
		}
		d.FeedID = d.Feed.ID
		d.FailingSince = scanTime(failingSince)
		d.Disabled = disabled == 1
		d.Feed.CreatedAt = time.Unix(created, 0).UTC()
		d.NextFetchAt = time.Unix(next, 0).UTC()
		d.LastFetchAt, d.ChangedAt = scanTime(last), scanTime(changed)
		d.HasRoutes = hasRoutes == 1
		out = append(out, &d)
	}
	return out, rows.Err()
}

// RecordFetch stores the outcome of a poll and the next time to try.
//
// It writes feed_state and nothing else. `disabled` is deliberately not among
// the columns: the worker reads a feed, spends up to a minute fetching it, and
// writing back the value it read would revert a decision made inside that
// window. The one case where a poll does stop a feed is the dead-feed rule,
// which goes through DisableFeed.
func (s *Store) RecordFetch(ctx context.Context, st *FetchState) error {
	_, err := s.rw.ExecContext(ctx, `
		UPDATE feed_state SET etag = ?, last_modified = ?, body_hash = ?,
			next_fetch_at = ?, last_fetch_at = ?, changed_at = ?, last_error = ?,
			failures = ?, failing_since = ?
		WHERE feed_id = ?`,
		st.ETag, st.LastModified, st.BodyHash,
		st.NextFetchAt.Unix(), nullTime(st.LastFetchAt), nullTime(st.ChangedAt),
		st.LastError, st.Failures, nullTime(st.FailingSince), st.FeedID)
	return err
}

// SetFeedTitle records a feed's title when it changes.
//
// Split out of RecordFetch on purpose. The title changes once in a feed's life
// and the fetch state changes every poll, so writing them together would put
// the feeds table back in the path of every fetch — which is the page churn
// splitting the tables was meant to remove.
func (s *Store) SetFeedTitle(ctx context.Context, feedID int64, title string) error {
	_, err := s.rw.ExecContext(ctx,
		`UPDATE feeds SET title = ? WHERE id = ? AND title <> ?`, title, feedID, title)
	return err
}

// DeferFeeds pushes a set of feeds out to a later time, in one statement.
//
// This is the poll loop putting back the feeds it could not take this round
// because their host had just been contacted. Leaving them where they were made
// them the soonest-due feeds on the next tick as well, so they were selected,
// skipped and reselected indefinitely, holding a slot in every batch and
// keeping whatever was behind them from ever being reached.
func (s *Store) DeferFeeds(ctx context.Context, feedIDs []int64, until time.Time) error {
	if len(feedIDs) == 0 {
		return nil
	}
	args := make([]any, 0, len(feedIDs)+1)
	args = append(args, until.Unix())
	for _, id := range feedIDs {
		args = append(args, id)
	}
	// Never pull a feed forward: one already due sooner than this is due for a
	// reason, and a deferral is about relieving a host rather than about it.
	_, err := s.rw.ExecContext(ctx, `
		UPDATE feed_state SET next_fetch_at = ?
		WHERE feed_id IN (`+placeholders(len(feedIDs))+`) AND next_fetch_at < ?`,
		append(args, until.Unix())...)
	return err
}

// DisableFeed stops a feed being polled, for the worker's dead-feed rule. It
// applies to every subscriber, which is correct: the document is gone, not one
// account's interest in it.
func (s *Store) DisableFeed(ctx context.Context, feedID int64) error {
	_, err := s.rw.ExecContext(ctx, `UPDATE feed_state SET disabled = 1 WHERE feed_id = ?`, feedID)
	return err
}

// --- Items -----------------------------------------------------------------

// InsertItem records an entry. It reports whether the row was new; a duplicate
// GUID is a no-op, which is what stops an entry being posted twice.
func (s *Store) InsertItem(ctx context.Context, it *Item) (bool, error) {
	return insertItem(ctx, s.rw, it)
}

// execQuerier is the part of *sql.DB and *sql.Tx that insertItem needs, so the
// same statement serves a standalone insert and one inside a batch.
type execQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func insertItem(ctx context.Context, q execQuerier, it *Item) (bool, error) {
	err := q.QueryRowContext(ctx, `
		INSERT INTO items (feed_id, guid, url, title, summary, author, published_at, seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (feed_id, guid) DO NOTHING
		RETURNING id`,
		it.FeedID, it.GUID, it.URL, it.Title, it.Summary, it.Author,
		nullTime(it.PublishedAt), time.Now().UTC().Unix()).Scan(&it.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil // already seen
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// TrimItems keeps the most recent n entries of every feed in one statement.
//
// It used to be a query per feed, issued from a loop over every feed in the
// database once an hour, and almost all of them had nothing to delete. The
// window function does the same work in a single pass, so the cost tracks the
// number of feeds over the retention rather than the number of feeds at all.
func (s *Store) TrimItems(ctx context.Context, keep int) (int64, error) {
	res, err := s.rw.ExecContext(ctx, `
		DELETE FROM items WHERE id IN (
			SELECT id FROM (
				SELECT id, row_number() OVER (PARTITION BY feed_id ORDER BY id DESC) AS rn
				FROM items
			) WHERE rn > ?
		)`, keep)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func placeholders(n int) string {
	if n <= 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

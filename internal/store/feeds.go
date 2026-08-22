package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// SetFeed installs the user's single feed, replacing any existing one. Changing
// the URL drops the previously seen entries with it, so the new feed is primed
// again rather than replayed.
func (s *Store) SetFeed(ctx context.Context, userID int64, url string) (*Feed, error) {
	now := time.Now().UTC()
	var f *Feed
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM feeds WHERE user_id = ?`, userID); err != nil {
			return err
		}
		var id int64
		err := tx.QueryRowContext(ctx, `
			INSERT INTO feeds (user_id, url, next_fetch_at, created_at)
			VALUES (?, ?, ?, ?) RETURNING id`,
			userID, url, now.Unix(), now.Unix()).Scan(&id)
		if err != nil {
			return err
		}
		// A new feed starts connected to everything the account already has, so
		// the common case needs no routing decisions. Narrowing it is a choice
		// the user makes afterwards.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO feed_destinations (feed_id, destination_id, user_id, created_at)
			SELECT ?, d.id, ?, ? FROM destinations d WHERE d.user_id = ?
			ON CONFLICT DO NOTHING`, id, userID, now.Unix(), userID); err != nil {
			return err
		}

		f = &Feed{ID: id, UserID: userID, URL: url, NextFetchAt: now, CreatedAt: now}
		return nil
	})
	return f, err
}

func (s *Store) FeedByUser(ctx context.Context, userID int64) (*Feed, error) {
	return s.scanFeed(s.db.QueryRowContext(ctx, feedColumns+` FROM feeds WHERE user_id = ?`, userID))
}

// FeedByID looks up a feed scoped to its owner.
func (s *Store) FeedByID(ctx context.Context, userID, id int64) (*Feed, error) {
	return s.scanFeed(s.db.QueryRowContext(ctx, feedColumns+` FROM feeds WHERE id = ? AND user_id = ?`, id, userID))
}

func (s *Store) DeleteFeed(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM feeds WHERE user_id = ?`, userID)
	return err
}

func (s *Store) SetFeedPaused(ctx context.Context, userID int64, paused bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE feeds SET paused = ? WHERE user_id = ?`, boolInt(paused), userID)
	return err
}

// FetchNow schedules the user's feed for immediate polling.
func (s *Store) FetchNow(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE feeds SET next_fetch_at = ? WHERE user_id = ?`, time.Now().UTC().Unix(), userID)
	return err
}

// DueFeed is a feed that is ready to be polled, along with whether anything
// is currently subscribed to it.
type DueFeed struct {
	*Feed
	// HasRoutes reports whether at least one unpaused destination receives
	// this feed. One that delivers nowhere is still recorded, but nothing is
	// waiting on it, so it can be polled far less often.
	HasRoutes bool
}

// DueFeeds returns active feeds whose next fetch time has passed, soonest
// first.
func (s *Store) DueFeeds(ctx context.Context, limit int) ([]*DueFeed, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT f.id, f.user_id, f.url, f.title, f.etag, f.last_modified, f.primed, f.paused,
			f.next_fetch_at, f.last_fetch_at, f.changed_at, f.last_error, f.failures, f.created_at,
			EXISTS (
				SELECT 1 FROM feed_destinations fd
				JOIN destinations d ON d.id = fd.destination_id AND d.user_id = fd.user_id
				WHERE fd.feed_id = f.id AND d.paused = 0
			) AS has_routes
		FROM feeds f
		WHERE f.paused = 0 AND f.next_fetch_at <= ?
		ORDER BY f.next_fetch_at
		LIMIT ?`, time.Now().UTC().Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*DueFeed
	for rows.Next() {
		var f Feed
		var primed, paused, hasRoutes int
		var next, created int64
		var last, changed sql.NullInt64
		if err := rows.Scan(&f.ID, &f.UserID, &f.URL, &f.Title, &f.ETag, &f.LastModified,
			&primed, &paused, &next, &last, &changed, &f.LastError, &f.Failures, &created,
			&hasRoutes); err != nil {
			return nil, err
		}
		f.Primed, f.Paused = primed == 1, paused == 1
		f.NextFetchAt = time.Unix(next, 0).UTC()
		f.CreatedAt = time.Unix(created, 0).UTC()
		f.LastFetchAt = scanTime(last)
		f.ChangedAt = scanTime(changed)
		out = append(out, &DueFeed{Feed: &f, HasRoutes: hasRoutes == 1})
	}
	return out, rows.Err()
}

// FeedOwner pairs a feed with its owner, for housekeeping loops.
type FeedOwner struct {
	FeedID int64
	UserID int64
}

// AllFeedIDs lists every feed and its owner.
func (s *Store) AllFeedIDs(ctx context.Context) ([]FeedOwner, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, user_id FROM feeds`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FeedOwner
	for rows.Next() {
		var f FeedOwner
		if err := rows.Scan(&f.FeedID, &f.UserID); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// RecordFetch stores the outcome of a poll and the next time to try.
//
// paused is deliberately not among the columns written. It belongs to the
// account, not to the fetch: the worker reads a feed, spends up to a minute
// fetching it, and writing back the value it read would revert a pause clicked
// inside that window — the dashboard says "Feed paused" and the feed keeps
// posting. The one case where a poll does pause a feed is the dead-feed rule,
// which goes through PauseFeed.
func (s *Store) RecordFetch(ctx context.Context, f *Feed) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE feeds SET title = ?, etag = ?, last_modified = ?, primed = ?,
			next_fetch_at = ?, last_fetch_at = ?, changed_at = ?, last_error = ?, failures = ?
		WHERE id = ?`,
		f.Title, f.ETag, f.LastModified, boolInt(f.Primed),
		f.NextFetchAt.Unix(), nullTime(f.LastFetchAt), nullTime(f.ChangedAt),
		f.LastError, f.Failures, f.ID)
	return err
}

// PauseFeed pauses one feed by id, for the worker's dead-feed rule. The
// dashboard's own pause is SetFeedPaused, which is scoped by owner; this one is
// reached from the poll loop, which has no request and no session behind it.
func (s *Store) PauseFeed(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE feeds SET paused = 1 WHERE id = ?`, id)
	return err
}

const feedColumns = `SELECT id, user_id, url, title, etag, last_modified, primed, paused,
	next_fetch_at, last_fetch_at, changed_at, last_error, failures, created_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func (s *Store) scanFeed(row *sql.Row) (*Feed, error) {
	f, err := s.scanFeedRows(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return f, err
}

func (s *Store) scanFeedRows(r rowScanner) (*Feed, error) {
	var f Feed
	var primed, paused int
	var next, created int64
	var last, changed sql.NullInt64
	err := r.Scan(&f.ID, &f.UserID, &f.URL, &f.Title, &f.ETag, &f.LastModified, &primed, &paused,
		&next, &last, &changed, &f.LastError, &f.Failures, &created)
	if err != nil {
		return nil, err
	}
	f.Primed, f.Paused = primed == 1, paused == 1
	f.NextFetchAt = time.Unix(next, 0).UTC()
	f.CreatedAt = time.Unix(created, 0).UTC()
	f.LastFetchAt = scanTime(last)
	f.ChangedAt = scanTime(changed)
	return &f, nil
}

// --- Items -----------------------------------------------------------------

// InsertItem records an entry. It reports whether the row was new; a duplicate
// GUID is a no-op, which is what stops an entry being posted twice.
func (s *Store) InsertItem(ctx context.Context, it *Item) (bool, error) {
	err := s.db.QueryRowContext(ctx, `
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

func (s *Store) ItemByID(ctx context.Context, id int64) (*Item, error) {
	var it Item
	var published sql.NullInt64
	var seen int64
	err := s.db.QueryRowContext(ctx, `
		SELECT id, feed_id, guid, url, title, summary, author, published_at, seen_at
		FROM items WHERE id = ?`, id).
		Scan(&it.ID, &it.FeedID, &it.GUID, &it.URL, &it.Title, &it.Summary, &it.Author, &published, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	it.PublishedAt = scanTime(published)
	it.SeenAt = time.Unix(seen, 0).UTC()
	return &it, nil
}

// TrimItems keeps the most recent n entries per feed so the database does not
// grow without bound.
func (s *Store) TrimItems(ctx context.Context, feedID int64, keep int) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM items WHERE feed_id = ? AND id NOT IN (
			SELECT id FROM items WHERE feed_id = ? ORDER BY id DESC LIMIT ?
		)`, feedID, feedID, keep)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

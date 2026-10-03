package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// RecordResult reports what one batch of entries did.
type RecordResult struct {
	// New is how many entries had not been seen before.
	New int
	// Queued is how many deliveries were created.
	Queued int
	// Skipped is how many new entries were recorded but deliberately not
	// delivered, because the burst exceeded maxDeliver.
	Skipped int
}

// RecordEntries writes a poll's entries and queues whatever they earned, in one
// transaction.
//
// One transaction is the whole point. Every insert used to be its own implicit
// transaction, so a two-hundred-entry feed that a server serves unchanged cost
// two hundred lock acquisitions, two hundred WAL frames and two hundred page
// dirties for the replica to ship — per poll, forever. The same batch is now one
// commit, and a feed whose body has not changed does not reach here at all.
//
// deliver false is priming: entries are recorded as seen and nothing is sent,
// which is what makes adding a feed start from the moment it was added rather
// than replaying its archive into somebody's timeline.
func (s *Store) RecordEntries(ctx context.Context, feedID int64, items []Item, deliver bool, maxDeliver int) (RecordResult, error) {
	var res RecordResult
	if len(items) == 0 {
		return res, nil
	}

	err := s.tx(ctx, func(tx *sql.Tx) error {
		var fresh []int64
		for i := range items {
			items[i].FeedID = feedID
			isNew, err := insertItem(ctx, tx, &items[i])
			if err != nil {
				return err
			}
			if isNew {
				fresh = append(fresh, items[i].ID)
			}
		}
		res.New = len(fresh)

		if !deliver || len(fresh) == 0 {
			res.Skipped = len(fresh)
			return nil
		}

		// A burst larger than the cap usually means the feed changed its ids or
		// republished itself, not that the author posted forty times in an hour.
		// Deliver the newest few and leave the rest recorded as seen.
		if maxDeliver > 0 && len(fresh) > maxDeliver {
			res.Skipped = len(fresh) - maxDeliver
			fresh = fresh[len(fresh)-maxDeliver:]
		}

		for _, id := range fresh {
			n, err := queueDeliveries(ctx, tx, feedID, id)
			if err != nil {
				return err
			}
			res.Queued += n
		}
		return nil
	})
	return res, err
}

// QueueDeliveries creates one pending delivery per destination that should
// receive an item.
func (s *Store) QueueDeliveries(ctx context.Context, feedID, itemID int64) (int, error) {
	var n int
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var err error
		n, err = queueDeliveries(ctx, tx, feedID, itemID)
		return err
	})
	return n, err
}

// queueDeliveries fans one entry out to every account that should receive it.
//
// The feed is shared, so this is where one fetch becomes many posts: each live
// subscription contributes its own account's unpaused, routed destinations. The
// prime watermark is what keeps that honest — an account that subscribed this
// morning has a watermark above every entry that existed then, so a feed it
// joined mid-life delivers only what arrives afterwards, even though the entries
// somebody else accumulated are sitting in the same table.
//
// The UNIQUE (item_id, destination_id) constraint makes this safe to call more
// than once, and the join is closed over sub.user_id at every step, so a
// delivery can only ever be created between rows belonging to one account.
func queueDeliveries(ctx context.Context, tx *sql.Tx, feedID, itemID int64) (int, error) {
	now := time.Now().UTC().Unix()
	res, err := tx.ExecContext(ctx, `
		INSERT INTO deliveries (user_id, item_id, destination_id, status, next_attempt_at, created_at, updated_at)
		SELECT sub.user_id, ?, d.id, 'pending', ?, ?, ?
		FROM subscriptions sub
		JOIN feed_destinations fd ON fd.feed_id = sub.feed_id AND fd.user_id = sub.user_id
		JOIN destinations d ON d.id = fd.destination_id AND d.user_id = sub.user_id AND d.paused = 0
		WHERE sub.feed_id = ? AND sub.paused = 0 AND sub.prime_item_id < ?
		ON CONFLICT (item_id, destination_id) DO NOTHING`,
		itemID, now, now, now, feedID, itemID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// DueDelivery is a queued delivery with everything sending it needs.
//
// It is one row on purpose. Resolving a delivery used to be five separate
// queries — the item, the destination, the feed for its title, the account for
// its timezone, then the status write — so a five-entry burst across seven
// destinations cost a hundred and seventy-five round trips through a pool that
// was one connection wide. All of it is available from one join.
type DueDelivery struct {
	ID            int64
	UserID        int64
	ItemID        int64
	DestinationID int64
	Attempts      int

	Item        Item
	Destination Destination
	FeedTitle   string
	FeedURL     string
	// Timezone is the account's own, which is what {{published}} renders in.
	Timezone string
}

// DueDeliveries claims work for the sender.
func (s *Store) DueDeliveries(ctx context.Context, limit int) ([]*DueDelivery, error) {
	rows, err := s.ro.QueryContext(ctx, `
		SELECT dl.id, dl.user_id, dl.item_id, dl.destination_id, dl.attempts,
			i.feed_id, i.guid, i.url, i.title, i.summary, i.author, i.published_at,
			d.user_id, d.kind, d.label, d.config, d.credentials, d.template, d.paused,
			f.title, f.url, u.timezone
		FROM deliveries dl
		JOIN items i ON i.id = dl.item_id
		JOIN destinations d ON d.id = dl.destination_id
		JOIN feeds f ON f.id = i.feed_id
		JOIN users u ON u.id = dl.user_id
		WHERE dl.status = 'pending' AND dl.next_attempt_at <= ?
		ORDER BY dl.next_attempt_at LIMIT ?`, time.Now().UTC().Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*DueDelivery
	for rows.Next() {
		var d DueDelivery
		var paused int
		var published sql.NullInt64
		if err := rows.Scan(&d.ID, &d.UserID, &d.ItemID, &d.DestinationID, &d.Attempts,
			&d.Item.FeedID, &d.Item.GUID, &d.Item.URL, &d.Item.Title, &d.Item.Summary,
			&d.Item.Author, &published,
			&d.Destination.UserID, &d.Destination.Kind, &d.Destination.Label,
			&d.Destination.Config, &d.Destination.Credentials, &d.Destination.Template, &paused,
			&d.FeedTitle, &d.FeedURL, &d.Timezone); err != nil {
			return nil, err
		}
		d.Item.ID, d.Destination.ID = d.ItemID, d.DestinationID
		d.Item.PublishedAt = scanTime(published)
		d.Destination.Paused = paused == 1
		out = append(out, &d)
	}
	return out, rows.Err()
}

// MarkDeliverySent finalises a successful delivery.
func (s *Store) MarkDeliverySent(ctx context.Context, id int64, remoteURL string) error {
	_, err := s.rw.ExecContext(ctx, `
		UPDATE deliveries SET status = 'sent', attempts = attempts + 1, last_error = '',
			remote_url = ?, updated_at = ?
		WHERE id = ?`, remoteURL, time.Now().UTC().Unix(), id)
	return err
}

// MarkDeliveryRetry schedules another attempt.
func (s *Store) MarkDeliveryRetry(ctx context.Context, id int64, at time.Time, reason string) error {
	_, err := s.rw.ExecContext(ctx, `
		UPDATE deliveries SET attempts = attempts + 1, next_attempt_at = ?, last_error = ?, updated_at = ?
		WHERE id = ?`, at.Unix(), reason, time.Now().UTC().Unix(), id)
	return err
}

// MarkDeliveryFailed gives up on a delivery.
func (s *Store) MarkDeliveryFailed(ctx context.Context, id int64, reason string) error {
	_, err := s.rw.ExecContext(ctx, `
		UPDATE deliveries SET status = 'failed', attempts = attempts + 1, last_error = ?, updated_at = ?
		WHERE id = ?`, reason, time.Now().UTC().Unix(), id)
	return err
}

// RequeueDelivery puts a failed delivery back in the queue, scoped to its
// owner. The attempt count goes back to zero: a delivery that used its six
// attempts would otherwise be abandoned again on the first try, which is not
// what asking for it again means. Only a failed delivery can be requeued, so a
// double-click cannot disturb one that is already on its way.
func (s *Store) RequeueDelivery(ctx context.Context, userID, id int64) error {
	now := time.Now().UTC().Unix()
	res, err := s.rw.ExecContext(ctx, `
		UPDATE deliveries
		SET status = 'pending', attempts = 0, next_attempt_at = ?, last_error = '', updated_at = ?
		WHERE id = ? AND user_id = ? AND status = 'failed'`, now, now, id, userID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// RecentDeliveries lists a user's delivery history, newest first.
func (s *Store) RecentDeliveries(ctx context.Context, userID int64, limit int) ([]*DeliveryView, error) {
	rows, err := s.ro.QueryContext(ctx, `
		SELECT dl.id, dl.user_id, dl.item_id, dl.destination_id, dl.status, dl.attempts,
			dl.next_attempt_at, dl.last_error, dl.remote_url, dl.created_at, dl.updated_at,
			i.title, i.url, f.title, d.kind, d.label
		FROM deliveries dl
		JOIN items i ON i.id = dl.item_id
		JOIN feeds f ON f.id = i.feed_id
		JOIN destinations d ON d.id = dl.destination_id
		WHERE dl.user_id = ?
		ORDER BY dl.id DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*DeliveryView
	for rows.Next() {
		var v DeliveryView
		var next, created, updated int64
		if err := rows.Scan(&v.ID, &v.UserID, &v.ItemID, &v.DestinationID, &v.Status, &v.Attempts,
			&next, &v.LastError, &v.RemoteURL, &created, &updated,
			&v.ItemTitle, &v.ItemURL, &v.FeedTitle, &v.DestinationKind, &v.DestinationName); err != nil {
			return nil, err
		}
		v.NextAttemptAt = time.Unix(next, 0).UTC()
		v.CreatedAt = time.Unix(created, 0).UTC()
		v.UpdatedAt = time.Unix(updated, 0).UTC()
		out = append(out, &v)
	}
	return out, rows.Err()
}

// TrimDeliveries drops history beyond the most recent n rows per account, for
// every account at once.
//
// Like TrimItems, this replaces a query per row of a loop that ran hourly over
// the whole table and found nothing to do in nearly every iteration. Pending
// rows are excluded from the count as well as from the delete: history is what
// grows without bound, and a queue that happened to be long at the moment the
// janitor ran must not have its tail deleted out from under the sender.
func (s *Store) TrimDeliveries(ctx context.Context, keep int) (int64, error) {
	res, err := s.rw.ExecContext(ctx, `
		DELETE FROM deliveries WHERE id IN (
			SELECT id FROM (
				SELECT id, row_number() OVER (PARTITION BY user_id ORDER BY id DESC) AS rn
				FROM deliveries WHERE status <> 'pending'
			) WHERE rn > ?
		)`, keep)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PendingCount reports how many deliveries are still queued for a user.
func (s *Store) PendingCount(ctx context.Context, userID int64) (int, error) {
	var n int
	err := s.ro.QueryRowContext(ctx,
		`SELECT count(*) FROM deliveries WHERE user_id = ? AND status = 'pending'`, userID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// QueueDeliveries creates one pending delivery per destination the feed is
// routed to, skipping paused ones. The UNIQUE (item_id, destination_id)
// constraint makes this safe to call more than once.
//
// Both the join and the where clause are scoped by owner, so a delivery can
// only ever be created between rows belonging to the same account.
func (s *Store) QueueDeliveries(ctx context.Context, userID, feedID, itemID int64) (int, error) {
	now := time.Now().UTC().Unix()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO deliveries (user_id, item_id, destination_id, status, next_attempt_at, created_at, updated_at)
		SELECT ?, ?, d.id, 'pending', ?, ?, ?
		FROM feed_destinations fd
		JOIN destinations d ON d.id = fd.destination_id AND d.user_id = fd.user_id
		WHERE fd.feed_id = ? AND fd.user_id = ? AND d.paused = 0
		ON CONFLICT (item_id, destination_id) DO NOTHING`,
		userID, itemID, now, now, now, feedID, userID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// DueDeliveries claims work for the sender.
func (s *Store) DueDeliveries(ctx context.Context, limit int) ([]*Delivery, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, item_id, destination_id, status, attempts, next_attempt_at,
			last_error, remote_url, created_at, updated_at
		FROM deliveries
		WHERE status = 'pending' AND next_attempt_at <= ?
		ORDER BY next_attempt_at LIMIT ?`, time.Now().UTC().Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Delivery
	for rows.Next() {
		var d Delivery
		var next, created, updated int64
		if err := rows.Scan(&d.ID, &d.UserID, &d.ItemID, &d.DestinationID, &d.Status, &d.Attempts,
			&next, &d.LastError, &d.RemoteURL, &created, &updated); err != nil {
			return nil, err
		}
		d.NextAttemptAt = time.Unix(next, 0).UTC()
		d.CreatedAt = time.Unix(created, 0).UTC()
		d.UpdatedAt = time.Unix(updated, 0).UTC()
		out = append(out, &d)
	}
	return out, rows.Err()
}

// DeliveryTargets resolves the item and destination a delivery refers to.
func (s *Store) DeliveryTargets(ctx context.Context, d *Delivery) (*Item, *Destination, error) {
	item, err := s.ItemByID(ctx, d.ItemID)
	if err != nil {
		return nil, nil, err
	}
	dest, err := s.destinationForDelivery(ctx, d.DestinationID)
	if err != nil {
		return nil, nil, err
	}
	if dest.UserID != d.UserID {
		// Cannot happen without corruption, but the worker sends under a user's
		// credentials, so verify rather than assume.
		return nil, nil, errors.New("store: delivery/destination owner mismatch")
	}
	return item, dest, nil
}

// MarkDeliverySent finalises a successful delivery.
func (s *Store) MarkDeliverySent(ctx context.Context, id int64, remoteURL string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE deliveries SET status = 'sent', attempts = attempts + 1, last_error = '',
			remote_url = ?, updated_at = ?
		WHERE id = ?`, remoteURL, time.Now().UTC().Unix(), id)
	return err
}

// MarkDeliveryRetry schedules another attempt.
func (s *Store) MarkDeliveryRetry(ctx context.Context, id int64, at time.Time, reason string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE deliveries SET attempts = attempts + 1, next_attempt_at = ?, last_error = ?, updated_at = ?
		WHERE id = ?`, at.Unix(), reason, time.Now().UTC().Unix(), id)
	return err
}

// MarkDeliveryFailed gives up on a delivery.
func (s *Store) MarkDeliveryFailed(ctx context.Context, id int64, reason string) error {
	_, err := s.db.ExecContext(ctx, `
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
	res, err := s.db.ExecContext(ctx, `
		UPDATE deliveries
		SET status = 'pending', attempts = 0, next_attempt_at = ?, last_error = '', updated_at = ?
		WHERE id = ? AND user_id = ? AND status = 'failed'`,
		time.Now().UTC().Unix(), time.Now().UTC().Unix(), id, userID)
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
	rows, err := s.db.QueryContext(ctx, `
		SELECT dl.id, dl.user_id, dl.item_id, dl.destination_id, dl.status, dl.attempts,
			dl.next_attempt_at, dl.last_error, dl.remote_url, dl.created_at, dl.updated_at,
			i.title, i.url, d.kind, d.label
		FROM deliveries dl
		JOIN items i ON i.id = dl.item_id
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
			&v.ItemTitle, &v.ItemURL, &v.DestinationKind, &v.DestinationName); err != nil {
			return nil, err
		}
		v.NextAttemptAt = time.Unix(next, 0).UTC()
		v.CreatedAt = time.Unix(created, 0).UTC()
		v.UpdatedAt = time.Unix(updated, 0).UTC()
		out = append(out, &v)
	}
	return out, rows.Err()
}

// TrimDeliveries drops history beyond the most recent n rows for a user.
func (s *Store) TrimDeliveries(ctx context.Context, userID int64, keep int) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM deliveries WHERE user_id = ? AND id NOT IN (
			SELECT id FROM deliveries WHERE user_id = ? ORDER BY id DESC LIMIT ?
		)`, userID, userID, keep)
	return err
}

// PendingCount reports how many deliveries are still queued for a user.
func (s *Store) PendingCount(ctx context.Context, userID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM deliveries WHERE user_id = ? AND status = 'pending'`, userID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const destColumns = `SELECT id, user_id, kind, label, config, credentials, template, paused,
	created_at, last_ok_at, last_error`

// CreateDestination stores a destination and connects it to whatever the
// account is already subscribed to, so adding one starts working without a
// second step.
//
// An account holds at most MaxDestinationsPerAccount, of any mix: two Mastodon
// accounts and three Discord channels are five. A caller that checked first
// cannot be relied on, because between its check and this insert sits a
// verification round trip to the service, so the count is read here, inside
// the write transaction, where nothing can get underneath it.
func (s *Store) CreateDestination(ctx context.Context, d *Destination) error {
	now := time.Now().UTC()
	return s.tx(ctx, func(tx *sql.Tx) error {
		var held int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM destinations WHERE user_id = ?`, d.UserID).Scan(&held); err != nil {
			return err
		}
		if held >= MaxDestinationsPerAccount {
			return ErrDestinationLimit
		}
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO destinations (user_id, kind, label, config, credentials, template, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING id`,
			d.UserID, d.Kind, d.Label, d.Config, d.Credentials, d.Template, now.Unix()).Scan(&d.ID); err != nil {
			return err
		}
		// Feeds are shared and carry no owner, so which feeds are "the account's"
		// is a question about its subscriptions.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO feed_destinations (feed_id, destination_id, user_id, created_at)
			SELECT sub.feed_id, ?, ?, ? FROM subscriptions sub WHERE sub.user_id = ?
			ON CONFLICT DO NOTHING`, d.ID, d.UserID, now.Unix(), d.UserID); err != nil {
			return err
		}
		d.CreatedAt = now
		return nil
	})
}

// DestinationsByUser lists a user's destinations.
func (s *Store) DestinationsByUser(ctx context.Context, userID int64) ([]*Destination, error) {
	rows, err := s.ro.QueryContext(ctx, destColumns+` FROM destinations WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Destination
	for rows.Next() {
		d, err := scanDestination(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Destination fetches one destination, scoped to its owner. Every handler must
// go through this rather than looking up by id alone.
func (s *Store) Destination(ctx context.Context, userID, id int64) (*Destination, error) {
	d, err := scanDestination(s.ro.QueryRowContext(ctx,
		destColumns+` FROM destinations WHERE id = ? AND user_id = ?`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return d, err
}

func (s *Store) UpdateDestination(ctx context.Context, d *Destination) error {
	res, err := s.rw.ExecContext(ctx, `
		UPDATE destinations SET label = ?, config = ?, credentials = ?, template = ?, paused = ?
		WHERE id = ? AND user_id = ?`,
		d.Label, d.Config, d.Credentials, d.Template, boolInt(d.Paused), d.ID, d.UserID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteDestination removes one of the account's destinations, taking its
// routes and queued deliveries with it by cascade. Someone else's destination
// is ErrNotFound, the same as one that does not exist.
func (s *Store) DeleteDestination(ctx context.Context, userID, id int64) error {
	res, err := s.rw.ExecContext(ctx, `DELETE FROM destinations WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RecordDestinationResult stores the outcome of the most recent send.
func (s *Store) RecordDestinationResult(ctx context.Context, id int64, sendErr string) error {
	if sendErr == "" {
		_, err := s.rw.ExecContext(ctx,
			`UPDATE destinations SET last_ok_at = ?, last_error = '' WHERE id = ?`,
			time.Now().UTC().Unix(), id)
		return err
	}
	_, err := s.rw.ExecContext(ctx, `UPDATE destinations SET last_error = ? WHERE id = ?`, sendErr, id)
	return err
}

// rowScanner is the part of *sql.Row and *sql.Rows that scanning needs, so one
// function serves a single lookup and a list.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanDestination(r rowScanner) (*Destination, error) {
	var d Destination
	var paused int
	var created int64
	var lastOK sql.NullInt64
	err := r.Scan(&d.ID, &d.UserID, &d.Kind, &d.Label, &d.Config, &d.Credentials, &d.Template,
		&paused, &created, &lastOK, &d.LastError)
	if err != nil {
		return nil, err
	}
	d.Paused = paused == 1
	d.CreatedAt = time.Unix(created, 0).UTC()
	d.LastOKAt = scanTime(lastOK)
	return &d, nil
}

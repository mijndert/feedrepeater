package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const destColumns = `SELECT id, user_id, kind, label, config, credentials, template, paused,
	created_at, last_ok_at, last_error`

// CreateDestination stores a destination and connects it to the account's
// existing feeds, so adding one starts working without a second step.
//
// An account gets one destination per kind. A caller that checked first cannot
// be relied on: between its check and this insert sits a webhook verification
// round trip, so simultaneous requests would each find the kind free. The
// unique index on (user_id, kind) settles it, and a violation comes back as
// ErrDuplicateKind rather than as a database error the handler cannot read.
func (s *Store) CreateDestination(ctx context.Context, d *Destination) error {
	now := time.Now().UTC()
	return s.tx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
			INSERT INTO destinations (user_id, kind, label, config, credentials, template, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING id`,
			d.UserID, d.Kind, d.Label, d.Config, d.Credentials, d.Template, now.Unix()).Scan(&d.ID)
		if isUniqueViolation(err) {
			return ErrDuplicateKind
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO feed_destinations (feed_id, destination_id, user_id, created_at)
			SELECT f.id, ?, ?, ? FROM feeds f WHERE f.user_id = ?
			ON CONFLICT DO NOTHING`, d.ID, d.UserID, now.Unix(), d.UserID); err != nil {
			return err
		}
		d.CreatedAt = now
		return nil
	})
}

// DestinationsByUser lists a user's destinations.
func (s *Store) DestinationsByUser(ctx context.Context, userID int64) ([]*Destination, error) {
	rows, err := s.db.QueryContext(ctx, destColumns+` FROM destinations WHERE user_id = ? ORDER BY id`, userID)
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
	d, err := scanDestination(s.db.QueryRowContext(ctx,
		destColumns+` FROM destinations WHERE id = ? AND user_id = ?`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return d, err
}

// destinationForDelivery fetches a destination without a user scope. It is
// unexported and used only by the worker, which resolves ownership from the
// delivery row it already holds.
func (s *Store) destinationForDelivery(ctx context.Context, id int64) (*Destination, error) {
	d, err := scanDestination(s.db.QueryRowContext(ctx, destColumns+` FROM destinations WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return d, err
}

func (s *Store) UpdateDestination(ctx context.Context, d *Destination) error {
	res, err := s.db.ExecContext(ctx, `
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

func (s *Store) DeleteDestination(ctx context.Context, userID, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM destinations WHERE id = ? AND user_id = ?`, id, userID)
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
		_, err := s.db.ExecContext(ctx,
			`UPDATE destinations SET last_ok_at = ?, last_error = '' WHERE id = ?`,
			time.Now().UTC().Unix(), id)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE destinations SET last_error = ? WHERE id = ?`, sendErr, id)
	return err
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

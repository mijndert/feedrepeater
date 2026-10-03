package store

import (
	"context"
	"database/sql"
	"time"
)

// SetFeedRoutes replaces the set of destinations one of the account's feeds
// publishes to.
//
// Only destinations the user owns can be attached: the insert selects from
// destinations filtered by owner rather than trusting the submitted ids, so a
// request naming someone else's destination attaches nothing rather than
// failing in a way that reveals the id exists.
func (s *Store) SetFeedRoutes(ctx context.Context, userID, feedID int64, destinationIDs []int64) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		// The caller must actually be subscribed to the feed. Feeds are shared,
		// so "is this yours" is a question about the subscription rather than
		// about the feed — and asking the feed would let any account route a
		// feed it merely knows the id of.
		var owned int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM subscriptions WHERE feed_id = ? AND user_id = ?`, feedID, userID).Scan(&owned); err != nil {
			return err
		}
		if owned == 0 {
			return ErrNotFound
		}

		if _, err := tx.ExecContext(ctx,
			`DELETE FROM feed_destinations WHERE feed_id = ? AND user_id = ?`, feedID, userID); err != nil {
			return err
		}
		if len(destinationIDs) == 0 {
			return nil
		}

		args := make([]any, 0, len(destinationIDs)+4)
		args = append(args, feedID, userID, time.Now().UTC().Unix(), userID)
		for _, id := range destinationIDs {
			args = append(args, id)
		}
		query := `
			INSERT INTO feed_destinations (feed_id, destination_id, user_id, created_at)
			SELECT ?, d.id, ?, ?
			FROM destinations d
			WHERE d.user_id = ? AND d.id IN (` + placeholders(len(destinationIDs)) + `)
			ON CONFLICT DO NOTHING`
		_, err := tx.ExecContext(ctx, query, args...)
		return err
	})
}

// FeedRoutes lists the destination ids a feed publishes to for this account.
func (s *Store) FeedRoutes(ctx context.Context, userID, feedID int64) ([]int64, error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT destination_id FROM feed_destinations WHERE feed_id = ? AND user_id = ? ORDER BY destination_id`,
		feedID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Connecting a new feed to the account's existing destinations, and a new
// destination to its existing feeds, happens inside Subscribe and
// CreateDestination respectively. It belongs in those transactions rather than
// in a helper here: a feed that exists for even a moment without its routing
// would poll and deliver nowhere.

// RoutedDestination is a destination as shown against a feed.
type RoutedDestination struct {
	Destination
	// Routed reports whether this destination receives the feed.
	Routed bool
}

// DestinationsForFeed lists every destination the user owns, marking the ones
// the feed publishes to. The feed page needs both halves to render the
// checkboxes. A feed the account does not follow marks nothing, and the list
// is still the account's own destinations — never anyone else's.
func (s *Store) DestinationsForFeed(ctx context.Context, userID, feedID int64) ([]*RoutedDestination, error) {
	rows, err := s.ro.QueryContext(ctx, `
		SELECT d.id, d.user_id, d.kind, d.label, d.config, d.credentials, d.template, d.paused,
			d.created_at, d.last_ok_at, d.last_error,
			CASE WHEN fd.destination_id IS NULL THEN 0 ELSE 1 END AS routed
		FROM destinations d
		LEFT JOIN feed_destinations fd
			ON fd.destination_id = d.id AND fd.feed_id = ? AND fd.user_id = ?
		WHERE d.user_id = ?
		ORDER BY d.id`, feedID, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*RoutedDestination
	for rows.Next() {
		var d Destination
		var paused, routed int
		var created int64
		var lastOK sql.NullInt64
		if err := rows.Scan(&d.ID, &d.UserID, &d.Kind, &d.Label, &d.Config, &d.Credentials, &d.Template,
			&paused, &created, &lastOK, &d.LastError, &routed); err != nil {
			return nil, err
		}
		d.Paused = paused == 1
		d.CreatedAt = time.Unix(created, 0).UTC()
		d.LastOKAt = scanTime(lastOK)
		out = append(out, &RoutedDestination{Destination: d, Routed: routed == 1})
	}
	return out, rows.Err()
}

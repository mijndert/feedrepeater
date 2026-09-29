package store

import (
	"context"
)

// FeedRoutes lists the destination ids a feed publishes to.
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

// Nothing here writes routing, and there is no longer anything that lets a
// person edit it. Connecting a new feed to the account's destination, and a new
// destination to its existing feed, happens inside Subscribe and
// CreateDestination respectively — in those transactions rather than in a helper
// here, because a feed that exists for even a moment without its routing would
// poll and deliver nowhere.
//
// The table stays a table. It is what makes more than one feed per account, or
// a second service, a change of code rather than a change of shape.

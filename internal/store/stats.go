package store

import "context"

// Stats holds aggregate counts describing the service as a whole.
//
// Every field is a count. No handle, feed URL, instance host or destination
// label belongs in this struct: it exists to be published, and the reason
// aggregates are safe to publish is that they say nothing about any one account.
type Stats struct {
	Users     int
	Instances int

	Feeds       int
	FeedsActive int
	// Subscriptions is how many account-to-feed links exist. Held against
	// Feeds it is the whole argument for sharing them: the gap between the two
	// is fetches that are no longer made.
	Subscriptions int
	Items         int

	Destinations int
	// DestinationsByKind is keyed by the kind column, e.g. "mastodon". Kinds
	// nobody has configured are absent rather than zero.
	DestinationsByKind map[string]int

	Deliveries        int
	DeliveriesSent    int
	DeliveriesPending int
	DeliveriesFailed  int
}

// Stats collects the counts in two queries.
//
// The scalar subqueries are one round trip on purpose, and both run on a reader
// connection. Every field is a count(*), which SQLite answers by walking the
// table, so this is the most expensive read the service performs and the one
// most worth keeping off the writer. Keeping it off a request's critical path
// is the caller's job: see web.Server.stats, which serves the previous answer
// and recomputes behind it, and admits one computation at a time.
func (s *Store) Stats(ctx context.Context) (*Stats, error) {
	var st Stats
	err := s.ro.QueryRowContext(ctx, `
		SELECT (SELECT count(*) FROM users),
		       (SELECT count(*) FROM instances),
		       (SELECT count(*) FROM feeds),
		       (SELECT count(*) FROM feed_state WHERE disabled = 0),
		       (SELECT count(*) FROM subscriptions),
		       (SELECT count(*) FROM items),
		       (SELECT count(*) FROM destinations),
		       (SELECT count(*) FROM deliveries),
		       (SELECT count(*) FROM deliveries WHERE status = 'sent'),
		       (SELECT count(*) FROM deliveries WHERE status = 'pending'),
		       (SELECT count(*) FROM deliveries WHERE status = 'failed')`,
	).Scan(
		&st.Users, &st.Instances,
		&st.Feeds, &st.FeedsActive, &st.Subscriptions, &st.Items,
		&st.Destinations,
		&st.Deliveries, &st.DeliveriesSent, &st.DeliveriesPending, &st.DeliveriesFailed,
	)
	if err != nil {
		return nil, err
	}

	rows, err := s.ro.QueryContext(ctx,
		`SELECT kind, count(*) FROM destinations GROUP BY kind`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	st.DestinationsByKind = map[string]int{}
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			return nil, err
		}
		st.DestinationsByKind[kind] = n
	}
	return &st, rows.Err()
}

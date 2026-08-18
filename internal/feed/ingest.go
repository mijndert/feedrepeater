package feed

import (
	"context"

	"feedrepeater.com/internal/store"
)

// Recorder is the part of the store ingestion needs.
type Recorder interface {
	InsertItem(ctx context.Context, item *store.Item) (bool, error)
	QueueDeliveries(ctx context.Context, userID, feedID, itemID int64) (int, error)
}

// IngestResult reports what one ingestion did.
type IngestResult struct {
	// New is how many entries had not been seen before.
	New int
	// Queued is how many deliveries were created.
	Queued int
	// Skipped is how many new entries were recorded but deliberately not
	// delivered, because the burst exceeded maxDeliver.
	Skipped int
}

// Ingest records entries against a feed.
//
// When prime is true nothing is delivered: every entry is written as already
// seen. That is what makes adding a feed start from the moment it was added
// rather than replaying its archive into someone's timeline. Priming happens
// when the feed is first saved and again on the first successful poll if the
// save-time fetch did not get that far, so there is no window in which an
// existing entry can be treated as new.
func Ingest(ctx context.Context, rec Recorder, feedID, userID int64, entries []Entry, prime bool, maxDeliver int) (IngestResult, error) {
	var res IngestResult

	var fresh []int64
	for _, e := range entries {
		item := &store.Item{
			FeedID:      feedID,
			GUID:        e.GUID,
			URL:         e.URL,
			Title:       e.Title,
			Summary:     e.Summary,
			Author:      e.Author,
			PublishedAt: e.Published,
		}
		isNew, err := rec.InsertItem(ctx, item)
		if err != nil {
			return res, err
		}
		if isNew {
			fresh = append(fresh, item.ID)
		}
	}
	res.New = len(fresh)

	if prime || len(fresh) == 0 {
		res.Skipped = len(fresh)
		return res, nil
	}

	// A burst larger than the cap usually means the feed changed its ids or
	// republished itself, not that the author posted forty times in an hour.
	// Deliver the newest few and leave the rest recorded as seen.
	if maxDeliver > 0 && len(fresh) > maxDeliver {
		res.Skipped = len(fresh) - maxDeliver
		fresh = fresh[len(fresh)-maxDeliver:]
	}

	for _, id := range fresh {
		n, err := rec.QueueDeliveries(ctx, userID, feedID, id)
		if err != nil {
			return res, err
		}
		res.Queued += n
	}
	return res, nil
}

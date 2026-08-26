package feed

import (
	"context"

	"feedrepeater.com/internal/store"
)

// Recorder is the part of the store ingestion needs.
type Recorder interface {
	RecordEntries(ctx context.Context, feedID int64, items []store.Item, deliver bool, maxDeliver int) (store.RecordResult, error)
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
//
// The whole batch goes down as one transaction. Which entries are new, and
// which of those are inside the burst cap, is decided in the store with the
// inserted ids in hand, because the answer and the deliveries it produces have
// to commit together or not at all.
func Ingest(ctx context.Context, rec Recorder, feedID int64, entries []Entry, prime bool, maxDeliver int) (IngestResult, error) {
	items := make([]store.Item, len(entries))
	for i, e := range entries {
		items[i] = store.Item{
			FeedID:      feedID,
			GUID:        e.GUID,
			URL:         e.URL,
			Title:       e.Title,
			Summary:     e.Summary,
			Author:      e.Author,
			PublishedAt: e.Published,
		}
	}
	res, err := rec.RecordEntries(ctx, feedID, items, !prime, maxDeliver)
	return IngestResult{New: res.New, Queued: res.Queued, Skipped: res.Skipped}, err
}

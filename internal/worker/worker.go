// Package worker runs the background loops: polling feeds, sending queued
// deliveries, and clearing out expired rows.
package worker

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"feedrepeater.com/internal/config"
	"feedrepeater.com/internal/feed"
	"feedrepeater.com/internal/publisher"
	"feedrepeater.com/internal/store"
)

// Tuning constants. These are deliberately conservative: the service is a
// relay, and being slow is much cheaper than being noisy.
const (
	pollTick       = 30 * time.Second
	sendTick       = 10 * time.Second
	janitorTick    = time.Hour
	feedsPerTick   = 40
	sendsPerTick   = 10
	sendConcurrent = 4
	pollConcurrent = 4
	keepItems      = 500
	keepDeliveries = 200

	// hostSpacing is the minimum gap between two requests to the same host.
	// Several accounts can subscribe to feeds on one popular domain, and
	// arriving all at once looks like a small attack from the other side.
	hostSpacing = 5 * time.Second
)

type Worker struct {
	cfg   *config.Config
	store *store.Store
	fetch *feed.Fetcher
	pub   *publisher.Publisher
	log   *slog.Logger

	// nextHostFetch spaces out requests per host. It is in-memory only: on
	// restart the worst case is one unspaced round, which is harmless.
	hostMu        sync.Mutex
	nextHostFetch map[string]time.Time
}

func New(cfg *config.Config, st *store.Store, f *feed.Fetcher, pub *publisher.Publisher, log *slog.Logger) *Worker {
	return &Worker{
		cfg: cfg, store: st, fetch: f, pub: pub, log: log,
		nextHostFetch: map[string]time.Time{},
	}
}

// Run blocks until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, loop := range []struct {
		name string
		tick time.Duration
		fn   func(context.Context)
	}{
		{"poll", pollTick, w.pollDue},
		{"send", sendTick, w.sendDue},
		{"janitor", janitorTick, w.clean},
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.every(ctx, loop.name, loop.tick, loop.fn)
		}()
	}
	wg.Wait()
}

func (w *Worker) every(ctx context.Context, name string, d time.Duration, fn func(context.Context)) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			func() {
				defer func() {
					if r := recover(); r != nil {
						w.log.Error("worker panic", "loop", name, "panic", r)
					}
				}()
				fn(ctx)
			}()
		}
	}
}

// --- polling ---------------------------------------------------------------

func (w *Worker) pollDue(ctx context.Context) {
	feeds, err := w.store.DueFeeds(ctx, feedsPerTick)
	if err != nil {
		w.log.Error("list due feeds", "error", err)
		return
	}
	if len(feeds) == 0 {
		return
	}

	sem := make(chan struct{}, pollConcurrent)
	var wg sync.WaitGroup
	for _, df := range feeds {
		// A feed whose host was contacted moments ago waits for the next tick
		// rather than queueing up behind itself.
		if !w.claimHost(df.URL) {
			continue
		}
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(df *store.DueFeed) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					w.log.Error("poll panic", "feed", df.ID, "panic", r)
				}
			}()
			w.poll(ctx, df)
		}(df)
	}
	wg.Wait()
}

// claimHost reports whether this host may be contacted now, reserving the next
// slot if so.
func (w *Worker) claimHost(rawURL string) bool {
	host := rawURL
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		host = strings.ToLower(u.Host)
	}
	now := time.Now()

	w.hostMu.Lock()
	defer w.hostMu.Unlock()

	if len(w.nextHostFetch) > 10_000 {
		for k, t := range w.nextHostFetch {
			if now.After(t) {
				delete(w.nextHostFetch, k)
			}
		}
	}
	if t, ok := w.nextHostFetch[host]; ok && now.Before(t) {
		return false
	}
	w.nextHostFetch[host] = now.Add(hostSpacing)
	return true
}

func (w *Worker) poll(ctx context.Context, df *store.DueFeed) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	f := df.Feed
	now := time.Now().UTC()
	res, err := w.fetch.Fetch(ctx, f.URL, f.ETag, f.LastModified)
	f.LastFetchAt = &now

	if err != nil {
		w.recordFailure(ctx, f, now, err)
		return
	}

	f.Failures = 0
	f.LastError = ""
	f.ETag = res.ETag
	f.LastModified = res.LastModified

	var ingested feed.IngestResult
	if !res.NotModified {
		if res.Title != "" {
			f.Title = res.Title
		}
		var err error
		if ingested, err = w.ingest(ctx, f, res.Entries); err != nil {
			w.log.Error("ingest entries", "feed", f.ID, "error", err)
		}
	}

	// New entries are the only signal that a feed is alive. A server without
	// conditional-request support answers 200 with identical content forever;
	// treating that as a change would defeat the whole schedule.
	if ingested.New > 0 {
		f.ChangedAt = &now
	}

	wait := w.cfg.MinPollInterval
	f.NextFetchAt = now.Add(withJitter(wait))

	// One line per fetch, always. A service that only speaks up when something
	// breaks is indistinguishable from a stopped one.
	w.log.Info("feed checked",
		"feed", f.ID,
		"modified", !res.NotModified,
		"entries", len(res.Entries),
		"new", ingested.New,
		"queued", ingested.Queued,
		"routed", df.HasRoutes,
		"next_check_in", wait.Round(time.Second).String())

	if err := w.store.RecordFetch(ctx, f); err != nil {
		w.log.Error("record fetch", "feed", f.ID, "error", err)
	}
}

// recordFailure schedules the next attempt, honours any Retry-After the server
// sent, and eventually stops asking altogether.
func (w *Worker) recordFailure(ctx context.Context, f *store.Feed, now time.Time, cause error) {
	f.Failures++
	f.LastError = truncate(cause.Error(), 300)

	var retryAfter time.Duration
	var fe *feed.FetchError
	if errors.As(cause, &fe) {
		retryAfter = fe.RetryAfter
	}
	delay := retryDelay(w.cfg.MinPollInterval, retryAfter)

	// A feed that has failed for this long in a row is gone, not having a bad
	// afternoon. Stop asking; the dashboard shows why.
	if deadFeed(w.cfg.MinPollInterval, f.Failures) && !f.Paused {
		f.Paused = true
		f.LastError = "Paused after repeated failures. " + f.LastError
		w.log.Warn("feed paused after repeated failures",
			"feed", f.ID, "failures", f.Failures, "error", cause)
	} else {
		w.log.Info("feed fetch failed",
			"feed", f.ID, "failures", f.Failures,
			"retry_in", delay.Round(time.Second).String(), "error", cause)
	}

	f.NextFetchAt = now.Add(withJitter(delay))
	if err := w.store.RecordFetch(ctx, f); err != nil {
		w.log.Error("record fetch", "feed", f.ID, "error", err)
	}
}

// ingest records entries and queues deliveries for the new ones.
//
// A feed that has not been primed yet records everything as seen and delivers
// nothing. Priming normally happens when the feed is saved; this covers the
// case where the save-time fetch failed, so the first poll to succeed is still
// a baseline rather than an archive replay.
func (w *Worker) ingest(ctx context.Context, f *store.Feed, entries []feed.Entry) (feed.IngestResult, error) {
	prime := !f.Primed
	res, err := feed.Ingest(ctx, w.store, f.ID, f.UserID, entries, prime, w.cfg.MaxItemsPerPoll)
	if err != nil {
		return res, err
	}
	if prime {
		f.Primed = true
		w.log.Info("feed primed", "feed", f.ID, "entries", res.New)
		return res, nil
	}
	if res.Skipped > 0 {
		w.log.Warn("entry burst capped", "feed", f.ID, "skipped", res.Skipped)
	}
	return res, nil
}

// --- sending ---------------------------------------------------------------

func (w *Worker) sendDue(ctx context.Context) {
	deliveries, err := w.store.DueDeliveries(ctx, sendsPerTick)
	if err != nil {
		w.log.Error("list due deliveries", "error", err)
		return
	}
	if len(deliveries) == 0 {
		return
	}

	sem := make(chan struct{}, sendConcurrent)
	var wg sync.WaitGroup
	for _, dl := range deliveries {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(dl *store.Delivery) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					w.log.Error("delivery panic", "delivery", dl.ID, "panic", r)
				}
			}()
			w.pub.Deliver(ctx, dl)
		}(dl)
	}
	wg.Wait()
}

// --- housekeeping ----------------------------------------------------------

func (w *Worker) clean(ctx context.Context) {
	if err := w.store.DeleteExpired(ctx); err != nil {
		w.log.Error("delete expired", "error", err)
	}
	feeds, err := w.store.AllFeedIDs(ctx)
	if err != nil {
		w.log.Error("list feeds", "error", err)
		return
	}
	for _, f := range feeds {
		if err := w.store.TrimItems(ctx, f.FeedID, keepItems); err != nil {
			w.log.Error("trim items", "feed", f.FeedID, "error", err)
		}
		if err := w.store.TrimDeliveries(ctx, f.UserID, keepDeliveries); err != nil {
			w.log.Error("trim deliveries", "user", f.UserID, "error", err)
		}
	}
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

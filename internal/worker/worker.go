// Package worker runs the background loops: polling feeds, sending queued
// deliveries, and clearing out expired rows.
package worker

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"runtime"
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
	pollTick    = 30 * time.Second
	janitorTick = time.Hour

	// sendIdle is how long the sender waits between looks when the queue has
	// been empty. It is long because it is no longer how deliveries are
	// noticed — ingesting an entry wakes the loop directly — so this is only
	// the backstop that catches a retry coming due.
	sendIdle = 2 * time.Minute
	// sendBusy is the gap while there is still work, so a backlog drains at a
	// steady rate rather than in one burst per tick.
	sendBusy = 2 * time.Second

	// feedsPerTick and sendsPerTick bound one pass, not the rate. A pass that
	// comes back full means there is more waiting, and the loop goes round again
	// immediately rather than sleeping on a queue it knows is deep — so these
	// size the batch and drainRounds sizes the burst.
	feedsPerTick = 100
	sendsPerTick = 50
	drainRounds  = 5

	keepItems      = 500
	keepDeliveries = 200

	// hostSpacing is the minimum gap between two requests to the same host.
	// Several accounts can subscribe to feeds on one popular domain, and
	// arriving all at once looks like a small attack from the other side.
	hostSpacing = 5 * time.Second
	// hostDefer is how far a feed is pushed out when its host was just
	// contacted. Without it the feed keeps its place at the head of the due
	// queue and is selected, skipped and reselected on every tick, holding a
	// batch slot each time and blocking whatever is behind it.
	hostDefer = 30 * time.Second
)

// concurrency sizes the fan-out for polling and sending. Both are waiting on
// somebody else's server rather than on this machine, so it is not bounded by
// cores in the way a compute loop would be — but it is bounded by the memory a
// parse takes, which is why it is not simply large.
func concurrency() int {
	return min(max(runtime.NumCPU()*2, 4), 16)
}

type Worker struct {
	cfg   *config.Config
	store *store.Store
	fetch *feed.Fetcher
	pub   *publisher.Publisher
	log   *slog.Logger

	// wake carries "something was queued" to the sender. Buffered by one: the
	// signal is a fact about the queue, not a count of it, so a second one
	// arriving before the first is read has nothing to add.
	wake chan struct{}

	// nextHostFetch spaces out requests per host. It is in-memory only: on
	// restart the worst case is one unspaced round, which is harmless.
	hostMu        sync.Mutex
	nextHostFetch map[string]time.Time
	nextHostSweep time.Time
}

func New(cfg *config.Config, st *store.Store, f *feed.Fetcher, pub *publisher.Publisher, log *slog.Logger) *Worker {
	return &Worker{
		cfg: cfg, store: st, fetch: f, pub: pub, log: log,
		wake:          make(chan struct{}, 1),
		nextHostFetch: map[string]time.Time{},
	}
}

// Notify tells the sender there is something to send.
//
// Without it the only way a queued delivery was noticed was the next tick of a
// loop that ran regardless, which meant the tick had to be short to feel
// responsive and therefore woke the process eight and a half thousand times a
// day to look at an empty table. Being told means the loop can idle for minutes
// and still send immediately.
func (w *Worker) Notify() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run blocks until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, loop := range []struct {
		name string
		fn   func(context.Context)
	}{
		{"poll", func(ctx context.Context) { w.every(ctx, "poll", pollTick, w.pollDue) }},
		{"send", w.runSender},
		{"janitor", func(ctx context.Context) { w.every(ctx, "janitor", janitorTick, w.clean) }},
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			loop.fn(ctx)
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
			w.guard(name, func() { fn(ctx) })
		}
	}
}

func (w *Worker) guard(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			w.log.Error("worker panic", "loop", name, "panic", r)
		}
	}()
	fn()
}

// --- polling ---------------------------------------------------------------

func (w *Worker) pollDue(ctx context.Context) {
	for round := 0; round < drainRounds; round++ {
		// polled, not claimed: a batch made entirely of feeds deferred for host
		// spacing did no fetching and is not evidence of a backlog. Counting the
		// claim would spin the drain loop and then report a capacity problem
		// that is really the spacing rule working.
		polled := w.pollBatch(ctx)
		if polled < feedsPerTick || ctx.Err() != nil {
			return
		}
	}
	// Still full after the last round. The queue is deeper than this process can
	// drain at the configured interval, which is a capacity fact worth saying
	// out loud rather than letting feeds quietly drift late.
	w.log.Warn("poll queue still full after draining",
		"batch", feedsPerTick, "rounds", drainRounds,
		"hint", "feeds are being checked later than the configured interval")
}

// pollBatch polls one batch and reports how many feeds it actually fetched.
func (w *Worker) pollBatch(ctx context.Context) int {
	feeds, err := w.store.DueFeeds(ctx, feedsPerTick)
	if err != nil {
		w.log.Error("list due feeds", "error", err)
		return 0
	}
	if len(feeds) == 0 {
		return 0
	}

	sem := make(chan struct{}, concurrency())
	var wg sync.WaitGroup
	var deferred []int64
	polled := 0

	for _, df := range feeds {
		// A feed whose host was contacted moments ago is pushed out rather than
		// skipped, so it stops occupying a slot in every batch until its turn.
		if !w.claimHost(df.URL) {
			deferred = append(deferred, df.FeedID)
			continue
		}
		select {
		case <-ctx.Done():
			wg.Wait()
			w.deferFeeds(ctx, deferred)
			return polled
		case sem <- struct{}{}:
		}
		polled++
		wg.Add(1)
		go func(df *store.DueFeed) {
			defer wg.Done()
			defer func() { <-sem }()
			w.guard("poll", func() { w.poll(ctx, df) })
		}(df)
	}
	wg.Wait()
	w.deferFeeds(ctx, deferred)
	return polled
}

func (w *Worker) deferFeeds(ctx context.Context, ids []int64) {
	if len(ids) == 0 {
		return
	}
	if err := w.store.DeferFeeds(ctx, ids, time.Now().UTC().Add(withJitter(hostDefer))); err != nil {
		w.log.Error("defer feeds", "count", len(ids), "error", err)
	}
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

	// Sweep on a timer rather than only once the map is large, so a long tail
	// of one-off hosts cannot sit in it until something forces a clear-out.
	if now.After(w.nextHostSweep) {
		for k, t := range w.nextHostFetch {
			if now.After(t) {
				delete(w.nextHostFetch, k)
			}
		}
		w.nextHostSweep = now.Add(time.Minute)
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

	now := time.Now().UTC()
	st := df.FetchState
	res, err := w.fetch.Fetch(ctx, df.URL, st.ETag, st.LastModified, st.BodyHash)
	st.LastFetchAt = &now

	if err != nil {
		w.recordFailure(ctx, df, &st, now, err)
		return
	}

	st.Failures = 0
	st.LastError = ""
	st.FailingSince = nil
	st.ETag = res.ETag
	st.LastModified = res.LastModified

	var ingested feed.IngestResult
	var ingestErr error
	// Three ways to learn nothing happened, in ascending order of cost: the
	// server said so, the bytes were identical, or the entries were all ones we
	// already had. Only the third reaches the database.
	parsed := !res.NotModified && !res.Unchanged
	if parsed {
		if res.Title != "" && res.Title != df.Title {
			if err := w.store.SetFeedTitle(ctx, df.FeedID, res.Title); err != nil {
				w.log.Error("record feed title", "feed", df.FeedID, "error", err)
			}
		}
		if ingested, ingestErr = w.ingest(ctx, df, res.Entries); ingestErr != nil {
			w.log.Error("ingest entries", "feed", df.FeedID, "error", ingestErr)
		}
	}

	// The stored hash asserts "this document's entries are recorded", so it may
	// only be adopted once they are. Recording it after a failed ingest would
	// make the next poll see an unchanged body, skip the parse, and drop that
	// batch permanently — RecordEntries is all-or-nothing, so nothing of it
	// would have been written. Keeping the old hash costs one redundant parse,
	// and every entry in it dedupes on (feed_id, guid).
	if len(res.BodyHash) > 0 && ingestErr == nil {
		st.BodyHash = res.BodyHash
	}

	// New entries are the only signal that a feed is alive. Nothing schedules
	// on it any more, but it is the honest answer to "when did this last post"
	// and costs nothing to keep.
	if ingested.New > 0 {
		st.ChangedAt = &now
	}
	if ingested.Queued > 0 {
		w.Notify()
	}

	wait := pollInterval(w.cfg.PollInterval, res.Hint)
	st.NextFetchAt = now.Add(withJitter(wait))

	// A fetch that found nothing is the overwhelming majority of fetches, and
	// saying so every time buries the ones that matter under half a million
	// lines a day. The interesting events keep their line at Info; the rest is
	// there at Debug for anyone who turns it on.
	level := slog.LevelDebug
	if ingested.New > 0 || ingested.Skipped > 0 {
		level = slog.LevelInfo
	}
	w.log.Log(ctx, level, "feed checked",
		"feed", df.FeedID,
		"not_modified", res.NotModified,
		"unchanged", res.Unchanged,
		"entries", len(res.Entries),
		"new", ingested.New,
		"queued", ingested.Queued,
		"next_check_in", wait.Round(time.Second).String())

	if err := w.store.RecordFetch(ctx, &st); err != nil {
		w.log.Error("record fetch", "feed", df.FeedID, "error", err)
	}
}

// recordFailure schedules the next attempt, honours any Retry-After the server
// sent, and eventually stops asking altogether.
func (w *Worker) recordFailure(ctx context.Context, df *store.DueFeed, st *store.FetchState, now time.Time, cause error) {
	st.Failures++
	st.LastError = truncate(cause.Error(), 300)

	var retryAfter time.Duration
	var fe *feed.FetchError
	if errors.As(cause, &fe) {
		retryAfter = fe.RetryAfter
	}
	// The streak starts at the first failure after a success and is cleared by
	// the next one, so the rule below measures the outage rather than guessing
	// at it from a count banked across changing intervals.
	if st.FailingSince == nil {
		st.FailingSince = &now
	}

	// The ordinary interval is what it would have waited had this succeeded.
	delay := retryDelay(w.cfg.PollInterval, retryAfter)

	// A feed that has failed for this long in a row is gone, not having a bad
	// afternoon. Stop asking; the dashboard shows why. Unlike a subscriber's own
	// pause this applies to everyone, because what is missing is the document.
	dead := deadFeed(now, st.FailingSince) && !df.Disabled
	if dead {
		st.LastError = "Stopped after repeated failures. " + st.LastError
		w.log.Warn("feed stopped after repeated failures",
			"feed", df.FeedID, "failures", st.Failures,
			"failing_for", now.Sub(*st.FailingSince).Round(time.Hour).String(), "error", cause)
	} else {
		w.log.Info("feed fetch failed",
			"feed", df.FeedID, "failures", st.Failures,
			"retry_in", delay.Round(time.Second).String(), "error", cause)
	}

	st.NextFetchAt = now.Add(withJitter(delay))
	if err := w.store.RecordFetch(ctx, st); err != nil {
		w.log.Error("record fetch", "feed", df.FeedID, "error", err)
	}
	// disabled is not RecordFetch's column to write — see its comment — so the
	// dead-feed rule has to say so itself. Ordered after the fetch state, since
	// the reason it stopped is the last_error recorded above.
	if dead {
		if err := w.store.DisableFeed(ctx, df.FeedID); err != nil {
			w.log.Error("disable dead feed", "feed", df.FeedID, "error", err)
		}
	}
}

// ingest records entries and queues deliveries for the new ones.
//
// There is no priming pass here, and no feed-level primed flag left to consult.
// Priming was a property of the feed when a feed belonged to one account; now
// that the document is shared it is a property of each subscription, and the
// watermark taken when the subscription was written does the whole job. An
// account that joined this morning has a watermark above every entry that
// existed then, so the fan-out skips it without the feed needing to know — and
// that holds on the first poll after subscribing exactly as it does on the
// thousandth.
func (w *Worker) ingest(ctx context.Context, df *store.DueFeed, entries []feed.Entry) (feed.IngestResult, error) {
	res, err := feed.Ingest(ctx, w.store, df.FeedID, entries, false, w.cfg.MaxItemsPerPoll)
	if err != nil {
		return res, err
	}
	if res.Skipped > 0 && res.Queued > 0 {
		w.log.Warn("entry burst capped", "feed", df.FeedID, "skipped", res.Skipped)
	}
	return res, nil
}

// --- sending ---------------------------------------------------------------

// runSender drains the delivery queue, woken by Notify and by its own timer.
func (w *Worker) runSender(ctx context.Context) {
	t := time.NewTimer(sendBusy)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
		case <-t.C:
		}

		busy := false
		w.guard("send", func() { busy = w.sendDue(ctx) })

		next := sendIdle
		if busy {
			next = sendBusy
		}
		if !t.Stop() {
			select {
			case <-t.C:
			default:
			}
		}
		t.Reset(next)
	}
}

// sendDue sends one batch and reports whether the queue may still hold more.
func (w *Worker) sendDue(ctx context.Context) bool {
	deliveries, err := w.store.DueDeliveries(ctx, sendsPerTick)
	if err != nil {
		w.log.Error("list due deliveries", "error", err)
		return false
	}
	if len(deliveries) == 0 {
		return false
	}

	sem := make(chan struct{}, concurrency())
	var wg sync.WaitGroup
	for _, dl := range deliveries {
		select {
		case <-ctx.Done():
			wg.Wait()
			return false
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(dl *store.DueDelivery) {
			defer wg.Done()
			defer func() { <-sem }()
			w.guard("deliver", func() { w.pub.Deliver(ctx, dl) })
		}(dl)
	}
	wg.Wait()
	return len(deliveries) == sendsPerTick
}

// --- housekeeping ----------------------------------------------------------

func (w *Worker) clean(ctx context.Context) {
	if err := w.store.DeleteExpired(ctx); err != nil {
		w.log.Error("delete expired", "error", err)
	}
	// Two statements for the whole database. This was a query per feed and a
	// query per feed's owner, issued from a loop over every feed there was, and
	// nearly every one of them had nothing to delete.
	items, err := w.store.TrimItems(ctx, keepItems)
	if err != nil {
		w.log.Error("trim items", "error", err)
	}
	deliveries, err := w.store.TrimDeliveries(ctx, keepDeliveries)
	if err != nil {
		w.log.Error("trim deliveries", "error", err)
	}
	// Every path that drops a subscription collects behind itself, so this
	// normally finds nothing. It is here for what a crash between the two
	// statements would leave: a feed polled forever with nobody to deliver it to.
	orphans, err := w.store.DeleteOrphanFeeds(ctx)
	if err != nil {
		w.log.Error("delete orphan feeds", "error", err)
	}
	if err := w.store.Optimize(ctx); err != nil {
		w.log.Error("optimize", "error", err)
	}
	w.log.Info("housekeeping",
		"items_trimmed", items, "deliveries_trimmed", deliveries, "orphan_feeds", orphans)
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"feedrepeater.com/internal/destination"
	"feedrepeater.com/internal/mastodon"
	"feedrepeater.com/internal/render"
	"feedrepeater.com/internal/store"
)

type dashboardData struct {
	// Feeds is every feed the account follows, oldest first.
	Feeds    []*store.FeedView
	MaxFeeds int
	// FeedURL is what was typed into the add form, kept on a rejected
	// submission so it can be corrected rather than retyped.
	FeedURL string
	// Destinations is every destination the account owns, in the order they
	// were connected.
	Destinations    []*store.Destination
	MaxDestinations int
	Kinds           []destination.Kind
	// Activity is delivery history folded into one line per entry.
	Activity []activityEntry
	// ActivityMore says history was cut off, so the list can admit it rather
	// than looking like everything there is.
	ActivityMore bool
	Pending      int
	Interval     string
}

// CanAddFeed reports whether the account has a feed slot left.
func (d dashboardData) CanAddFeed() bool { return len(d.Feeds) < d.MaxFeeds }

// CanAddDestination reports whether the account has a destination slot left.
func (d dashboardData) CanAddDestination() bool {
	return len(d.Destinations) < d.MaxDestinations
}

// The activity list is capped in entries, not deliveries.
//
// One entry going to seven destinations is seven rows in the table and one line
// here. Counting rows meant a well-connected account watching a busy feed saw a
// list that was four entries deep and twenty-five lines long: every service
// repeated under every title, and yesterday already off the bottom. Counting
// entries makes the length of the list a number of things that happened.
//
// activityRows is what is read to build them — enough for the cap even when
// every entry went to every destination the account can hold, with room for
// the trailing group that gets dropped below.
const (
	activityEntries = 10
	activityRows    = activityEntries*store.MaxDestinationsPerAccount + 1
)

// activityEntry is one entry and every delivery of it.
type activityEntry struct {
	ItemID    int64
	Title     string
	URL       string
	FeedTitle string
	// UpdatedAt is the most recent attempt in the group, which is what "2h ago"
	// on the line should mean when a retry has moved one of them.
	UpdatedAt  time.Time
	Deliveries []*store.DeliveryView
}

// Status reports the state of the entry as a whole, for the pip on the line:
// failed if anything failed, otherwise pending if anything is still queued.
func (e activityEntry) Status() string {
	status := "sent"
	for _, d := range e.Deliveries {
		switch d.Status {
		case "failed":
			return "failed"
		case "pending":
			status = "pending"
		}
	}
	return status
}

// groupActivity folds delivery rows, newest first, into one entry each.
//
// full says the read hit its limit, which means the oldest group in it may have
// been cut in half — some of its deliveries are in the rows that were not read.
// Rendering that group would show an entry as posted to two destinations when
// it went to five, so it is dropped rather than shown wrong.
func groupActivity(rows []*store.DeliveryView, full bool) ([]activityEntry, bool) {
	entries := make([]activityEntry, 0, activityEntries)
	at := make(map[int64]int, activityEntries)
	for _, d := range rows {
		i, ok := at[d.ItemID]
		if !ok {
			at[d.ItemID] = len(entries)
			entries = append(entries, activityEntry{
				ItemID: d.ItemID, Title: d.ItemTitle, URL: d.ItemURL,
				FeedTitle: d.FeedTitle, UpdatedAt: d.UpdatedAt,
			})
			i = len(entries) - 1
		}
		e := &entries[i]
		e.Deliveries = append(e.Deliveries, d)
		if d.UpdatedAt.After(e.UpdatedAt) {
			e.UpdatedAt = d.UpdatedAt
		}
	}
	if full && len(entries) > 0 {
		entries = entries[:len(entries)-1]
	}
	if len(entries) > activityEntries {
		return entries[:activityEntries], true
	}
	return entries, full
}

// recentActivity reads the history and groups it.
func (s *Server) recentActivity(ctx context.Context, userID int64) ([]activityEntry, bool, error) {
	rows, err := s.store.RecentDeliveries(ctx, userID, activityRows)
	if err != nil {
		return nil, false, err
	}
	entries, more := groupActivity(rows, len(rows) == activityRows)
	return entries, more, nil
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	s.renderDashboard(w, r, http.StatusOK, "", "")
}

// renderDashboard draws the dashboard with whatever is on the account. It is
// the one entry point for the page, so a rejected form and a plain visit
// cannot drift apart in what they load.
func (s *Server) renderDashboard(w http.ResponseWriter, r *http.Request, status int, errMsg, feedURL string) {
	user := userFrom(r)
	data := dashboardData{
		MaxFeeds:        store.MaxFeedsPerAccount,
		MaxDestinations: store.MaxDestinationsPerAccount,
		Kinds:           destination.Kinds,
		Interval:        humanInterval(s.cfg.PollInterval),
		FeedURL:         feedURL,
	}
	var err error
	if data.Feeds, err = s.store.FeedsByUser(r.Context(), user.ID); err != nil {
		s.log.Error("load feeds", "error", err)
	}
	if data.Destinations, err = s.store.DestinationsByUser(r.Context(), user.ID); err != nil {
		s.log.Error("load destinations", "error", err)
	}
	if data.Activity, data.ActivityMore, err = s.recentActivity(r.Context(), user.ID); err != nil {
		s.log.Error("load deliveries", "error", err)
	}
	if data.Pending, err = s.store.PendingCount(r.Context(), user.ID); err != nil {
		s.log.Error("count pending", "error", err)
	}
	s.render(w, r, status, "dashboard", page{Title: "feedrepeater", Error: errMsg, Data: data})
}

func (s *Server) dashboardError(w http.ResponseWriter, r *http.Request, msg, feedURL string) {
	s.renderDashboard(w, r, http.StatusBadRequest, msg, feedURL)
}

// --- feeds -----------------------------------------------------------------

func (s *Server) handleFeedSave(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	raw := r.PostFormValue("url")

	// The count is for the message. Subscribe holds the rule itself, inside its
	// transaction, so two tabs adding a fifth feed at once still end with five.
	if feeds, err := s.store.FeedsByUser(r.Context(), user.ID); err != nil {
		s.log.Error("list feeds", "error", err)
		s.fail(w, r, http.StatusInternalServerError, "Something went wrong.")
		return
	} else if len(feeds) >= store.MaxFeedsPerAccount {
		s.dashboardError(w, r, feedLimitMessage, "")
		return
	}

	u, err := s.http.ParseURL(raw)
	if err != nil {
		s.dashboardError(w, r, "That address cannot be used. Enter a public http or https feed URL.", raw)
		return
	}

	if !s.writeLimit.allow("feed:" + strconv.FormatInt(user.ID, 10)) {
		s.dashboardError(w, r, "Too many changes. Try again later.", raw)
		return
	}

	// Fetch once now so an address that is not a feed is rejected while the
	// person is still looking at the form. A site address is accepted too: what
	// comes back is the feed that page links to, which is what gets stored.
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	feedURL, res, err := s.fetcher.Resolve(ctx, u.String())
	if err != nil {
		s.log.Info("validate feed", "user", user.ID, "error", err)
		s.dashboardError(w, r, "That address could not be read as a feed, and the page there did not link to one.", raw)
		return
	}
	discovered := feedURL != u.String()

	// The address is always fetched, including when this service already follows
	// it for somebody else.
	//
	// Skipping the fetch for a known feed was free and quick, and that was the
	// problem: whether a fetch happened was observable from the response, so
	// submitting addresses and watching which ones came back instantly
	// enumerated the set of feeds this service's users read. Feed contents are
	// public; who reads them here was not, and on a service small enough to
	// publish its own account count that narrows towards individuals.
	//
	// What it costs to close is one request, once, when somebody adds a feed.
	// The saving that matters is not this — it is that the feed is then polled
	// once for everyone who follows it, ninety-six times a day rather than
	// ninety-six times each, and that is untouched. It also removes a race
	// worth not having: a lookup that said "known" could be stale by the time
	// the insert ran, if the last other subscriber left in between.
	//
	// Subscribe handles either case; the seed and validators below are used
	// only if this really is the first subscriber, so there is nothing to check
	// for here.
	//
	// The entries seeded are recorded as seen and delivered nowhere: posting
	// starts from what appears after this moment, so adding a feed never floods
	// a timeline. Doing it in the same transaction as the subscription means
	// there is no window in which an existing entry could be mistaken for a new
	// one.
	seed := make([]store.Item, 0, len(res.Entries))
	for _, e := range res.Entries {
		seed = append(seed, store.Item{
			GUID: e.GUID, URL: e.URL, Title: e.Title,
			Summary: e.Summary, Author: e.Author, PublishedAt: e.Published,
		})
	}
	// The validators from this fetch go with it, so the worker's first poll is a
	// conditional request rather than a second full download of what was just
	// read.
	now := time.Now().UTC()
	state := store.FetchState{
		ETag:         res.ETag,
		LastModified: res.LastModified,
		BodyHash:     res.BodyHash,
		NextFetchAt:  now.Add(s.cfg.PollInterval),
	}

	// The account's own Mastodon destination is made at sign-in and repaired
	// here if that never worked, before Subscribe, because Subscribe attaches a
	// new feed to the destinations that already exist — after it, there would
	// be a window in which the feed was polled and published nowhere. Almost
	// always this finds the destination made at sign-in and does nothing.
	//
	// Failing only matters when it leaves the feed with nowhere to go. An
	// account that holds other destinations — ten webhooks, say, which is the
	// one case the repair cannot fit another row into — still gets its feed.
	if _, _, err := s.ensureMastodonDestination(ctx, user, ""); err != nil {
		s.log.Error("connect mastodon destination", "user", user.ID, "error", err)
		if list, listErr := s.store.DestinationsByUser(r.Context(), user.ID); listErr != nil || len(list) == 0 {
			s.dashboardError(w, r, "Your feed was not added: posting to your Mastodon account could not be set up. Sign out, sign back in, and try again.", raw)
			return
		}
	}

	if _, _, err := s.store.Subscribe(r.Context(), user.ID, feedURL, res.Title, seed, state); err != nil {
		switch {
		case errors.Is(err, store.ErrFeedLimit):
			s.dashboardError(w, r, feedLimitMessage, "")
		case errors.Is(err, store.ErrAlreadySubscribed):
			s.dashboardError(w, r, "You already follow that feed.", "")
		default:
			s.log.Error("save feed", "error", err)
			s.fail(w, r, http.StatusInternalServerError, "Something went wrong.")
		}
		return
	}

	if discovered {
		redirect(w, r, "/dashboard", "feed-found")
		return
	}
	redirect(w, r, "/dashboard", "feed-saved")
}

var feedLimitMessage = fmt.Sprintf("You already follow %d feeds, which is the limit. Remove one before adding another.", store.MaxFeedsPerAccount)

// feedData is what a feed's own page renders: the feed, and every destination
// the account owns marked with whether this feed reaches it.
type feedData struct {
	Feed         *store.FeedView
	Destinations []*store.RoutedDestination
	Interval     string
}

// RoutedCount reports how many live destinations the feed publishes to.
func (d feedData) RoutedCount() int {
	n := 0
	for _, dest := range d.Destinations {
		if dest.Routed && !dest.Paused {
			n++
		}
	}
	return n
}

func (s *Server) handleFeed(w http.ResponseWriter, r *http.Request) {
	f := s.feedFromPath(w, r)
	if f == nil {
		return
	}
	dests, err := s.store.DestinationsForFeed(r.Context(), f.UserID, f.ID)
	if err != nil {
		s.log.Error("load destinations for feed", "feed", f.ID, "error", err)
	}
	title := f.Title
	if title == "" {
		title = hostOf(f.URL)
	}
	s.render(w, r, http.StatusOK, "feed", page{Title: title, Data: feedData{
		Feed: f, Destinations: dests, Interval: humanInterval(s.cfg.PollInterval),
	}})
}

// handleFeedRoutes saves which destinations one feed publishes to. Submitted
// ids are filtered against the account's own destinations in the store, so an
// id belonging to someone else is simply not attached.
func (s *Server) handleFeedRoutes(w http.ResponseWriter, r *http.Request) {
	f := s.feedFromPath(w, r)
	if f == nil {
		return
	}

	var ids []int64
	for _, raw := range r.PostForm["destination"] {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			continue
		}
		ids = append(ids, id)
	}
	if len(ids) > 100 {
		ids = ids[:100]
	}

	if err := s.store.SetFeedRoutes(r.Context(), f.UserID, f.ID, ids); err != nil {
		s.log.Error("save routes", "feed", f.ID, "error", err)
		s.fail(w, r, http.StatusInternalServerError, "Something went wrong.")
		return
	}
	redirect(w, r, feedPath(f.ID), "routes-saved")
}

func (s *Server) handleFeedPause(w http.ResponseWriter, r *http.Request) {
	f := s.feedFromPath(w, r)
	if f == nil {
		return
	}
	pause := r.PostFormValue("paused") == "1"
	if err := s.store.SetSubscriptionPaused(r.Context(), f.UserID, f.ID, pause); err != nil {
		s.log.Error("pause feed", "feed", f.ID, "error", err)
	}
	if pause {
		redirect(w, r, returnPath(r, f), "feed-paused")
		return
	}
	redirect(w, r, returnPath(r, f), "feed-resumed")
}

func (s *Server) handleFeedRefresh(w http.ResponseWriter, r *http.Request) {
	f := s.feedFromPath(w, r)
	if f == nil {
		return
	}
	if !s.writeLimit.allow("refresh:" + strconv.FormatInt(f.UserID, 10)) {
		s.dashboardError(w, r, "Too many refreshes. Try again later.", "")
		return
	}
	if err := s.store.FetchNow(r.Context(), f.UserID, f.ID); err != nil {
		s.log.Error("schedule fetch", "feed", f.ID, "error", err)
	}
	redirect(w, r, returnPath(r, f), "refreshing")
}

func (s *Server) handleFeedDelete(w http.ResponseWriter, r *http.Request) {
	f := s.feedFromPath(w, r)
	if f == nil {
		return
	}
	// Unsubscribing collects orphaned feeds, so it is as much of a write as
	// adding one. Limiting only the add left save/delete as an unbounded loop.
	if !s.writeLimit.allow("feed:" + strconv.FormatInt(f.UserID, 10)) {
		s.dashboardError(w, r, "Too many changes. Try again later.", "")
		return
	}
	if err := s.store.Unsubscribe(r.Context(), f.UserID, f.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.log.Error("delete feed", "feed", f.ID, "error", err)
	}
	redirect(w, r, "/dashboard", "feed-removed")
}

// returnPath is where a feed action sends the browser back to: the feed's own
// page when the form said it came from there, the dashboard otherwise. The
// value is a flag rather than a path, so nothing in the request chooses where
// a redirect goes.
func returnPath(r *http.Request, f *store.FeedView) string {
	if r.PostFormValue("return") == "feed" {
		return feedPath(f.ID)
	}
	return "/dashboard"
}

func feedPath(id int64) string { return "/feeds/" + strconv.FormatInt(id, 10) }

// feedFromPath loads the feed named in the path, scoped to the signed-in
// user's subscription, and writes the response itself if it cannot. A feed the
// account does not follow is reported the same way as one that does not exist.
func (s *Server) feedFromPath(w http.ResponseWriter, r *http.Request) *store.FeedView {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.fail(w, r, http.StatusNotFound, "That feed does not exist.")
		return nil
	}
	f, err := s.store.FeedByUser(r.Context(), userFrom(r).ID, id)
	if err != nil {
		s.fail(w, r, http.StatusNotFound, "That feed does not exist.")
		return nil
	}
	return f
}

// --- destinations ----------------------------------------------------------

type destinationForm struct {
	Kind destination.Kind
	Dest *store.Destination
	// Own marks the destination that posts as the signed-in account, which can
	// be paused but not removed.
	Own      bool
	Label    string
	Template string
	// Instance is the Mastodon server a further account is connected from.
	Instance     string
	Visibility   string
	Handle       string
	URL          string
	Server       string
	Topic        string
	Priority     int
	Tags         string
	Unread       bool
	Account      string
	Paused       bool
	NewSecret    string
	Variables    []render.Variable
	Visibilities []string
	Priorities   []destination.NtfyPriority
	Preview      string
	Limit        int
}

func (s *Server) handleDestinationNew(w http.ResponseWriter, r *http.Request) {
	kind, ok := destination.KindByName(r.URL.Query().Get("kind"))
	if !ok {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	user := userFrom(r)
	form := destinationForm{
		Kind:  kind,
		Label: defaultLabel(kind.Name, user),
		// The account's own default wins over the kind's, which is the whole
		// point of setting one; a kind that wants something different still gets
		// it when the account has expressed no preference.
		Template:     nonEmpty(user.DefaultTemplate, kind.DefaultTemplate),
		Account:      user.Handle(),
		Variables:    render.Variables,
		Visibilities: destination.Visibilities,
		Visibility:   "public",
		Priorities:   destination.NtfyPriorities,
		Priority:     destination.DefaultNtfyPriority,
	}
	switch kind.Name {
	case destination.KindNtfy:
		// The hosted service is where most people's phone is subscribed. Only ntfy
		// has a default address: a linkding is somebody's own server and nobody
		// else's, so that field starts empty.
		form.Server = destination.DefaultNtfyServer
	case destination.KindLinkding:
		// A feed arriving as a bookmark is a reading list, so it starts unread.
		// linkding's own default is read, which for entries nobody has seen yet
		// would be a claim rather than a state.
		form.Unread = true
	}
	s.render(w, r, http.StatusOK, "destination_new", page{
		Title: "Add destination",
		Data:  form,
		// Connecting a Mastodon account hands the browser to an instance, the
		// same way signing in does, so the form needs the same room.
		Handoff: kind.Name == destination.KindMastodon,
	})
}

func (s *Server) handleDestinationCreate(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	kind, ok := destination.KindByName(r.PostFormValue("kind"))
	if !ok {
		s.fail(w, r, http.StatusBadRequest, "Unknown destination type.")
		return
	}
	// The count is for the message. CreateDestination holds the rule itself,
	// since between here and the insert sits a round trip to the service. It
	// runs before the rate limiter so a refused add does not spend write
	// budget it never uses.
	if list, err := s.store.DestinationsByUser(r.Context(), user.ID); err != nil {
		s.log.Error("list destinations", "error", err)
		s.fail(w, r, http.StatusInternalServerError, "Something went wrong.")
		return
	} else if len(list) >= store.MaxDestinationsPerAccount {
		s.fail(w, r, http.StatusBadRequest, destinationLimitMessage)
		return
	}
	if !s.writeLimit.allow("dest:" + strconv.FormatInt(user.ID, 10)) {
		s.fail(w, r, http.StatusTooManyRequests, "Too many changes. Try again later.")
		return
	}

	form := destinationForm{
		Kind:         kind,
		Label:        strings.TrimSpace(r.PostFormValue("label")),
		Template:     r.PostFormValue("template"),
		Instance:     strings.TrimSpace(r.PostFormValue("instance")),
		Visibility:   r.PostFormValue("visibility"),
		Handle:       strings.TrimSpace(r.PostFormValue("handle")),
		URL:          strings.TrimSpace(r.PostFormValue("url")),
		Server:       strings.TrimSpace(r.PostFormValue("server")),
		Topic:        strings.TrimSpace(r.PostFormValue("topic")),
		Priority:     formPriority(r),
		Tags:         strings.TrimSpace(r.PostFormValue("tags")),
		Unread:       r.PostFormValue("unread") == "1",
		Account:      user.Handle(),
		Variables:    render.Variables,
		Visibilities: destination.Visibilities,
		Priorities:   destination.NtfyPriorities,
	}
	if form.Label == "" {
		form.Label = defaultLabel(kind.Name, user)
	}

	// A Mastodon destination is an account authorised at its own instance, so
	// adding one is an OAuth flow rather than a form: nothing is stored until
	// the instance has handed back a token for it. The label and template are
	// decided when the flow completes, from the account it turns out to be.
	if kind.Name == destination.KindMastodon {
		s.startConnect(w, r, user, form)
		return
	}

	if err := render.ValidateTemplate(form.Template); err != nil {
		s.destinationFormError(w, r, "destination_new", form, err.Error())
		return
	}

	d := &store.Destination{
		UserID:   user.ID,
		Kind:     kind.Name,
		Label:    clipLabel(form.Label),
		Template: form.Template,
	}

	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	var newSecret string
	var err error
	switch kind.Name {
	case destination.KindBluesky:
		err = s.configureBluesky(ctx, d, form.Handle, r.PostFormValue("app_password"))
	case destination.KindDiscord:
		err = s.configureDiscord(ctx, d, form.URL)
	case destination.KindSlack:
		err = s.configureSlack(ctx, d, form.URL)
	case destination.KindNtfy:
		err = s.configureNtfy(ctx, d, form.Server, form.Topic, r.PostFormValue("token"), form.Priority, true)
	case destination.KindLinkding:
		err = s.configureLinkding(ctx, d, form.Server, r.PostFormValue("token"), form.Tags, form.Unread, true)
	case destination.KindWebhook:
		newSecret, err = s.configureWebhook(ctx, d, form.URL)
	}
	if err != nil {
		s.destinationFormError(w, r, "destination_new", form, err.Error())
		return
	}

	if err := s.store.CreateDestination(r.Context(), d); err != nil {
		// A race for the last slot, and this one lost. The credentials it just
		// verified are not stored, so nothing is left behind to clean up.
		if errors.Is(err, store.ErrDestinationLimit) {
			s.fail(w, r, http.StatusBadRequest, destinationLimitMessage)
			return
		}
		s.log.Error("create destination", "error", err)
		s.fail(w, r, http.StatusInternalServerError, "Something went wrong.")
		return
	}

	if newSecret == "" {
		redirect(w, r, "/destinations/"+strconv.FormatInt(d.ID, 10), "dest-added")
		return
	}
	// The signing secret is shown once, on this response, and never again.
	s.renderDestinationEdit(w, r, d, newSecret, "")
}

var destinationLimitMessage = fmt.Sprintf("You already have %d destinations, which is the limit. Remove one before adding another.", store.MaxDestinationsPerAccount)

// startConnect begins the OAuth flow that adds a further Mastodon account as a
// destination. The instance named is checked the same way sign-in checks it,
// and the flow is recorded as a connect for this account, so the callback can
// insist that this account is the one that finishes it.
func (s *Server) startConnect(w http.ResponseWriter, r *http.Request, user *store.User, form destinationForm) {
	host, err := mastodon.NormalizeHost(form.Instance)
	if err != nil {
		s.destinationFormError(w, r, "destination_new", form, err.Error())
		return
	}
	if s.cfg.InstanceBlocked(host) {
		s.log.Info("connect to blocked instance", "user", user.ID, "host", host)
		s.destinationFormError(w, r, "destination_new", form, "Accounts on that server cannot be connected.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	to, err := s.beginOAuth(ctx, w, store.OAuthState{Host: host, Purpose: store.OAuthConnect, UserID: user.ID})
	if err != nil {
		if errors.Is(err, errInstanceUnreachable) {
			s.log.Info("register app", "host", host, "error", err)
			s.destinationFormError(w, r, "destination_new", form, "Could not reach that instance. Check the address and try again.")
			return
		}
		s.log.Error("start connect", "host", host, "error", err)
		s.fail(w, r, http.StatusInternalServerError, "Something went wrong.")
		return
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func (s *Server) handleDestinationEdit(w http.ResponseWriter, r *http.Request) {
	d := s.destinationFromPath(w, r)
	if d == nil {
		return
	}
	s.renderDestinationEdit(w, r, d, "", "")
}

func (s *Server) handleDestinationUpdate(w http.ResponseWriter, r *http.Request) {
	d := s.destinationFromPath(w, r)
	if d == nil {
		return
	}
	user := userFrom(r)

	// Updating can re-resolve a Bluesky handle and open a session against a
	// server the user named, so it reaches third parties just as creating does
	// and needs the same limit.
	if !s.writeLimit.allow("dest:" + strconv.FormatInt(user.ID, 10)) {
		s.fail(w, r, http.StatusTooManyRequests, "Too many changes. Try again later.")
		return
	}

	label := clipLabel(strings.TrimSpace(r.PostFormValue("label")))
	if label == "" {
		label = d.Label
	}
	tmpl := r.PostFormValue("template")
	if err := render.ValidateTemplate(tmpl); err != nil {
		d.Label, d.Template = label, tmpl
		s.renderDestinationEdit(w, r, d, "", err.Error())
		return
	}
	d.Label = label
	d.Template = tmpl
	d.Paused = r.PostFormValue("paused") == "1"

	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	switch d.Kind {
	case destination.KindMastodon:
		if v := r.PostFormValue("visibility"); destination.ValidVisibility(v) {
			cfg, _ := decodeConfig[destination.MastodonConfig](d.Config)
			cfg.Visibility = v
			if err := s.setConfig(d, cfg); err != nil {
				s.fail(w, r, http.StatusInternalServerError, "Something went wrong.")
				return
			}
		}
	case destination.KindBluesky:
		// An app password is only replaced when a new one is supplied.
		if pass := r.PostFormValue("app_password"); strings.TrimSpace(pass) != "" {
			cfg, _ := decodeConfig[destination.BlueskyConfig](d.Config)
			if err := s.configureBluesky(ctx, d, cfg.Handle, pass); err != nil {
				s.renderDestinationEdit(w, r, d, "", err.Error())
				return
			}
		}
	case destination.KindDiscord:
		// As with a Bluesky app password, the URL is only replaced when a new
		// one is given, and the new one is verified before it is stored.
		if u := strings.TrimSpace(r.PostFormValue("url")); u != "" {
			if err := s.configureDiscord(ctx, d, u); err != nil {
				s.renderDestinationEdit(w, r, d, "", err.Error())
				return
			}
		}
	case destination.KindSlack:
		if u := strings.TrimSpace(r.PostFormValue("url")); u != "" {
			if err := s.configureSlack(ctx, d, u); err != nil {
				s.renderDestinationEdit(w, r, d, "", err.Error())
				return
			}
		}
	case destination.KindNtfy:
		// Server, topic and priority are not secrets, so the form carries them
		// whole and they are applied as given. The token is, so an empty field
		// means "keep the stored one" unless the box asking to drop it is ticked.
		prev, _ := decodeConfig[destination.NtfyConfig](d.Config)
		stored := s.storedNtfyToken(d)
		token := strings.TrimSpace(r.PostFormValue("token"))
		if token == "" && r.PostFormValue("clear_token") != "1" {
			token = stored
		}
		server := strings.TrimSpace(r.PostFormValue("server"))
		topic := strings.TrimSpace(r.PostFormValue("topic"))

		// Where a post goes has to prove itself again when it moves; a template
		// or priority edit does not, because verification publishes a
		// notification and nobody wants one for changing a word. The server is
		// compared normalised, so "ntfy.sh" and "https://ntfy.sh/" are the same
		// address rather than a change.
		normalised := ""
		if u, err := destination.ParseNtfyServer(server); err == nil {
			normalised = u.String()
		}
		moved := normalised != prev.Server || topic != prev.Topic || token != stored

		if err := s.configureNtfy(ctx, d, server, topic, token, formPriority(r), moved); err != nil {
			s.renderDestinationEdit(w, r, d, "", err.Error())
			return
		}
	case destination.KindLinkding:
		// The address, the tags and the unread flag are not secrets, so the form
		// carries them whole. The token is, so an empty field means "keep the
		// stored one" — and unlike ntfy's there is no way to remove it, because a
		// linkding destination without a token cannot write anything.
		prev, _ := decodeConfig[destination.LinkdingConfig](d.Config)
		stored := s.storedLinkdingToken(d)
		token := strings.TrimSpace(r.PostFormValue("token"))
		if token == "" {
			token = stored
		}
		server := strings.TrimSpace(r.PostFormValue("server"))

		// Where a bookmark goes proves itself again when it moves. Compared
		// normalised, so "linkding.example" and "https://linkding.example/" are the
		// same address rather than a change.
		normalised := ""
		if u, err := destination.ParseLinkdingServer(server); err == nil {
			normalised = u.String()
		}
		moved := normalised != prev.Server || token != stored

		if err := s.configureLinkding(ctx, d, server, token,
			strings.TrimSpace(r.PostFormValue("tags")), r.PostFormValue("unread") == "1", moved); err != nil {
			s.renderDestinationEdit(w, r, d, "", err.Error())
			return
		}
	case destination.KindWebhook:
		if u := strings.TrimSpace(r.PostFormValue("url")); u != "" {
			cfg, _ := decodeConfig[destination.WebhookConfig](d.Config)
			if u != cfg.URL {
				// A new address is a new endpoint and has to prove itself the
				// same way the original did. It is verified against a fresh
				// secret, which is then the one that stays.
				secretValue, err := s.configureWebhook(ctx, d, u)
				if err != nil {
					s.renderDestinationEdit(w, r, d, "", err.Error())
					return
				}
				if err := s.store.UpdateDestination(r.Context(), d); err != nil {
					s.log.Error("update destination", "error", err)
					s.fail(w, r, http.StatusInternalServerError, "Something went wrong.")
					return
				}
				s.renderDestinationEdit(w, r, d, secretValue, "")
				return
			}
		}
		if r.PostFormValue("rotate") == "1" {
			secretValue, err := s.newWebhookSecret(d)
			if err != nil {
				s.fail(w, r, http.StatusInternalServerError, "Something went wrong.")
				return
			}
			if err := s.store.UpdateDestination(r.Context(), d); err != nil {
				s.log.Error("update destination", "error", err)
				s.fail(w, r, http.StatusInternalServerError, "Something went wrong.")
				return
			}
			s.renderDestinationEdit(w, r, d, secretValue, "")
			return
		}
	}

	if err := s.store.UpdateDestination(r.Context(), d); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.fail(w, r, http.StatusNotFound, "That destination no longer exists.")
			return
		}
		s.log.Error("update destination", "user", user.ID, "error", err)
		s.fail(w, r, http.StatusInternalServerError, "Something went wrong.")
		return
	}
	redirect(w, r, "/destinations/"+strconv.FormatInt(d.ID, 10), "dest-saved")
}

func (s *Server) handleDestinationDelete(w http.ResponseWriter, r *http.Request) {
	d := s.destinationFromPath(w, r)
	if d == nil {
		return
	}
	user := userFrom(r)
	if !s.writeLimit.allow("dest:" + strconv.FormatInt(user.ID, 10)) {
		s.fail(w, r, http.StatusTooManyRequests, "Too many changes. Try again later.")
		return
	}
	// The destination that posts as the signed-in account cannot be removed,
	// because every sign-in and every feed add would put it back, routed to
	// every feed — content the person had stopped posting would resume on
	// their own timeline with nobody asking. Pausing it is the way to stop it.
	if isOwnDestination(user, d) {
		s.renderDestinationEdit(w, r, d, "", "This is the account you signed in with, so it stays. Pause it to stop posting there.")
		return
	}
	// A connected Mastodon account's token is handed back before the record of
	// it goes, so removing the destination leaves this service unable to post
	// as that account.
	s.revokeDestinationToken(r.Context(), user, d, false)
	if err := s.store.DeleteDestination(r.Context(), d.UserID, d.ID); err != nil {
		s.log.Error("delete destination", "error", err)
	}
	redirect(w, r, "/dashboard", "dest-removed")
}

func (s *Server) handleDestinationTest(w http.ResponseWriter, r *http.Request) {
	d := s.destinationFromPath(w, r)
	if d == nil {
		return
	}
	user := userFrom(r)
	if !s.writeLimit.allow("test:" + strconv.FormatInt(user.ID, 10)) {
		s.renderDestinationEdit(w, r, d, "", "Too many test posts. Try again later.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	item := s.sampleItem(ctx, user)
	if _, err := s.pub.Send(ctx, d, item, user.Location(), "test-"+strconv.FormatInt(time.Now().UnixNano(), 36)); err != nil {
		s.log.Info("test post failed", "destination", d.ID, "error", err)
		s.renderDestinationEdit(w, r, d, "", "Test post failed: "+clip(err.Error(), 300))
		return
	}
	redirect(w, r, "/destinations/"+strconv.FormatInt(d.ID, 10), "test-sent")
}

// --- deliveries ------------------------------------------------------------

// handleDeliveryRetry puts a failed delivery back in the queue. The worker
// picks it up on its next pass rather than sending it here: a button that waits
// on a third party is a button that times out.
func (s *Server) handleDeliveryRetry(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.fail(w, r, http.StatusNotFound, "That delivery does not exist.")
		return
	}
	// Requeuing reaches a third party once the worker gets to it, so it is
	// limited like anything else that does.
	if !s.writeLimit.allow("retry:" + strconv.FormatInt(user.ID, 10)) {
		s.fail(w, r, http.StatusTooManyRequests, "Too many retries. Try again later.")
		return
	}
	if err := s.store.RequeueDelivery(r.Context(), user.ID, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Someone else's delivery, or one that is not failed, are the same
			// answer: there is nothing here to retry.
			s.fail(w, r, http.StatusNotFound, "That delivery cannot be retried.")
			return
		}
		s.log.Error("requeue delivery", "delivery", id, "error", err)
		s.fail(w, r, http.StatusInternalServerError, "Something went wrong.")
		return
	}
	// The sender idles for minutes at a time now, so tell it rather than
	// letting somebody watch a spinner until the next tick.
	s.notify()
	redirect(w, r, "/dashboard", "retrying")
}

// --- settings --------------------------------------------------------------

// settingsData is what the settings page renders. Timezone and DefaultTemplate
// come from the form on a rejected submission and from the account otherwise, so
// a bad value is shown back rather than discarded.
type settingsData struct {
	Timezone        string
	Zones           []string
	DefaultTemplate string
	Variables       []render.Variable
	Preview         string
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	s.renderSettings(w, r, http.StatusOK, "", user.Timezone, user.DefaultTemplate)
}

// renderSettings draws the page with whatever values should appear in the form.
func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, status int, errMsg, timezone, template string) {
	s.render(w, r, status, "settings", page{
		Title: "Settings",
		Error: errMsg,
		Data: settingsData{
			Timezone:        timezone,
			Zones:           zoneOptions(timezone),
			DefaultTemplate: template,
			Variables:       render.Variables,
			// Previewed at no limit: this text is the starting point for any
			// destination, and each one trims to its own service's length.
			Preview: s.previewTemplate(r, template, 0),
		},
	})
}

// handleSettingsSave stores the account-level preferences.
func (s *Server) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	rawZone := strings.TrimSpace(r.PostFormValue("timezone"))
	tmpl := strings.TrimSpace(r.PostFormValue("default_template"))

	zone, err := parseTimezone(rawZone)
	if err != nil {
		s.renderSettings(w, r, http.StatusBadRequest, err.Error(), rawZone, tmpl)
		return
	}
	// Empty is allowed here and means the built-in template, unlike a
	// destination's own text, which has to say something.
	if tmpl != "" {
		if err := render.ValidateTemplate(tmpl); err != nil {
			s.renderSettings(w, r, http.StatusBadRequest, err.Error(), rawZone, tmpl)
			return
		}
	}

	if err := s.store.SetUserPreferences(r.Context(), user.ID, zone, tmpl); err != nil {
		s.log.Error("save settings", "user", user.ID, "error", err)
		s.fail(w, r, http.StatusInternalServerError, "Something went wrong.")
		return
	}
	// After the write, not before it. Dropping the entry first leaves a window
	// in which a concurrent request on the same session re-reads the old row and
	// caches it for another full TTL — so the page that renders next shows the
	// value that was just replaced, which is exactly what this is here to stop.
	s.forgetSession(sessionIDFrom(r))
	redirect(w, r, "/settings", "settings-saved")
}

// parseTimezone validates an IANA zone name, returning what should be stored.
//
// Empty is stored for UTC rather than the string "UTC", so the column's default
// and an account that cleared the field mean the same thing. LoadLocation is the
// check because it is also what reads the value back: a name it accepts here
// cannot fail there.
func parseTimezone(name string) (string, error) {
	if name == "" {
		return "", nil
	}
	if len(name) > 64 {
		return "", fmt.Errorf("that is not a timezone name")
	}
	// "Local" resolves to the server's own zone, which is a fact about this
	// machine and not about the account. It would also silently change meaning
	// if the service moved.
	if strings.EqualFold(name, "local") {
		return "", fmt.Errorf("name a zone such as Europe/Amsterdam rather than Local")
	}
	if _, err := time.LoadLocation(name); err != nil {
		return "", fmt.Errorf("%q is not a timezone name; use one like Europe/Amsterdam or UTC", clip(name, 40))
	}
	return name, nil
}

// commonZones are the zones the timezone dropdown offers. parseTimezone still
// accepts any name in the IANA database, so this is what the form can express
// rather than what the account may hold — see zoneOptions.
var commonZones = []string{
	"UTC",
	"Europe/Amsterdam", "Europe/Berlin", "Europe/Brussels", "Europe/Dublin",
	"Europe/Lisbon", "Europe/London", "Europe/Madrid", "Europe/Moscow",
	"Europe/Paris", "Europe/Rome", "Europe/Stockholm", "Europe/Warsaw",
	"America/Argentina/Buenos_Aires", "America/Bogota", "America/Chicago",
	"America/Denver", "America/Los_Angeles", "America/Mexico_City",
	"America/New_York", "America/Sao_Paulo", "America/Toronto", "America/Vancouver",
	"Africa/Cairo", "Africa/Johannesburg", "Africa/Lagos", "Africa/Nairobi",
	"Asia/Dubai", "Asia/Hong_Kong", "Asia/Jakarta", "Asia/Jerusalem",
	"Asia/Kolkata", "Asia/Seoul", "Asia/Shanghai", "Asia/Singapore", "Asia/Tokyo",
	"Australia/Melbourne", "Australia/Perth", "Australia/Sydney",
	"Pacific/Auckland", "Pacific/Honolulu",
}

// zoneOptions is the list the dropdown renders, with the account's own zone
// guaranteed to be in it.
//
// The field was a text box accepting any IANA name before it was a select, so an
// account can hold a zone this list does not carry. A select that omits it has no
// option to mark selected, the browser falls back to the first — UTC — and the
// next save of an unrelated preference silently moves that account's dates. So
// the current value is carried along rather than being a value the form cannot
// express. Empty needs no entry: it means UTC, which is the first option anyway.
func zoneOptions(current string) []string {
	if current == "" || slices.Contains(commonZones, current) {
		return commonZones
	}
	return slices.Concat(commonZones, []string{current})
}

func (s *Server) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	if r.PostFormValue("confirm") != user.Acct {
		s.renderSettings(w, r, http.StatusBadRequest,
			"Type your username exactly to confirm.", user.Timezone, user.DefaultTemplate)
		return
	}
	// Hand every token back before the record of them is gone: the account's
	// own, and those of any further Mastodon accounts it connected. Deleting
	// the account here should not leave this service still able to post as
	// any of them.
	//
	// The account is re-read first. The one on the request is up to thirty
	// seconds old, and a sign-in from another browser in that window has
	// replaced its token; revoking the copy here would retire a token the
	// instance already forgot and leave the live one standing.
	if fresh, err := s.store.UserByID(r.Context(), user.ID); err == nil {
		user = fresh
	}
	s.revokeUserToken(r.Context(), user)
	if list, err := s.store.DestinationsByUser(r.Context(), user.ID); err == nil {
		for _, d := range list {
			s.revokeDestinationToken(r.Context(), user, d, true)
		}
	}

	if err := s.store.DeleteUser(r.Context(), user.ID); err != nil {
		s.log.Error("delete account", "error", err)
		s.fail(w, r, http.StatusInternalServerError, "Something went wrong.")
		return
	}
	// Every session, not just this one: the account may be signed in elsewhere,
	// and those entries would otherwise resolve to a deleted user for a further
	// TTL. The database rows are already gone by cascade; this is the cache
	// catching up.
	s.forgetUser(user.ID)
	s.endSession(w, r)
	redirect(w, r, "/", "account-gone")
}

// --- information pages -----------------------------------------------------

// infoData carries the few live numbers the FAQ quotes, so a deployment that
// polls on a different schedule does not have a page contradicting it.
type infoData struct {
	Interval        string
	MaxItems        int
	MaxFeeds        int
	MaxDestinations int
	Kinds           []destination.Kind
	Updated         string
	// Contact and Jurisdiction are the operator's own, and empty on a
	// deployment that has not named them. The terms page drops the section
	// rather than stating who to write to and leaving the address out.
	Contact      string
	Jurisdiction string
}

// termsUpdated is the date the terms last changed. Bump it when editing
// templates/terms.html; the page states it and the terms say it is binding.
const termsUpdated = "3 October 2026"

func (s *Server) handleFAQ(w http.ResponseWriter, r *http.Request) {
	s.renderInfo(w, r, "faq", "FAQ")
}

func (s *Server) handleTerms(w http.ResponseWriter, r *http.Request) {
	s.renderInfo(w, r, "terms", "Terms of Service")
}

// renderInfo draws a page that is readable signed in or out. The session is
// resolved here rather than by requireUser, which would send signed-out
// visitors to the front page; looking it up keeps the header's nav for anyone
// who is signed in.
func (s *Server) renderInfo(w http.ResponseWriter, r *http.Request, name, title string) {
	user, sessionID := s.currentUser(r)
	p := page{Title: title, User: user, Data: s.infoData()}
	// render() reads the session from the request context, which only
	// requireUser fills in, so the token has to come from the session resolved
	// here. Every other page holds the invariant that a page with a user has a
	// token to post with; these two pages carry no form today, and the next one
	// added to the layout should not quietly fail on them alone.
	if sessionID != "" {
		p.CSRF = s.keys.CSRFToken(sessionID)
	}
	s.render(w, r, http.StatusOK, name, p)
}

// renderIndex draws the sign-in page. Sign-in fails in several ways and each
// one lands back here, so they share an entry point rather than repeating what
// the page needs.
//
// The services it names come from the destination registry rather than from a
// sentence typed into the template. The page that names every supported
// service is the one place a new service is easiest to forget.
func (s *Server) renderIndex(w http.ResponseWriter, r *http.Request, status int, message string) {
	s.render(w, r, status, "index", page{
		Title:   "feedrepeater",
		Error:   message,
		Data:    s.infoData(),
		Handoff: true,
	})
}

// infoData collects the numbers a page states about how the service behaves.
func (s *Server) infoData() infoData {
	return infoData{
		Interval:        humanInterval(s.cfg.PollInterval),
		MaxItems:        s.cfg.MaxItemsPerPoll,
		MaxFeeds:        store.MaxFeedsPerAccount,
		MaxDestinations: store.MaxDestinationsPerAccount,
		Kinds:           destination.Kinds,
		Updated:         termsUpdated,
		Contact:         s.cfg.Contact,
		Jurisdiction:    s.cfg.Jurisdiction,
	}
}

// --- helpers ---------------------------------------------------------------

// destinationFromPath loads the destination named in the path, scoped to the
// signed-in user, and writes the response itself if it cannot.
func (s *Server) destinationFromPath(w http.ResponseWriter, r *http.Request) *store.Destination {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.fail(w, r, http.StatusNotFound, "That destination does not exist.")
		return nil
	}
	d, err := s.store.Destination(r.Context(), userFrom(r).ID, id)
	if err != nil {
		// A destination owned by someone else is reported the same way as one
		// that does not exist.
		s.fail(w, r, http.StatusNotFound, "That destination does not exist.")
		return nil
	}
	return d
}

func (s *Server) sampleItem(ctx context.Context, user *store.User) destination.Item {
	item := destination.Item{
		Title:     "feedrepeater test post",
		URL:       s.cfg.BaseURL.String(),
		Summary:   "This is a test post from feedrepeater.",
		Author:    user.Acct,
		Published: time.Now().UTC(),
		FeedTitle: "feedrepeater",
		FeedURL:   s.cfg.BaseURL.String(),
	}
	feeds, err := s.store.FeedsByUser(ctx, user.ID)
	if err != nil || len(feeds) == 0 {
		return item
	}
	item.FeedTitle, item.FeedURL = nonEmpty(feeds[0].Title, hostOf(feeds[0].URL)), feeds[0].URL
	return item
}

// formPriority reads the ntfy priority field, falling back to ntfy's own
// default. An out-of-range value is corrected rather than rejected: it can only
// come from a hand-made request, and there is nothing useful to say about it.
func formPriority(r *http.Request) int {
	p, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("priority")))
	if err != nil || !destination.ValidNtfyPriority(p) {
		return destination.DefaultNtfyPriority
	}
	return p
}

func defaultLabel(kind string, user *store.User) string {
	if kind == destination.KindMastodon {
		return user.Handle()
	}
	return kindLabel(kind)
}

func clipLabel(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	return clip(s, 80)
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func humanInterval(d time.Duration) string {
	if d < time.Hour {
		return strconv.Itoa(int(d.Minutes())) + " minutes"
	}
	return strconv.Itoa(int(d.Hours())) + " hours"
}

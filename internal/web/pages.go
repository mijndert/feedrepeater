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
	"feedrepeater.com/internal/render"
	"feedrepeater.com/internal/store"
)

type dashboardData struct {
	Feed    *store.FeedView
	FeedURL string
	// Destination is where the feed is published: the Mastodon account this
	// person signed in with. It is made when the feed is added rather than
	// chosen, so the dashboard states it instead of offering it.
	Destination *store.Destination
	// Activity is delivery history folded into one line per entry.
	Activity []activityEntry
	// ActivityMore says history was cut off, so the list can admit it rather
	// than looking like everything there is.
	ActivityMore bool
	Pending      int
	Interval     string
}

// Visibility is how the destination posts, for the line under the account.
func (d dashboardData) Visibility() string {
	if d.Destination == nil {
		return ""
	}
	cfg, err := decodeConfig[destination.MastodonConfig](d.Destination.Config)
	if err != nil {
		return ""
	}
	return cfg.Visibility
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
// every entry went everywhere, with room for the trailing group that gets
// dropped below.
const (
	activityEntries = 10
	// One destination per account is one delivery per entry, so the read is the
	// cap plus the row that says there is more behind it. It was ten times the
	// number of services when an account could have seven of them.
	activityRows = activityEntries + 1
)

// activityEntry is one entry and every delivery of it.
type activityEntry struct {
	ItemID int64
	Title  string
	URL    string
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
				ItemID: d.ItemID, Title: d.ItemTitle, URL: d.ItemURL, UpdatedAt: d.UpdatedAt,
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
	user := userFrom(r)
	data := dashboardData{Interval: humanInterval(s.cfg.MinPollInterval)}

	f, err := s.store.FeedByUser(r.Context(), user.ID)
	if err == nil {
		data.Feed = f
		data.FeedURL = f.URL
	} else if !errors.Is(err, store.ErrNotFound) {
		s.log.Error("load feed", "error", err)
	}

	data.Destination = s.destinationOfKind(r.Context(), user.ID, destination.KindMastodon)
	if data.Activity, data.ActivityMore, err = s.recentActivity(r.Context(), user.ID); err != nil {
		s.log.Error("load deliveries", "error", err)
	}
	if data.Pending, err = s.store.PendingCount(r.Context(), user.ID); err != nil {
		s.log.Error("count pending", "error", err)
	}

	s.render(w, r, http.StatusOK, "dashboard", page{Title: "feedrepeater", Data: data})
}

// --- feed ------------------------------------------------------------------

func (s *Server) handleFeedSave(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	raw := r.PostFormValue("url")

	// There is one feed per account and no way to edit its address: removing
	// and adding is the only path, which keeps the entry history and the URL
	// from ever disagreeing.
	if _, err := s.store.FeedByUser(r.Context(), user.ID); err == nil {
		s.dashboardError(w, r, "You already have a feed. Remove it before adding a different one.", "")
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		s.log.Error("check existing feed", "error", err)
		s.fail(w, r, http.StatusInternalServerError, "Something went wrong.")
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
		NextFetchAt:  now.Add(s.cfg.MinPollInterval),
	}

	// Where this feed publishes is not a question with two answers: it is the
	// Mastodon account this person signed in with, and it is connected here
	// rather than asked for. Before Subscribe, because Subscribe attaches a new
	// feed to the destinations that already exist — after it, there would be a
	// window in which the feed was polled and published nowhere.
	//
	// Almost always this finds the destination made at sign-in and does
	// nothing. It matters for the account that predates that, or whose
	// destination could not be written then.
	if _, err := s.ensureMastodonDestination(ctx, user, ""); err != nil {
		s.log.Error("connect mastodon destination", "user", user.ID, "error", err)
		s.dashboardError(w, r, "Your feed was not added: posting to your Mastodon account could not be set up. Sign out, sign back in, and try again.", raw)
		return
	}

	if _, _, err := s.store.Subscribe(r.Context(), user.ID, feedURL, res.Title, seed, state); err != nil {
		s.log.Error("save feed", "error", err)
		s.fail(w, r, http.StatusInternalServerError, "Something went wrong.")
		return
	}

	if discovered {
		redirect(w, r, "/dashboard", "feed-found")
		return
	}
	redirect(w, r, "/dashboard", "feed-saved")
}

func (s *Server) dashboardError(w http.ResponseWriter, r *http.Request, msg, feedURL string) {
	user := userFrom(r)
	data := dashboardData{Interval: humanInterval(s.cfg.MinPollInterval), FeedURL: feedURL}
	if f, err := s.store.FeedByUser(r.Context(), user.ID); err == nil {
		data.Feed = f
	}
	data.Destination = s.destinationOfKind(r.Context(), user.ID, destination.KindMastodon)
	data.Activity, data.ActivityMore, _ = s.recentActivity(r.Context(), user.ID)
	s.render(w, r, http.StatusBadRequest, "dashboard", page{Title: "feedrepeater", Error: msg, Data: data})
}

func (s *Server) handleFeedPause(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	pause := r.PostFormValue("paused") == "1"
	if err := s.store.SetSubscriptionPaused(r.Context(), user.ID, pause); err != nil {
		s.log.Error("pause feed", "error", err)
	}
	if pause {
		redirect(w, r, "/dashboard", "feed-paused")
		return
	}
	redirect(w, r, "/dashboard", "feed-resumed")
}

func (s *Server) handleFeedRefresh(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	if !s.writeLimit.allow("refresh:" + strconv.FormatInt(user.ID, 10)) {
		s.dashboardError(w, r, "Too many refreshes. Try again later.", "")
		return
	}
	if err := s.store.FetchNow(r.Context(), user.ID); err != nil {
		s.log.Error("schedule fetch", "error", err)
	}
	redirect(w, r, "/dashboard", "refreshing")
}

func (s *Server) handleFeedDelete(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	// Unsubscribing collects orphaned feeds, so it is as much of a write as
	// adding one. Limiting only the add left save/delete as an unbounded loop.
	if !s.writeLimit.allow("feed:" + strconv.FormatInt(user.ID, 10)) {
		s.dashboardError(w, r, "Too many changes. Try again later.", "")
		return
	}
	if err := s.store.Unsubscribe(r.Context(), user.ID); err != nil {
		s.log.Error("delete feed", "error", err)
	}
	redirect(w, r, "/dashboard", "feed-removed")
}

// --- destinations ----------------------------------------------------------

type destinationForm struct {
	Dest         *store.Destination
	Label        string
	Template     string
	Visibility   string
	Account      string
	Paused       bool
	Variables    []render.Variable
	Visibilities []string
	Preview      string
	Limit        int
}

func (s *Server) handleDestinationEdit(w http.ResponseWriter, r *http.Request) {
	d := s.destinationFromPath(w, r)
	if d == nil {
		return
	}
	s.renderDestinationEdit(w, r, d, "")
}

func (s *Server) handleDestinationUpdate(w http.ResponseWriter, r *http.Request) {
	d := s.destinationFromPath(w, r)
	if d == nil {
		return
	}
	user := userFrom(r)

	// Saving writes nothing to the instance, but it is still a write here and
	// the limiter is what keeps a held-down key from being a workload.
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
		s.renderDestinationEdit(w, r, d, err.Error())
		return
	}
	d.Label = label
	d.Template = tmpl
	d.Paused = r.PostFormValue("paused") == "1"

	if v := r.PostFormValue("visibility"); destination.ValidVisibility(v) {
		cfg, _ := decodeConfig[destination.MastodonConfig](d.Config)
		cfg.Visibility = v
		if err := s.setConfig(d, cfg); err != nil {
			s.fail(w, r, http.StatusInternalServerError, "Something went wrong.")
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

func (s *Server) handleDestinationTest(w http.ResponseWriter, r *http.Request) {
	d := s.destinationFromPath(w, r)
	if d == nil {
		return
	}
	user := userFrom(r)
	if !s.writeLimit.allow("test:" + strconv.FormatInt(user.ID, 10)) {
		s.renderDestinationEdit(w, r, d, "Too many test posts. Try again later.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	item := s.sampleItem(ctx, user)
	if _, err := s.pub.Send(ctx, d, item, user.Location(), "test-"+strconv.FormatInt(time.Now().UnixNano(), 36)); err != nil {
		s.log.Info("test post failed", "destination", d.ID, "error", err)
		s.renderDestinationEdit(w, r, d, "Test post failed: "+clip(err.Error(), 300))
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
	// Hand the token back before the record of it is gone. Deleting the account
	// here should not leave this service still able to post as them.
	s.revokeUserToken(r.Context(), user)

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
	Interval string
	MaxItems int
	Updated  string
}

// termsUpdated is the date the terms last changed. Bump it when editing
// templates/terms.html; the page states it and the terms say it is binding.
const termsUpdated = "17 August 2026"

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
func (s *Server) renderIndex(w http.ResponseWriter, r *http.Request, status int, message string) {
	s.render(w, r, status, "index", page{Title: "feedrepeater", Error: message})
}

// infoData collects the numbers a page states about how the service behaves.
func (s *Server) infoData() infoData {
	return infoData{
		Interval: humanInterval(s.cfg.MinPollInterval),
		MaxItems: s.cfg.MaxItemsPerPoll,
		Updated:  termsUpdated,
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

// destinationOfKind returns the account's destination of this kind, if it has
// one. A failed lookup reads as "none", which at worst offers a form that the
// create handler then refuses.
func (s *Server) destinationOfKind(ctx context.Context, userID int64, kind string) *store.Destination {
	list, err := s.store.DestinationsByUser(ctx, userID)
	if err != nil {
		s.log.Error("load destinations", "error", err)
		return nil
	}
	for _, d := range list {
		if d.Kind == kind {
			return d
		}
	}
	return nil
}

func (s *Server) sampleItem(ctx context.Context, user *store.User) destination.Item {
	item := destination.Item{
		Title:     "feedrepeater test post",
		URL:       s.cfg.BaseURL.String(),
		Summary:   "This is a test post from feedrepeater.",
		Author:    user.Acct,
		Published: time.Now().UTC(),
		FeedTitle: "feedrepeater",
	}
	f, err := s.store.FeedByUser(ctx, user.ID)
	if err != nil {
		return item
	}
	item.FeedTitle = f.Title
	return item
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

package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"feedrepeater.com/internal/destination"
	"feedrepeater.com/internal/feed"
	"feedrepeater.com/internal/render"
	"feedrepeater.com/internal/store"
)

type dashboardData struct {
	Feed    *store.Feed
	FeedURL string
	// Destinations carries every destination the account owns, each marked
	// with whether the feed publishes to it.
	Destinations []*store.RoutedDestination
	Deliveries   []*store.DeliveryView
	Pending      int
	Kinds        []destination.Kind
	Interval     string
}

// kindRow is one line of the destinations list: a service, and the account's
// destination for it once there is one.
type kindRow struct {
	destination.Kind
	Dest *store.RoutedDestination
}

// Rows lists every kind in display order, carrying the destination connected
// to it. An account has at most one per kind, so the list is the same length
// whatever the account has done. A duplicate left by older data would not fit
// that shape, so it gets a row to itself rather than disappearing.
func (d dashboardData) Rows() []kindRow {
	rows := make([]kindRow, 0, len(d.Kinds))
	taken := make(map[int64]bool, len(d.Destinations))
	for _, k := range d.Kinds {
		row := kindRow{Kind: k}
		for _, dest := range d.Destinations {
			if dest.Kind == k.Name {
				row.Dest = dest
				taken[dest.ID] = true
				break
			}
		}
		rows = append(rows, row)
	}
	for _, dest := range d.Destinations {
		if taken[dest.ID] {
			continue
		}
		kind, ok := destination.KindByName(dest.Kind)
		if !ok {
			kind = destination.Kind{Name: dest.Kind, Label: kindLabel(dest.Kind)}
		}
		rows = append(rows, kindRow{Kind: kind, Dest: dest})
	}
	return rows
}

// RoutedCount reports how many destinations the feed publishes to.
func (d dashboardData) RoutedCount() int {
	n := 0
	for _, dest := range d.Destinations {
		if dest.Routed && !dest.Paused {
			n++
		}
	}
	return n
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	data := dashboardData{
		Kinds:    destination.Kinds,
		Interval: humanInterval(s.cfg.MinPollInterval),
	}

	f, err := s.store.FeedByUser(r.Context(), user.ID)
	if err == nil {
		data.Feed = f
		data.FeedURL = f.URL
	} else if !errors.Is(err, store.ErrNotFound) {
		s.log.Error("load feed", "error", err)
	}

	if data.Destinations, err = s.routedDestinations(r.Context(), user.ID, data.Feed); err != nil {
		s.log.Error("load destinations", "error", err)
	}
	if data.Deliveries, err = s.store.RecentDeliveries(r.Context(), user.ID, 25); err != nil {
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

	f, err := s.store.SetFeed(r.Context(), user.ID, feedURL)
	if err != nil {
		s.log.Error("save feed", "error", err)
		s.fail(w, r, http.StatusInternalServerError, "Something went wrong.")
		return
	}

	// Prime from the fetch that was just made: every entry the feed already has
	// is recorded as seen and delivered nowhere. Posting starts from what
	// appears after this moment, so adding a feed never floods a timeline.
	// Doing it here rather than waiting for the first poll means there is no
	// window in which an existing entry could be mistaken for a new one.
	now := time.Now().UTC()
	primed, err := feed.Ingest(r.Context(), s.store, f.ID, user.ID, res.Entries, true, s.cfg.MaxItemsPerPoll)
	if err != nil {
		s.log.Error("prime feed", "feed", f.ID, "error", err)
	} else {
		f.Primed = true
		s.log.Info("feed primed", "feed", f.ID, "entries", primed.New)
	}

	f.Title = res.Title
	f.ETag, f.LastModified = res.ETag, res.LastModified
	f.LastFetchAt = &now
	f.NextFetchAt = now.Add(s.cfg.MinPollInterval)
	if err := s.store.RecordFetch(r.Context(), f); err != nil {
		s.log.Error("record feed state", "error", err)
	}

	if discovered {
		redirect(w, r, "/dashboard", "feed-found")
		return
	}
	redirect(w, r, "/dashboard", "feed-saved")
}

func (s *Server) dashboardError(w http.ResponseWriter, r *http.Request, msg, feedURL string) {
	user := userFrom(r)
	data := dashboardData{Kinds: destination.Kinds, Interval: humanInterval(s.cfg.MinPollInterval), FeedURL: feedURL}
	if f, err := s.store.FeedByUser(r.Context(), user.ID); err == nil {
		data.Feed = f
	}
	data.Destinations, _ = s.routedDestinations(r.Context(), user.ID, data.Feed)
	data.Deliveries, _ = s.store.RecentDeliveries(r.Context(), user.ID, 25)
	s.render(w, r, http.StatusBadRequest, "dashboard", page{Title: "feedrepeater", Error: msg, Data: data})
}

func (s *Server) handleFeedPause(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	pause := r.PostFormValue("paused") == "1"
	if err := s.store.SetFeedPaused(r.Context(), user.ID, pause); err != nil {
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
	if err := s.store.DeleteFeed(r.Context(), userFrom(r).ID); err != nil {
		s.log.Error("delete feed", "error", err)
	}
	redirect(w, r, "/dashboard", "feed-removed")
}

// routedDestinations lists the account's destinations, marked with whether the
// feed publishes to them. With no feed yet, none are marked.
func (s *Server) routedDestinations(ctx context.Context, userID int64, f *store.Feed) ([]*store.RoutedDestination, error) {
	if f != nil {
		return s.store.DestinationsForFeed(ctx, userID, f.ID)
	}
	plain, err := s.store.DestinationsByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]*store.RoutedDestination, 0, len(plain))
	for _, d := range plain {
		out = append(out, &store.RoutedDestination{Destination: *d})
	}
	return out, nil
}

// handleFeedRoutes saves which destinations the feed publishes to. Submitted
// ids are filtered against the account's own destinations in the store, so an
// id belonging to someone else is simply not attached.
func (s *Server) handleFeedRoutes(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	f, err := s.store.FeedByUser(r.Context(), user.ID)
	if err != nil {
		redirect(w, r, "/dashboard", "")
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

	if err := s.store.SetFeedRoutes(r.Context(), user.ID, f.ID, ids); err != nil {
		s.log.Error("save routes", "feed", f.ID, "error", err)
		s.fail(w, r, http.StatusInternalServerError, "Something went wrong.")
		return
	}
	redirect(w, r, "/dashboard", "routes-saved")
}

// --- destinations ----------------------------------------------------------

type destinationForm struct {
	Kind         destination.Kind
	Dest         *store.Destination
	Label        string
	Template     string
	Visibility   string
	Handle       string
	URL          string
	Account      string
	Paused       bool
	NewSecret    string
	Variables    []render.Variable
	Visibilities []string
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
	// One destination per kind, so a kind already connected has nothing to add.
	// Sending them to the one they have beats a form that cannot be submitted.
	if d := s.destinationOfKind(r.Context(), user.ID, kind.Name); d != nil {
		http.Redirect(w, r, "/destinations/"+strconv.FormatInt(d.ID, 10), http.StatusSeeOther)
		return
	}
	form := destinationForm{
		Kind:         kind,
		Label:        defaultLabel(kind.Name, user),
		Template:     kind.DefaultTemplate,
		Account:      user.Handle(),
		Variables:    render.Variables,
		Visibilities: destination.Visibilities,
		Visibility:   "public",
	}
	s.render(w, r, http.StatusOK, "destination_new", page{Title: "Add destination", Data: form})
}

func (s *Server) handleDestinationCreate(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	kind, ok := destination.KindByName(r.PostFormValue("kind"))
	if !ok {
		s.fail(w, r, http.StatusBadRequest, "Unknown destination type.")
		return
	}
	// One destination per kind. This check is for the message: the unique index
	// behind CreateDestination is what actually holds the rule, since between
	// here and the insert sits a network round trip. It runs before the rate
	// limiter so a duplicate does not spend write budget it never uses.
	if d := s.destinationOfKind(r.Context(), user.ID, kind.Name); d != nil {
		s.fail(w, r, http.StatusBadRequest, "You already have a "+kind.Label+" destination. Edit that one instead.")
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
		Visibility:   r.PostFormValue("visibility"),
		Handle:       strings.TrimSpace(r.PostFormValue("handle")),
		URL:          strings.TrimSpace(r.PostFormValue("url")),
		Account:      user.Handle(),
		Variables:    render.Variables,
		Visibilities: destination.Visibilities,
	}
	if form.Label == "" {
		form.Label = defaultLabel(kind.Name, user)
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
	case destination.KindMastodon:
		err = s.configureMastodon(ctx, user, d, form.Visibility)
	case destination.KindBluesky:
		err = s.configureBluesky(ctx, d, form.Handle, r.PostFormValue("app_password"))
	case destination.KindDiscord:
		err = s.configureDiscord(ctx, d, form.URL)
	case destination.KindSlack:
		err = s.configureSlack(ctx, d, form.URL)
	case destination.KindWebhook:
		newSecret, err = s.configureWebhook(ctx, d, form.URL)
	}
	if err != nil {
		s.destinationFormError(w, r, "destination_new", form, err.Error())
		return
	}

	if err := s.store.CreateDestination(r.Context(), d); err != nil {
		// Two requests for the same kind raced and this one lost. The
		// credentials it just verified are not stored, so nothing is left
		// behind to clean up.
		if errors.Is(err, store.ErrDuplicateKind) {
			s.fail(w, r, http.StatusBadRequest, "You already have a "+kind.Label+" destination. Edit that one instead.")
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
	if _, err := s.pub.Send(ctx, d, item, "test-"+strconv.FormatInt(time.Now().UnixNano(), 36)); err != nil {
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
	redirect(w, r, "/dashboard", "retrying")
}

// --- settings --------------------------------------------------------------

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "settings", page{Title: "Settings"})
}

func (s *Server) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	if r.PostFormValue("confirm") != user.Acct {
		s.render(w, r, http.StatusBadRequest, "settings", page{
			Title: "Settings",
			Error: "Type your username exactly to confirm.",
		})
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
		FeedURL:   s.cfg.BaseURL.String(),
	}
	f, err := s.store.FeedByUser(ctx, user.ID)
	if err != nil {
		return item
	}
	item.FeedTitle, item.FeedURL = f.Title, f.URL
	return item
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

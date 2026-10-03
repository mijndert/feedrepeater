package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"feedrepeater.com/internal/destination"
	"feedrepeater.com/internal/mastodon"
	"feedrepeater.com/internal/render"
	"feedrepeater.com/internal/secret"
	"feedrepeater.com/internal/store"
)

// --- Mastodon ----------------------------------------------------------------

// ensureMastodonDestination returns the destination that posts as the account
// this person signed in with, creating it if there is not one yet.
//
// It is created on the way in and repaired on every sign-in, which is what
// makes a dashboard with a feed on it always a dashboard that posts somewhere.
// Further Mastodon accounts are connected through connectMastodonDestination;
// this one is the account's own, and the only one made without being asked.
//
// token is the freshly issued access token when the caller has just been given
// one, and empty when it does not have one, in which case the stored token is
// read and used without being checked with the instance first — it was checked
// when it was issued, and a token revoked since shows up as a failed delivery
// with the instance's own words on the dashboard.
//
// retired is a token the destination held and no longer does, for the caller
// to revoke once nothing relies on it. It is empty when nothing was replaced.
func (s *Server) ensureMastodonDestination(ctx context.Context, user *store.User, token string) (d *store.Destination, retired string, err error) {
	if d := s.mastodonDestinationFor(ctx, user.ID, user.Host, user.RemoteID); d != nil {
		if token == "" {
			return d, "", nil
		}
		retired, err := s.refreshMastodonToken(ctx, d, user.Host, user.Acct, user.RemoteID, token)
		return d, retired, err
	}

	if token == "" {
		if token, err = s.storedToken(user); err != nil {
			return nil, "", err
		}
	}
	d, err = s.createMastodonDestination(ctx, user, user.Host, user.Acct, user.RemoteID, token)
	if err != nil {
		// Two requests raced and this one lost — a second tab adding a feed, or
		// a sign-in landing while one is being added. The row the winner wrote
		// is the same row this would have written, so it is the answer.
		if existing := s.mastodonDestinationFor(ctx, user.ID, user.Host, user.RemoteID); existing != nil {
			return existing, "", nil
		}
		return nil, "", err
	}
	return d, "", nil
}

// connectMastodonDestination records a Mastodon account that has just been
// authorised through the connect flow as a destination of user. An account
// already connected is re-sealed against the new token rather than connected
// twice, which also makes re-running the flow the repair for a revoked one.
func (s *Server) connectMastodonDestination(ctx context.Context, user *store.User, host string, account *mastodon.Account, token string) (d *store.Destination, retired string, err error) {
	if d := s.mastodonDestinationFor(ctx, user.ID, host, account.ID); d != nil {
		retired, err := s.refreshMastodonToken(ctx, d, host, account.Acct, account.ID, token)
		return d, retired, err
	}
	d, err = s.createMastodonDestination(ctx, user, host, account.Acct, account.ID, token)
	return d, "", err
}

func (s *Server) createMastodonDestination(ctx context.Context, user *store.User, host, acct, remoteID, token string) (*store.Destination, error) {
	d := &store.Destination{
		UserID: user.ID,
		Kind:   destination.KindMastodon,
		Label:  "@" + acct + "@" + host,
		// The account's own default wins over the built-in one, which is the
		// whole point of setting one in Settings.
		Template: nonEmpty(user.DefaultTemplate, render.DefaultTemplate),
	}
	if err := s.setMastodonConfig(d, host, acct, remoteID, "public"); err != nil {
		return nil, err
	}
	if err := s.setCredentials(d, destination.MastodonCredentials{AccessToken: token}); err != nil {
		return nil, err
	}
	if err := s.store.CreateDestination(ctx, d); err != nil {
		return nil, err
	}
	return d, nil
}

// refreshMastodonToken re-seals a destination against a token just issued for
// its account, so re-authorising repairs one whose token was revoked.
//
// The handle is updated rather than compared: a handle is display text the
// instance can change, and the row was matched on (host, account id) already.
// The token it held before comes back so the caller can retire it — a token
// this service no longer uses should not stay live at the instance.
func (s *Server) refreshMastodonToken(ctx context.Context, d *store.Destination, host, acct, remoteID, token string) (string, error) {
	cfg, err := decodeConfig[destination.MastodonConfig](d.Config)
	if err != nil {
		cfg = destination.MastodonConfig{}
	}
	previous := s.storedMastodonToken(d)
	if err := s.setMastodonConfig(d, host, acct, remoteID, cfg.Visibility); err != nil {
		return "", err
	}
	if err := s.setCredentials(d, destination.MastodonCredentials{AccessToken: token}); err != nil {
		return "", err
	}
	if err := s.store.UpdateDestination(ctx, d); err != nil {
		return "", err
	}
	if previous == token {
		previous = ""
	}
	return previous, nil
}

// mastodonDestinationFor returns the account's destination that posts as the
// Mastodon account identified by host and remoteID, if it has one. A failed
// lookup reads as "none", which at worst offers a form that the create handler
// then refuses.
func (s *Server) mastodonDestinationFor(ctx context.Context, userID int64, host, remoteID string) *store.Destination {
	list, err := s.store.DestinationsByUser(ctx, userID)
	if err != nil {
		s.log.Error("load destinations", "error", err)
		return nil
	}
	for _, d := range list {
		if d.Kind != destination.KindMastodon {
			continue
		}
		cfg, err := decodeConfig[destination.MastodonConfig](d.Config)
		if err == nil && cfg.SameAccount(host, remoteID) {
			return d
		}
	}
	return nil
}

// storedToken reads back the access token this account signed in with.
func (s *Server) storedToken(user *store.User) (string, error) {
	token, err := s.keys.DecryptString(secret.PurposeUserToken, user.AccessToken)
	if err != nil {
		return "", fmt.Errorf("your stored authorisation could not be read; sign out and back in")
	}
	return token, nil
}

// storedMastodonToken reads back the token a Mastodon destination posts with,
// to be re-sealed or revoked rather than shown. An unreadable blob reads as no
// token.
func (s *Server) storedMastodonToken(d *store.Destination) string {
	if d.Kind != destination.KindMastodon || len(d.Credentials) == 0 {
		return ""
	}
	raw, err := s.keys.Decrypt(secret.PurposeDestination, d.Credentials)
	if err != nil {
		return ""
	}
	var creds destination.MastodonCredentials
	if json.Unmarshal(raw, &creds) != nil {
		return ""
	}
	return creds.AccessToken
}

// isOwnDestination reports whether d posts as the account user signed in with.
// Decided by identity, not by comparing tokens: the two can disagree for a
// moment after a re-authorisation, and a stale cached user struct would make
// the comparison wrong for thirty seconds.
func isOwnDestination(user *store.User, d *store.Destination) bool {
	if d.Kind != destination.KindMastodon {
		return false
	}
	cfg, err := decodeConfig[destination.MastodonConfig](d.Config)
	return err == nil && cfg.SameAccount(user.Host, user.RemoteID)
}

// revokeDestinationToken hands a Mastodon destination's token back to its
// instance, for a destination about to go.
//
// ownToo says whether the destination that posts as the signed-in account is in
// scope. Deleting a destination passes false: that row cannot be deleted, and
// its token is the sign-in token. Deleting the account passes true, with the
// account as freshly read from the store, and the token is revoked only when
// it differs from the sign-in token — which revokeUserToken has already
// handled — so a destination left holding an older token of its own does not
// outlive the account. Other kinds hold nothing an instance can be asked to
// forget.
func (s *Server) revokeDestinationToken(ctx context.Context, user *store.User, d *store.Destination, ownToo bool) {
	if !ownToo && isOwnDestination(user, d) {
		return
	}
	token := s.storedMastodonToken(d)
	if token == "" {
		return
	}
	if own, err := s.storedToken(user); err == nil && own == token {
		return
	}
	cfg, err := decodeConfig[destination.MastodonConfig](d.Config)
	if err != nil || cfg.Host == "" {
		return
	}
	s.revokeToken(ctx, cfg.Host, token)
}

// revokeToken best-effort invalidates a token at its instance. Failure is
// logged, never surfaced: the caller is in the middle of something the user
// asked for, and an unreachable instance must not stop it.
func (s *Server) revokeToken(ctx context.Context, host, token string) {
	app, err := s.oauthApp(ctx, host)
	if err != nil {
		s.log.Warn("no client credentials to revoke with", "host", host, "error", err)
		return
	}
	if err := s.mastodon.RevokeToken(ctx, host, app.ClientID, app.ClientSecret, token); err != nil {
		s.log.Warn("could not revoke token", "host", host, "error", err)
	}
}

func (s *Server) setMastodonConfig(d *store.Destination, host, acct, remoteID, visibility string) error {
	if !destination.ValidVisibility(visibility) {
		visibility = "public"
	}
	return s.setConfig(d, destination.MastodonConfig{
		Host: host, Acct: acct, RemoteID: remoteID, Visibility: visibility,
	})
}

// --- the other services ------------------------------------------------------

// configureBluesky resolves the handle and verifies the app password by opening
// a session; storing credentials that do not work helps nobody.
func (s *Server) configureBluesky(ctx context.Context, d *store.Destination, handle, appPassword string) error {
	cfg, err := destination.ResolveBlueskyAccount(ctx, s.http, handle)
	if err != nil {
		return err
	}
	if len(appPassword) < 8 || len(appPassword) > 200 {
		return fmt.Errorf("enter the app password generated in your Bluesky settings")
	}
	// Any session cached against these credentials is dropped first, so a wrong
	// password fails here rather than being masked by an old token.
	destination.ForgetBlueskySession(cfg.DID, appPassword)
	if err := destination.CheckBlueskyLogin(ctx, s.http, cfg, appPassword); err != nil {
		s.log.Info("bluesky login check failed", "error", err)
		return fmt.Errorf("Bluesky did not accept that handle and app password")
	}
	if err := s.setConfig(d, cfg); err != nil {
		return err
	}
	return s.setCredentials(d, destination.BlueskyCredentials{AppPassword: appPassword})
}

// configureDiscord verifies the webhook URL and stores it as a credential.
//
// The URL is the whole authorisation, so it is sealed like a password rather
// than kept in the config blob, and the form never shows it back. Discord will
// describe a webhook on request, so this proves the URL works without posting
// anything into the channel.
func (s *Server) configureDiscord(ctx context.Context, d *store.Destination, rawURL string) error {
	cfg, err := destination.VerifyDiscordWebhook(ctx, s.http, rawURL)
	if err != nil {
		s.log.Info("discord webhook check failed", "error", err)
		return err
	}
	u, err := destination.ParseDiscordWebhookURL(rawURL)
	if err != nil {
		return err
	}
	if err := s.setConfig(d, cfg); err != nil {
		return err
	}
	return s.setCredentials(d, destination.DiscordCredentials{URL: u.String()})
}

// configureSlack verifies the incoming webhook and stores it as a credential.
// Slack offers no way to ask about a hook without using it, so verification
// posts one line into the channel; the form says so before it is submitted.
func (s *Server) configureSlack(ctx context.Context, d *store.Destination, rawURL string) error {
	u, cfg, err := destination.ParseSlackWebhookURL(rawURL)
	if err != nil {
		return err
	}
	if err := destination.VerifySlackWebhook(ctx, s.http, u.String()); err != nil {
		s.log.Info("slack webhook check failed", "error", err)
		return fmt.Errorf("Slack did not accept that webhook: %v", err)
	}
	if err := s.setConfig(d, cfg); err != nil {
		return err
	}
	return s.setCredentials(d, destination.SlackCredentials{URL: u.String()})
}

// configureNtfy verifies the server and topic, then stores the topic in the
// clear and the access token sealed.
//
// The server address is user-supplied, so it is checked before anything is
// stored: see VerifyNtfy for why that check is the difference between this kind
// and an open relay. The token is optional — an open topic on ntfy.sh needs
// none — but a stored empty token is still a stored credential blob, so the
// shape of the row does not change with it.
//
// verify runs that check and publishes a confirming notification. It is always
// true when connecting and true on an edit only when the destination moved,
// since a template change should not buzz a phone.
func (s *Server) configureNtfy(ctx context.Context, d *store.Destination, server, topic, token string, priority int, verify bool) error {
	u, err := destination.ParseNtfyServer(server)
	if err != nil {
		return err
	}
	if err := destination.ValidNtfyTopic(topic); err != nil {
		return err
	}
	if err := s.http.ValidateURL(u); err != nil {
		return fmt.Errorf("that server address cannot be used; enter a public https address")
	}
	if !destination.ValidNtfyPriority(priority) {
		priority = destination.DefaultNtfyPriority
	}
	if len(token) > 300 {
		return fmt.Errorf("that access token is too long")
	}
	if verify {
		if err := destination.VerifyNtfy(ctx, s.http, u.String(), topic, token); err != nil {
			s.log.Info("ntfy check failed", "host", u.Host, "error", err)
			return fmt.Errorf("could not publish to that topic: %v", err)
		}
	}
	if err := s.setConfig(d, destination.NtfyConfig{
		Server: u.String(), Topic: strings.TrimSpace(topic), Priority: priority,
	}); err != nil {
		return err
	}
	return s.setCredentials(d, destination.NtfyCredentials{Token: token})
}

// storedNtfyToken reads back the access token already saved for a destination.
//
// It is read to be re-sealed rather than shown: an edit that leaves the token
// field empty must keep the token it had, not quietly drop it and start failing
// on a protected topic. An unreadable blob reads as no token, which the form
// then reports as a topic that refused the request.
func (s *Server) storedNtfyToken(d *store.Destination) string {
	if len(d.Credentials) == 0 {
		return ""
	}
	raw, err := s.keys.Decrypt(secret.PurposeDestination, d.Credentials)
	if err != nil {
		return ""
	}
	var creds destination.NtfyCredentials
	if json.Unmarshal(raw, &creds) != nil {
		return ""
	}
	return creds.Token
}

// configureLinkding verifies the server and token, then stores the address, tags
// and unread flag in the clear and the token sealed.
//
// The address is user-supplied, so it is checked before anything is stored: see
// VerifyLinkding for why that check is the difference between this kind and an
// open relay. Unlike ntfy the token is required, since there is no such thing as
// a linkding that takes bookmarks from anybody.
//
// verify runs that check. It is always true when connecting, and on an edit only
// when the address or the token changed — not because verifying is expensive, but
// because a template edit should not fail on a home server that happens to be
// offline.
func (s *Server) configureLinkding(ctx context.Context, d *store.Destination, server, token, rawTags string, unread, verify bool) error {
	u, err := destination.ParseLinkdingServer(server)
	if err != nil {
		return err
	}
	if err := s.http.ValidateURL(u); err != nil {
		return fmt.Errorf("that address cannot be used; enter a public https address")
	}
	if err := destination.ValidLinkdingToken(token); err != nil {
		return err
	}
	tags, err := destination.ParseLinkdingTags(rawTags)
	if err != nil {
		return err
	}
	if verify {
		if err := destination.VerifyLinkding(ctx, s.http, u.String(), token); err != nil {
			s.log.Info("linkding check failed", "host", u.Host, "error", err)
			return fmt.Errorf("could not connect to that linkding: %v", err)
		}
	}
	if err := s.setConfig(d, destination.LinkdingConfig{
		Server: u.String(), Tags: tags, Unread: unread,
	}); err != nil {
		return err
	}
	return s.setCredentials(d, destination.LinkdingCredentials{Token: token})
}

// storedLinkdingToken reads back the API token already saved for a destination,
// for the same reason storedNtfyToken does: an edit that leaves the field empty
// keeps the token it had rather than dropping it. An unreadable blob reads as no
// token, which the form then reports as a token linkding refused.
func (s *Server) storedLinkdingToken(d *store.Destination) string {
	if len(d.Credentials) == 0 {
		return ""
	}
	raw, err := s.keys.Decrypt(secret.PurposeDestination, d.Credentials)
	if err != nil {
		return ""
	}
	var creds destination.LinkdingCredentials
	if json.Unmarshal(raw, &creds) != nil {
		return ""
	}
	return creds.Token
}

// configureWebhook stores the endpoint and mints a signing secret.
func (s *Server) configureWebhook(ctx context.Context, d *store.Destination, rawURL string) (string, error) {
	u, err := s.http.ParseURL(rawURL)
	if err != nil {
		return "", fmt.Errorf("that address cannot be used; enter a public http or https URL")
	}
	if err := s.setConfig(d, destination.WebhookConfig{URL: u.String()}); err != nil {
		return "", err
	}
	value, err := s.newWebhookSecret(d)
	if err != nil {
		return "", err
	}

	// Prove the address belongs to whoever is adding it, before it can be
	// used. Otherwise this service delivers requests to strangers on request.
	if err := destination.VerifyEndpoint(ctx, s.http, u.String(), value); err != nil {
		s.log.Info("webhook verification failed", "host", u.Host, "error", err)
		return "", fmt.Errorf("could not verify that address: %v. It must answer the verification request by returning the challenge value it was sent", err)
	}
	return value, nil
}

// newWebhookSecret generates and stores a fresh signing secret, returning the
// plaintext so it can be shown once.
func (s *Server) newWebhookSecret(d *store.Destination) (string, error) {
	value := secret.Token()
	if err := s.setCredentials(d, destination.WebhookCredentials{Secret: value}); err != nil {
		return "", err
	}
	return value, nil
}

// --- shared --------------------------------------------------------------------

func (s *Server) setConfig(d *store.Destination, cfg any) error {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	d.Config = string(raw)
	return nil
}

func (s *Server) setCredentials(d *store.Destination, creds any) error {
	raw, err := json.Marshal(creds)
	if err != nil {
		return err
	}
	sealed, err := s.keys.Encrypt(secret.PurposeDestination, raw)
	if err != nil {
		return err
	}
	d.Credentials = sealed
	return nil
}

func decodeConfig[T any](raw string) (T, error) {
	var out T
	if raw == "" {
		return out, nil
	}
	err := json.Unmarshal([]byte(raw), &out)
	return out, err
}

// renderDestinationEdit shows the edit form. newSecret is non-empty only on the
// one response that is allowed to reveal a webhook secret.
func (s *Server) renderDestinationEdit(w http.ResponseWriter, r *http.Request, d *store.Destination, newSecret, errMsg string) {
	kind, ok := destination.KindByName(d.Kind)
	if !ok {
		kind = destination.Kind{Name: d.Kind, Label: kindLabel(d.Kind)}
	}
	form := destinationForm{
		Kind:         kind,
		Dest:         d,
		Own:          isOwnDestination(userFrom(r), d),
		Label:        d.Label,
		Template:     d.Template,
		Paused:       d.Paused,
		NewSecret:    newSecret,
		Variables:    render.Variables,
		Visibilities: destination.Visibilities,
		Priorities:   destination.NtfyPriorities,
	}

	switch d.Kind {
	case destination.KindMastodon:
		cfg, _ := decodeConfig[destination.MastodonConfig](d.Config)
		form.Account = "@" + cfg.Acct + "@" + cfg.Host
		form.Visibility = cfg.Visibility
		form.Limit = mastodon.DefaultMaxCharacters
	case destination.KindBluesky:
		cfg, _ := decodeConfig[destination.BlueskyConfig](d.Config)
		form.Handle = cfg.Handle
		form.Account = "@" + cfg.Handle
		form.Limit = destination.BlueskyLimit
	case destination.KindDiscord:
		cfg, _ := decodeConfig[destination.DiscordConfig](d.Config)
		// The URL is a credential and is never rendered back, so the account
		// line names the webhook instead.
		form.Account = nonEmpty(cfg.Name, "Discord webhook")
		form.Limit = destination.DiscordLimit
	case destination.KindSlack:
		cfg, _ := decodeConfig[destination.SlackConfig](d.Config)
		form.Account = "Slack webhook"
		if cfg.Team != "" {
			form.Account = cfg.Team + " · " + cfg.Hook
		}
		form.Limit = destination.SlackLimit
	case destination.KindNtfy:
		cfg, _ := decodeConfig[destination.NtfyConfig](d.Config)
		form.Server = cfg.Server
		form.Topic = cfg.Topic
		form.Priority = cfg.Priority
		if !destination.ValidNtfyPriority(form.Priority) {
			form.Priority = destination.DefaultNtfyPriority
		}
		// The topic is not a secret, so unlike a channel webhook it can name the
		// destination. The token is, and is not rendered back.
		form.Account = hostOf(cfg.Server) + "/" + cfg.Topic
		form.Limit = destination.NtfyLimit
	case destination.KindLinkding:
		cfg, _ := decodeConfig[destination.LinkdingConfig](d.Config)
		form.Server = cfg.Server
		// Shown back in the form the way it is typed, so editing the list is
		// editing what is stored rather than starting again.
		form.Tags = strings.Join(cfg.Tags, ", ")
		form.Unread = cfg.Unread
		// The address is not a credential, so it names the destination; the token
		// is, and is not rendered back.
		form.Account = hostOf(cfg.Server)
		form.Limit = destination.LinkdingLimit
	case destination.KindWebhook:
		cfg, _ := decodeConfig[destination.WebhookConfig](d.Config)
		form.URL = cfg.URL
		form.Account = hostOf(cfg.URL)
	}

	form.Preview = s.previewTemplate(r, form.Template, form.Limit)

	status := http.StatusOK
	if errMsg != "" {
		status = http.StatusBadRequest
	}
	s.render(w, r, status, "destination_edit", page{Title: form.Label, Error: errMsg, Data: form})
}

// previewTemplate renders a template against a fixed example entry, so the
// effect of an edit is visible before anything is saved. limit is the service's
// post length, or 0 where there is none.
//
// The example is dated now rather than left empty, which is what makes the
// account's timezone visible: {{published}} is the one placeholder a zone
// changes, and it changes it exactly at the point a reader would notice.
func (s *Server) previewTemplate(r *http.Request, tmpl string, limit int) string {
	return render.Render(nonEmpty(tmpl, render.DefaultTemplate), render.Vars{
		Title:     "An example entry",
		URL:       "https://example.com/an-example-entry",
		Summary:   "The first part of the entry, with any markup removed.",
		Author:    "Example Author",
		FeedTitle: "Example Feed",
		Published: time.Now(),
		Location:  userFrom(r).Location(),
	}, limit)
}

func (s *Server) destinationFormError(w http.ResponseWriter, r *http.Request, tmpl string, form destinationForm, msg string) {
	form.Variables = render.Variables
	form.Visibilities = destination.Visibilities
	form.Priorities = destination.NtfyPriorities
	// For these two the URL is the credential, so a rejected form is not
	// re-filled with it. The templates do not render it either; this is the
	// belt to that pair of braces.
	if form.Kind.Name == destination.KindDiscord || form.Kind.Name == destination.KindSlack {
		form.URL = ""
	}
	s.render(w, r, http.StatusBadRequest, tmpl, page{
		Title:   "Add destination",
		Error:   msg,
		Data:    form,
		Handoff: form.Kind.Name == destination.KindMastodon,
	})
}

// errInstanceUnreachable reports that an instance could not be asked for a
// client registration, which is the one OAuth failure the visitor can do
// something about.
var errInstanceUnreachable = errors.New("instance unreachable")

func nonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

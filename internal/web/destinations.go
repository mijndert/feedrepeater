package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"feedrepeater.com/internal/destination"
	"feedrepeater.com/internal/render"
	"feedrepeater.com/internal/secret"
	"feedrepeater.com/internal/store"
)

// configureMastodon points a destination at the signed-in account, reusing the
// token granted at sign-in. There is no separate authorisation step because the
// sign-in already asked for permission to post.
func (s *Server) configureMastodon(ctx context.Context, user *store.User, d *store.Destination, visibility string) error {
	if !destination.ValidVisibility(visibility) {
		visibility = "public"
	}
	token, err := s.keys.DecryptString(secret.PurposeUserToken, user.AccessToken)
	if err != nil {
		return fmt.Errorf("your stored authorisation could not be read; sign out and back in")
	}
	// Confirm the token still works before saving a destination that would only
	// fail later, in the background.
	if _, err := s.mastodon.VerifyCredentials(ctx, user.Host, token); err != nil {
		return fmt.Errorf("your instance did not accept the stored authorisation; sign out and back in")
	}
	if err := s.setConfig(d, destination.MastodonConfig{
		Host: user.Host, Acct: user.Acct, Visibility: visibility,
	}); err != nil {
		return err
	}
	return s.setCredentials(d, destination.MastodonCredentials{AccessToken: token})
}

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
// This is one of two places a stored destination credential is decrypted outside
// the publisher, and it is read to be re-sealed rather than shown: an edit that
// leaves the token field empty must keep the token it had, not quietly drop it
// and start failing on a protected topic. An unreadable blob reads as no token,
// which the form then reports as a topic that refused the request.
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
	kind, _ := destination.KindByName(d.Kind)
	form := destinationForm{
		Kind:         kind,
		Dest:         d,
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
		form.Limit = 500
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
	s.render(w, r, http.StatusBadRequest, tmpl, page{Title: "Add destination", Error: msg, Data: form})
}

func nonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

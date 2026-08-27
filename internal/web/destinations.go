package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"feedrepeater.com/internal/destination"
	"feedrepeater.com/internal/render"
	"feedrepeater.com/internal/secret"
	"feedrepeater.com/internal/store"
)

// ensureMastodonDestination returns the account's destination, creating it if
// this account does not have one yet.
//
// Nobody connects anything here any more: you sign in with a Mastodon account,
// so the account to post to is already known and asking for it again would be
// asking a question with one answer. It is created on the way in and repaired
// on every sign-in, which is what makes a dashboard with a feed on it always a
// dashboard that posts somewhere.
//
// token is the freshly issued access token when the caller has just been given
// one, and empty when it does not have one, in which case the stored token is
// read and checked with the instance before anything is written.
func (s *Server) ensureMastodonDestination(ctx context.Context, user *store.User, token string) (*store.Destination, error) {
	if d := s.destinationOfKind(ctx, user.ID, destination.KindMastodon); d != nil {
		if token == "" {
			return d, nil
		}
		return d, s.refreshMastodonToken(ctx, user, d, token)
	}

	d := &store.Destination{
		UserID: user.ID,
		Kind:   destination.KindMastodon,
		Label:  user.Handle(),
		// The account's own default wins over the built-in one, which is the
		// whole point of setting one in Settings.
		Template: nonEmpty(user.DefaultTemplate, render.DefaultTemplate),
	}
	if token != "" {
		if err := s.setMastodonConfig(d, user, "public"); err != nil {
			return nil, err
		}
		if err := s.setCredentials(d, destination.MastodonCredentials{AccessToken: token}); err != nil {
			return nil, err
		}
	} else {
		stored, err := s.storedToken(user)
		if err != nil {
			return nil, err
		}
		if err := s.setMastodonConfig(d, user, "public"); err != nil {
			return nil, err
		}
		if err := s.setCredentials(d, destination.MastodonCredentials{AccessToken: stored}); err != nil {
			return nil, err
		}
	}

	if err := s.store.CreateDestination(ctx, d); err != nil {
		// Two requests raced and this one lost — a second tab adding a feed, or
		// a sign-in landing while one is being added. The row the winner wrote
		// is the same row this would have written, so it is the answer.
		if errors.Is(err, store.ErrDuplicateKind) {
			if existing := s.destinationOfKind(ctx, user.ID, destination.KindMastodon); existing != nil {
				return existing, nil
			}
		}
		return nil, err
	}
	return d, nil
}

// refreshMastodonToken re-seals a destination against the token a sign-in just
// issued, so re-authorising repairs one whose token was revoked.
//
// The account is matched on host alone. Identity here is (host, account id) and
// a handle is display text the instance can change, so comparing it would mean
// a rename quietly stopped the refresh and left the destination posting with a
// token that is about to be revoked. The stored handle is updated instead.
func (s *Server) refreshMastodonToken(ctx context.Context, user *store.User, d *store.Destination, token string) error {
	cfg, err := decodeConfig[destination.MastodonConfig](d.Config)
	if err != nil || cfg.Host != user.Host {
		return nil
	}
	if err := s.setMastodonConfig(d, user, cfg.Visibility); err != nil {
		return err
	}
	if err := s.setCredentials(d, destination.MastodonCredentials{AccessToken: token}); err != nil {
		return err
	}
	return s.store.UpdateDestination(ctx, d)
}

// storedToken reads back the access token this account signed in with.
//
// It is not checked with the instance first. It was checked when it was issued,
// it is re-sealed here on every sign-in, and a token that has been revoked in
// the meantime shows up as a failed delivery with the instance's own words on
// the dashboard. Asking the instance would put a third party between somebody
// and their own feed being added, which is a worse trade than a first post that
// fails and says why.
func (s *Server) storedToken(user *store.User) (string, error) {
	token, err := s.keys.DecryptString(secret.PurposeUserToken, user.AccessToken)
	if err != nil {
		return "", fmt.Errorf("your stored authorisation could not be read; sign out and back in")
	}
	return token, nil
}

func (s *Server) setMastodonConfig(d *store.Destination, user *store.User, visibility string) error {
	if !destination.ValidVisibility(visibility) {
		visibility = "public"
	}
	return s.setConfig(d, destination.MastodonConfig{
		Host: user.Host, Acct: user.Acct, Visibility: visibility,
	})
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

// renderDestinationEdit shows the posting settings for the account's
// destination.
func (s *Server) renderDestinationEdit(w http.ResponseWriter, r *http.Request, d *store.Destination, errMsg string) {
	cfg, _ := decodeConfig[destination.MastodonConfig](d.Config)
	form := destinationForm{
		Dest:         d,
		Label:        d.Label,
		Template:     d.Template,
		Paused:       d.Paused,
		Account:      "@" + cfg.Acct + "@" + cfg.Host,
		Visibility:   cfg.Visibility,
		Variables:    render.Variables,
		Visibilities: destination.Visibilities,
		Limit:        500,
	}
	form.Preview = s.previewTemplate(r, form.Template, form.Limit)

	status := http.StatusOK
	if errMsg != "" {
		status = http.StatusBadRequest
	}
	s.render(w, r, status, "destination_edit", page{Title: "Posting", Error: errMsg, Data: form})
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

func nonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

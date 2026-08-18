package web

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"feedrepeater.com/internal/destination"
	"feedrepeater.com/internal/mastodon"
	"feedrepeater.com/internal/secret"
	"feedrepeater.com/internal/store"
)

// redirectURI is fixed and registered with every instance.
func (s *Server) redirectURI() string { return s.cfg.AbsoluteURL("/auth/callback") }

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if user, _ := s.currentUser(r); user != nil {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	s.renderIndex(w, r, http.StatusOK, "")
}

// handleLogin starts the OAuth flow against the instance the visitor named.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := s.checkOrigin(r, true); err != nil {
		s.fail(w, r, http.StatusForbidden, "That request did not come from this site.")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, http.StatusBadRequest, "That form could not be read.")
		return
	}
	if !s.loginLimit.allow(clientIP(r)) {
		s.renderIndex(w, r, http.StatusTooManyRequests, "Too many sign-in attempts. Try again later.")
		return
	}

	host, err := mastodon.NormalizeHost(r.PostFormValue("instance"))
	if err != nil {
		s.renderIndex(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if s.cfg.InstanceBlocked(host) {
		s.log.Info("sign-in from blocked instance", "host", host)
		s.renderIndex(w, r, http.StatusForbidden, "Accounts on that server cannot sign in.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	app, err := s.oauthApp(ctx, host)
	if err != nil {
		s.log.Info("register app", "host", host, "error", err)
		s.renderIndex(w, r, http.StatusBadGateway, "Could not reach that instance. Check the address and try again.")
		return
	}

	// PKCE: the verifier stays here (encrypted), only its hash travels.
	verifier := secret.Token()
	challenge := pkceChallenge(verifier)
	sealed, err := s.keys.EncryptString(secret.PurposePKCEVerifier, verifier)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "Something went wrong.")
		return
	}

	state := secret.Token()
	if err := s.store.PutOAuthState(ctx, secret.Hash(state), host, sealed, oauthTTL); err != nil {
		s.log.Error("store oauth state", "error", err)
		s.fail(w, r, http.StatusInternalServerError, "Something went wrong.")
		return
	}
	s.setOAuthCookie(w, state)

	http.Redirect(w, r, mastodon.AuthorizeURL(host, app.ClientID, s.redirectURI(), state, challenge), http.StatusSeeOther)
}

// oauthApp returns the client credentials for an instance, registering them on
// first use.
func (s *Server) oauthApp(ctx context.Context, host string) (*mastodon.App, error) {
	redirectURI := s.redirectURI()

	in, err := s.store.Instance(ctx, host)
	if err == nil {
		// A client id is bound to the redirect URI it was registered with. If
		// this service has moved since — a new domain, a different port — the
		// stored client is dead: the instance would reject every authorisation
		// and the failure would happen over there, leaving nothing in these
		// logs but a 303. Re-register instead of reusing it.
		switch {
		case in.RedirectURI == "":
			s.log.Info("re-registering client with an unrecorded callback", "host", host)
		case in.RedirectURI != redirectURI:
			s.log.Warn("re-registering client after callback change",
				"host", host, "stored", in.RedirectURI, "current", redirectURI)
		default:
			clientSecret, err := s.keys.DecryptString(secret.PurposeClientSecret, in.ClientSecret)
			if err == nil {
				return &mastodon.App{ClientID: in.ClientID, ClientSecret: clientSecret}, nil
			}
			s.log.Error("decrypt client secret", "host", host, "error", err)
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	app, err := s.mastodon.RegisterApp(ctx, host, redirectURI, s.cfg.BaseURL.String())
	if err != nil {
		return nil, err
	}
	sealed, err := s.keys.EncryptString(secret.PurposeClientSecret, app.ClientSecret)
	if err != nil {
		return nil, err
	}
	if err := s.store.SaveInstance(ctx, &store.Instance{
		Host: host, ClientID: app.ClientID, ClientSecret: sealed, RedirectURI: redirectURI,
	}); err != nil {
		return nil, err
	}
	return app, nil
}

// handleCallback completes the OAuth flow.
func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	// Cleared up front, not deferred: a deferred Set-Cookie would be written
	// after the response headers had already gone out and would do nothing.
	s.clearOAuthCookie(w)

	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		s.fail(w, r, http.StatusBadRequest, "Sign-in was cancelled or refused by your instance.")
		return
	}

	state := q.Get("state")
	code := q.Get("code")
	if state == "" || code == "" {
		s.fail(w, r, http.StatusBadRequest, "That sign-in link is incomplete.")
		return
	}

	// The state must match the cookie set when the flow began, which stops
	// someone else's authorisation being completed in this browser.
	c, err := r.Cookie(s.oauthCookieName())
	if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(state)) != 1 {
		s.fail(w, r, http.StatusBadRequest, "That sign-in did not start here. Try again.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	// The host comes from what we recorded at the start, never from the
	// callback parameters, and the state is consumed so it cannot be replayed.
	host, sealedVerifier, err := s.store.TakeOAuthState(ctx, secret.Hash(state))
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "That sign-in link has expired. Try again.")
		return
	}
	verifier, err := s.keys.DecryptString(secret.PurposePKCEVerifier, sealedVerifier)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "That sign-in could not be completed. Try again.")
		return
	}

	app, err := s.oauthApp(ctx, host)
	if err != nil {
		s.fail(w, r, http.StatusBadGateway, "Could not reach that instance. Try again.")
		return
	}

	token, err := s.mastodon.ExchangeCode(ctx, host, app.ClientID, app.ClientSecret, code, s.redirectURI(), verifier)
	if err != nil {
		s.log.Info("exchange code", "host", host, "error", err)
		s.fail(w, r, http.StatusBadGateway, "That instance did not complete the sign-in. Try again.")
		return
	}

	account, err := s.mastodon.VerifyCredentials(ctx, host, token)
	if err != nil {
		s.log.Info("verify credentials", "host", host, "error", err)
		s.fail(w, r, http.StatusBadGateway, "That instance did not confirm the account. Try again.")
		return
	}

	sealedToken, err := s.keys.EncryptString(secret.PurposeUserToken, token)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "Something went wrong.")
		return
	}

	// Read the token this sign-in is about to replace, so it can be retired
	// once the new one is safely in place.
	supersededToken := ""
	if prior, err := s.store.UserByRemote(ctx, host, account.ID); err == nil {
		if old, err := s.keys.DecryptString(secret.PurposeUserToken, prior.AccessToken); err == nil && old != token {
			supersededToken = old
		}
	}

	// Gates apply to new accounts only; someone who already has one always
	// gets back in, however full or restricted the service currently is.
	if supersededToken == "" && !s.knownUser(ctx, host, account.ID) {
		if msg := s.signupRefusal(ctx, host); msg != "" {
			s.log.Info("signup refused", "host", host, "reason", msg)
			s.fail(w, r, http.StatusForbidden, msg)
			return
		}
	}

	user, err := s.store.UpsertUser(ctx, &store.User{
		Host:        host,
		RemoteID:    account.ID,
		Acct:        account.Acct,
		DisplayName: account.DisplayName,
		AvatarURL:   account.Avatar,
		AccessToken: sealedToken,
	})
	if err != nil {
		s.log.Error("upsert user", "error", err)
		s.fail(w, r, http.StatusInternalServerError, "Something went wrong.")
		return
	}

	// Signing in also refreshes the token held by the Mastodon destination for
	// this same account, so a re-authorisation repairs a revoked one.
	s.syncMastodonDestination(ctx, user, token)

	// Only now, with nothing still relying on it, retire the old token. The
	// instance would otherwise keep every token it has ever issued to this
	// account, each able to post, and signing in a few times would quietly
	// leave a pile of them behind.
	if supersededToken != "" {
		if err := s.mastodon.RevokeToken(ctx, host, app.ClientID, app.ClientSecret, supersededToken); err != nil {
			// Not fatal: the sign-in succeeded, and the stale token is the
			// instance's to expire. Worth knowing about, though.
			s.log.Warn("could not revoke the previous token", "host", host, "error", err)
		} else {
			s.log.Info("revoked the previous token", "host", host)
		}
	}

	if err := s.startSession(w, r, user.ID); err != nil {
		s.log.Error("start session", "error", err)
		s.fail(w, r, http.StatusInternalServerError, "Something went wrong.")
		return
	}
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

// syncMastodonDestination updates the stored token of the destination that
// points at the account just signed in with.
func (s *Server) syncMastodonDestination(ctx context.Context, user *store.User, token string) {
	dests, err := s.store.DestinationsByUser(ctx, user.ID)
	if err != nil {
		return
	}
	for _, d := range dests {
		if d.Kind != destination.KindMastodon {
			continue
		}
		var cfg destination.MastodonConfig
		if json.Unmarshal([]byte(d.Config), &cfg) != nil {
			continue
		}
		if cfg.Host != user.Host || cfg.Acct != user.Acct {
			continue
		}
		creds, err := json.Marshal(destination.MastodonCredentials{AccessToken: token})
		if err != nil {
			continue
		}
		sealed, err := s.keys.Encrypt(secret.PurposeDestination, creds)
		if err != nil {
			continue
		}
		d.Credentials = sealed
		if err := s.store.UpdateDestination(ctx, d); err != nil {
			s.log.Error("refresh destination token", "destination", d.ID, "error", err)
		}
	}
}

// handleLogoutConfirm asks before ending the session. Signing out is a POST
// so that it cannot be triggered by a link someone else controls, which means
// the confirmation has to be its own page rather than a browser dialog — there
// is no JavaScript here to put one up.
func (s *Server) handleLogoutConfirm(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "logout", page{Title: "Sign out"})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.endSession(w, r)
	redirect(w, r, "/", "")
}

// knownUser reports whether this identity already has an account.
func (s *Server) knownUser(ctx context.Context, host, remoteID string) bool {
	_, err := s.store.UserByRemote(ctx, host, remoteID)
	return err == nil
}

// signupRefusal returns the reason a new account cannot be created, or an
// empty string if it can.
//
// None of these look at what the instance says about the account. A hostile
// server can claim any age, any follower count, any history it likes, so
// filtering on those only inconveniences honest people. What it cannot fake is
// how many accounts it has already registered here.
func (s *Server) signupRefusal(ctx context.Context, host string) string {
	if s.cfg.InstanceBlocked(host) {
		return "Accounts on that server cannot sign up."
	}
	if s.cfg.MaxAccounts > 0 {
		total, err := s.store.CountUsers(ctx)
		if err != nil {
			s.log.Error("count users", "error", err)
			return "Sign-ups are unavailable right now. Try again later."
		}
		if total >= s.cfg.MaxAccounts {
			return "feedrepeater is full while it is in beta. Try again later."
		}
	}
	// Signup volume from one server is deliberately not capped. A post that
	// does well on a large instance produces exactly the same shape as a farm,
	// and refusing it would turn the best day into the worst one. The number is
	// logged instead, so a genuine flood can be answered with
	// FR_BLOCKED_INSTANCES rather than guessed at in advance.
	if recent, err := s.store.CountRecentUsersByHost(ctx, host, time.Now().UTC().Add(-24*time.Hour)); err == nil && recent >= 25 {
		s.log.Info("high signup volume from one instance", "host", host, "last_24h", recent)
	}
	return ""
}

// revokeUserToken best-effort invalidates a user's access token at their
// instance. Failure is logged, never surfaced: the caller is in the middle of
// something the user asked for, and an unreachable instance must not stop it.
func (s *Server) revokeUserToken(ctx context.Context, user *store.User) {
	token, err := s.keys.DecryptString(secret.PurposeUserToken, user.AccessToken)
	if err != nil {
		return
	}
	app, err := s.oauthApp(ctx, user.Host)
	if err != nil {
		s.log.Warn("no client credentials to revoke with", "host", user.Host, "error", err)
		return
	}
	if err := s.mastodon.RevokeToken(ctx, user.Host, app.ClientID, app.ClientSecret, token); err != nil {
		s.log.Warn("could not revoke token", "host", user.Host, "error", err)
	}
}

// pkceChallenge is the S256 challenge for a verifier.
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

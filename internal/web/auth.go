package web

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

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
	// Capped here as well as in requireUser: sign-in is the one form reachable
	// without a session, so it is the one an anonymous stranger can post to.
	limitForm(w, r)
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

	to, err := s.beginOAuth(ctx, w, store.OAuthState{Host: host, Purpose: store.OAuthLogin})
	if err != nil {
		if errors.Is(err, errInstanceUnreachable) {
			s.log.Info("register app", "host", host, "error", err)
			s.renderIndex(w, r, http.StatusBadGateway, "Could not reach that instance. Check the address and try again.")
			return
		}
		s.log.Error("start sign-in", "host", host, "error", err)
		s.fail(w, r, http.StatusInternalServerError, "Something went wrong.")
		return
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// beginOAuth starts an authorisation against st.Host and returns where to send
// the browser. What the flow is for, and for whom, is recorded with the state
// here so the callback never has to take either from a parameter.
//
// A host that cannot be asked for a client registration is errInstanceUnreachable,
// which is the one failure the person can do something about; anything else is
// this service's own.
func (s *Server) beginOAuth(ctx context.Context, w http.ResponseWriter, st store.OAuthState) (string, error) {
	app, err := s.oauthApp(ctx, st.Host)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errInstanceUnreachable, err)
	}

	// PKCE: the verifier stays here (encrypted), only its hash travels.
	verifier := secret.Token()
	challenge := pkceChallenge(verifier)
	sealed, err := s.keys.EncryptString(secret.PurposePKCEVerifier, verifier)
	if err != nil {
		return "", err
	}

	state := secret.Token()
	if err := s.store.PutOAuthState(ctx, secret.Hash(state), st, sealed, oauthTTL); err != nil {
		return "", fmt.Errorf("store oauth state: %w", err)
	}
	s.setOAuthCookie(w, state)

	return mastodon.AuthorizeURL(st.Host, app.ClientID, s.redirectURI(), state, challenge), nil
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

	// The host, and what the flow was for, come from what we recorded at the
	// start, never from the callback parameters, and the state is consumed so
	// it cannot be replayed.
	flow, sealedVerifier, err := s.store.TakeOAuthState(ctx, secret.Hash(state))
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "That sign-in link has expired. Try again.")
		return
	}
	host := flow.Host
	verifier, err := s.keys.DecryptString(secret.PurposePKCEVerifier, sealedVerifier)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "That sign-in could not be completed. Try again.")
		return
	}

	// A connect flow is refused before any token is minted when it cannot be
	// finished: by a different session than the one that started it, or
	// against an instance the operator has since blocked. A token issued and
	// then refused would have to be revoked, and a refusal that never asks for
	// one has nothing to leave behind.
	var connector *store.User
	if flow.Purpose == store.OAuthConnect {
		connector, _ = s.currentUser(r)
		if connector == nil || connector.ID != flow.UserID {
			s.fail(w, r, http.StatusForbidden, "That connection was started from a different account. Sign in with it and try again.")
			return
		}
		if s.cfg.InstanceBlocked(host) {
			s.fail(w, r, http.StatusForbidden, "Accounts on that server cannot be connected.")
			return
		}
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
		// The token is live and unusable here; hand it back rather than leave
		// it at the instance with nothing on record about it.
		if err := s.mastodon.RevokeToken(ctx, host, app.ClientID, app.ClientSecret, token); err != nil {
			s.log.Warn("could not revoke an unused token", "host", host, "error", err)
		}
		s.fail(w, r, http.StatusBadGateway, "That instance did not confirm the account. Try again.")
		return
	}

	// A connect flow ends here: the account authorised becomes a destination of
	// whoever started it, and nobody is signed in or out.
	if flow.Purpose == store.OAuthConnect {
		s.completeConnect(ctx, w, r, connector, flow, app, token, account)
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
	// Sessions elsewhere may hold a copy of this account with the token just
	// replaced. Drop them, so nothing decides anything from the old one.
	s.forgetUser(user.ID)

	// Signing in is also what connects this account to itself: the destination
	// is made here if it does not exist, and re-sealed against the new token if
	// it does, so re-authorising is the repair for one whose token was revoked.
	// Failing is not fatal to the sign-in — adding a feed tries again.
	_, retired, err := s.ensureMastodonDestination(ctx, user, token)
	if err != nil {
		s.log.Error("connect mastodon destination", "user", user.ID, "error", err)
	}

	// Only now, with nothing still relying on them, retire the old tokens. The
	// instance would otherwise keep every token it has ever issued to this
	// account, each able to post, and signing in a few times would quietly
	// leave a pile of them behind. The destination's previous token is usually
	// the same one and is revoked once; it differs only when this account was
	// also connected to itself through the connect flow.
	for _, old := range []string{supersededToken, retired} {
		if old == "" || old == token || (old == retired && old == supersededToken) {
			continue
		}
		if err := s.mastodon.RevokeToken(ctx, host, app.ClientID, app.ClientSecret, old); err != nil {
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

// completeConnect finishes a connect flow: the account just authorised at
// flow.Host becomes a Mastodon destination of user, who started it.
//
// The session and instance checks ran in handleCallback before the token was
// minted; they are repeated here only as the guard on a function that stores a
// token under an account. On any refusal the token just issued is handed
// straight back: nothing of it is stored, and leaving it live at the instance
// would be leaving a key under the mat for a door that was never opened.
//
// Connecting the account the person signed in with is allowed, and renews it:
// the new token becomes the sign-in token as well as the destination's, so the
// one it replaces can be retired without anything still pointing at it. A
// sign-in and a self-connect for one account finishing in the same instant can
// each retire the other's token; both rows then hold a revoked one until the
// next sign-in, which is the repair for every revoked token. Only the account
// itself can arrange that, so it is not guarded against.
func (s *Server) completeConnect(ctx context.Context, w http.ResponseWriter, r *http.Request, user *store.User, flow store.OAuthState, app *mastodon.App, token string, account *mastodon.Account) {
	revoke := func() {
		if err := s.mastodon.RevokeToken(ctx, flow.Host, app.ClientID, app.ClientSecret, token); err != nil {
			s.log.Warn("could not revoke an unused token", "host", flow.Host, "error", err)
		}
	}

	if user == nil || user.ID != flow.UserID || s.cfg.InstanceBlocked(flow.Host) {
		revoke()
		s.fail(w, r, http.StatusForbidden, "That connection could not be completed.")
		return
	}

	// Connecting the account the person signed in with renews it. The new
	// token becomes the sign-in token first, and the one it replaces is read
	// back from the store in the same transaction — not from the account on
	// the request, which may be a cached copy from before an earlier swap. It
	// is revoked below whatever happens to the destination, so a failure
	// further on cannot leave it live with nothing on record pointing at it.
	self := flow.Host == user.Host && account.ID == user.RemoteID
	var previousOwn string
	if self {
		sealed, err := s.keys.EncryptString(secret.PurposeUserToken, token)
		var prev []byte
		if err == nil {
			prev, err = s.store.SwapUserToken(ctx, user.ID, sealed)
		}
		if err != nil {
			revoke()
			s.log.Error("replace sign-in token", "user", user.ID, "error", err)
			s.fail(w, r, http.StatusInternalServerError, "Something went wrong.")
			return
		}
		if old, err := s.keys.DecryptString(secret.PurposeUserToken, prev); err == nil && old != token {
			previousOwn = old
		}
		// Every cached copy of this account now carries a token that is about
		// to be retired.
		s.forgetUser(user.ID)
	}

	d, retired, err := s.connectMastodonDestination(ctx, user, flow.Host, account, token)
	if self {
		// The destination's previous token is usually this same one, revoked
		// once; it differs only when the row had drifted from the account.
		if retired == previousOwn {
			retired = ""
		}
		if previousOwn != "" {
			if err := s.mastodon.RevokeToken(ctx, flow.Host, app.ClientID, app.ClientSecret, previousOwn); err != nil {
				s.log.Warn("could not revoke the previous token", "host", flow.Host, "error", err)
			}
		}
	}
	if err != nil {
		if !self {
			revoke()
		}
		if errors.Is(err, store.ErrDestinationLimit) {
			s.fail(w, r, http.StatusBadRequest, destinationLimitMessage)
			return
		}
		s.log.Error("connect mastodon destination", "user", user.ID, "host", flow.Host, "error", err)
		s.fail(w, r, http.StatusInternalServerError, "Something went wrong.")
		return
	}
	// An account connected a second time has a new token; the one it held is
	// retired the same way a sign-in retires its predecessor.
	if retired != "" {
		if err := s.mastodon.RevokeToken(ctx, flow.Host, app.ClientID, app.ClientSecret, retired); err != nil {
			s.log.Warn("could not revoke the previous token", "host", flow.Host, "error", err)
		}
	}
	s.log.Info("connected a mastodon account", "user", user.ID, "host", flow.Host, "self", self)
	redirect(w, r, "/destinations/"+strconv.FormatInt(d.ID, 10), "dest-added")
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

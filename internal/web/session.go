package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"feedrepeater.com/internal/secret"
	"feedrepeater.com/internal/store"
)

const (
	sessionCookie = "fr_session"
	oauthCookie   = "fr_oauth"
)

// The __Host- prefix makes a cookie unforgeable by a sibling subdomain: the
// browser refuses to accept one carrying a Domain attribute, so a compromised
// host under the same registrable domain cannot plant a session. It requires
// Secure and Path=/, which rules it out over plain HTTP, so the plain name is
// kept for local development.
func (s *Server) sessionCookieName() string {
	if s.cfg.Secure() {
		return "__Host-" + sessionCookie
	}
	return sessionCookie
}

func (s *Server) oauthCookieName() string {
	if s.cfg.Secure() {
		return "__Host-" + oauthCookie
	}
	return oauthCookie
}

type ctxKey int

const (
	ctxUser ctxKey = iota
	ctxSessionID
)

// startSession issues a new session cookie. The cookie value is random; only
// its hash is stored, so the database never holds anything that could be
// replayed as a login.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, userID int64) error {
	token := secret.Token()
	if err := s.store.CreateSession(r.Context(), secret.Hash(token), userID, SessionTTL); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     s.sessionCookieName(),
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.Secure(),
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(SessionTTL),
		MaxAge:   int(SessionTTL / time.Second),
	})
	return nil
}

func (s *Server) endSession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(s.sessionCookieName()); err == nil {
		_ = s.store.DeleteSession(r.Context(), secret.Hash(c.Value))
	}
	http.SetCookie(w, &http.Cookie{
		Name:     s.sessionCookieName(),
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.Secure(),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// currentUser resolves the signed-in user, if any.
func (s *Server) currentUser(r *http.Request) (*store.User, string) {
	c, err := r.Cookie(s.sessionCookieName())
	if err != nil || c.Value == "" {
		return nil, ""
	}
	id := secret.Hash(c.Value)
	user, err := s.store.SessionUser(r.Context(), id)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Error("look up session", "error", err)
		}
		return nil, ""
	}
	return user, id
}

// userFrom returns the user attached by requireUser.
func userFrom(r *http.Request) *store.User {
	u, _ := r.Context().Value(ctxUser).(*store.User)
	return u
}

func sessionIDFrom(r *http.Request) string {
	id, _ := r.Context().Value(ctxSessionID).(string)
	return id
}

// requireUser gates a handler behind a session and, for unsafe methods, checks
// both the request's origin and a session-bound CSRF token. Either check alone
// would do; running both means a gap in one is not a vulnerability.
func (s *Server) requireUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, sessionID := s.currentUser(r)
		if user == nil {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
			s.fail(w, r, http.StatusForbidden, "Your session has expired. Sign in again.")
			return
		}

		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if err := s.checkOrigin(r, false); err != nil {
				s.fail(w, r, http.StatusForbidden, "That request did not come from this site.")
				return
			}
			if err := r.ParseForm(); err != nil {
				s.fail(w, r, http.StatusBadRequest, "That form could not be read.")
				return
			}
			if !s.keys.ValidCSRF(sessionID, r.PostFormValue("csrf")) {
				s.fail(w, r, http.StatusForbidden, "That form has expired. Try again.")
				return
			}
		}

		ctx := context.WithValue(r.Context(), ctxUser, user)
		ctx = context.WithValue(ctx, ctxSessionID, sessionID)
		next(w, r.WithContext(ctx))
	}
}

// checkOrigin verifies that a state-changing request was issued by this site.
func (s *Server) checkOrigin(r *http.Request, requireOrigin bool) error {
	// Browsers that send it: this is the strongest signal available.
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		if site == "same-origin" || site == "none" {
			return nil
		}
		return errors.New("cross-site request")
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = r.Header.Get("Referer")
	}
	if origin == "" {
		// No origin information at all. Where a session-bound CSRF token is
		// also checked this is tolerable, but sign-in has no session yet and
		// therefore no token, so origin is the only control and must not fail
		// open. Every browser sends Origin on a cross-site POST.
		if requireOrigin {
			return errors.New("request carries no origin information")
		}
		return nil
	}
	u, err := url.Parse(origin)
	if err != nil {
		return err
	}
	if !strings.EqualFold(u.Host, s.cfg.BaseURL.Host) {
		return errors.New("cross-origin request")
	}
	return nil
}

// setOAuthCookie ties an in-flight authorisation to this browser.
func (s *Server) setOAuthCookie(w http.ResponseWriter, state string) {
	http.SetCookie(w, &http.Cookie{
		Name:  s.oauthCookieName(),
		Value: state,
		// Path is "/" rather than the callback alone because the __Host-
		// prefix requires it. The cookie is short-lived and cleared on use.
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.Secure(),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(oauthTTL / time.Second),
	})
}

func (s *Server) clearOAuthCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.oauthCookieName(),
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.Secure(),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

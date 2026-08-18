// Package web serves the user interface. It is server-rendered, form-driven,
// and ships no JavaScript.
package web

import (
	"context"
	"embed"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"feedrepeater.com/internal/config"
	"feedrepeater.com/internal/feed"
	"feedrepeater.com/internal/mastodon"
	"feedrepeater.com/internal/publisher"
	"feedrepeater.com/internal/safehttp"
	"feedrepeater.com/internal/secret"
	"feedrepeater.com/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// SessionTTL is how long a sign-in lasts.
const SessionTTL = 30 * 24 * time.Hour

// oauthTTL bounds how long an authorisation may stay in flight.
const oauthTTL = 10 * time.Minute

type Server struct {
	cfg      *config.Config
	store    *store.Store
	keys     *secret.Keyring
	http     *safehttp.Client
	mastodon *mastodon.Client
	fetcher  *feed.Fetcher
	pub      *publisher.Publisher
	log      *slog.Logger

	templates map[string]*template.Template

	loginLimit *limiter // per client address, for sign-in attempts
	writeLimit *limiter // per user, for actions that reach third parties

	startedAt time.Time

	// /stats is public and its counts are recomputed at most once per statsTTL.
	statsMu   sync.Mutex
	statsBody []byte
	statsAt   time.Time
}

type Deps struct {
	Config    *config.Config
	Store     *store.Store
	Keyring   *secret.Keyring
	HTTP      *safehttp.Client
	Mastodon  *mastodon.Client
	Fetcher   *feed.Fetcher
	Publisher *publisher.Publisher
	Logger    *slog.Logger
}

func NewServer(d Deps) (*Server, error) {
	s := &Server{
		cfg:        d.Config,
		store:      d.Store,
		keys:       d.Keyring,
		http:       d.HTTP,
		mastodon:   d.Mastodon,
		fetcher:    d.Fetcher,
		pub:        d.Publisher,
		log:        d.Logger,
		loginLimit: newLimiter(10, time.Hour),
		writeLimit: newLimiter(30, time.Hour),
		startedAt:  time.Now(),
	}
	if err := s.parseTemplates(); err != nil {
		return nil, err
	}
	return s, nil
}

// Handler builds the router with middleware applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", staticCache(http.FileServerFS(static))))

	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /stats", s.handleStats)
	mux.HandleFunc("GET /faq", s.handleFAQ)
	mux.HandleFunc("GET /terms", s.handleTerms)

	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("GET /auth/callback", s.handleCallback)
	mux.HandleFunc("GET /logout", s.requireUser(s.handleLogoutConfirm))
	mux.HandleFunc("POST /logout", s.requireUser(s.handleLogout))

	mux.HandleFunc("GET /dashboard", s.requireUser(s.handleDashboard))

	mux.HandleFunc("POST /feed", s.requireUser(s.handleFeedSave))
	mux.HandleFunc("POST /feed/pause", s.requireUser(s.handleFeedPause))
	mux.HandleFunc("POST /feed/refresh", s.requireUser(s.handleFeedRefresh))
	mux.HandleFunc("POST /feed/delete", s.requireUser(s.handleFeedDelete))
	mux.HandleFunc("POST /feed/destinations", s.requireUser(s.handleFeedRoutes))

	mux.HandleFunc("GET /destinations/new", s.requireUser(s.handleDestinationNew))
	mux.HandleFunc("POST /destinations", s.requireUser(s.handleDestinationCreate))
	mux.HandleFunc("GET /destinations/{id}", s.requireUser(s.handleDestinationEdit))
	mux.HandleFunc("POST /destinations/{id}", s.requireUser(s.handleDestinationUpdate))
	mux.HandleFunc("POST /destinations/{id}/test", s.requireUser(s.handleDestinationTest))
	mux.HandleFunc("POST /destinations/{id}/delete", s.requireUser(s.handleDestinationDelete))

	mux.HandleFunc("POST /deliveries/{id}/retry", s.requireUser(s.handleDeliveryRetry))

	mux.HandleFunc("GET /settings", s.requireUser(s.handleSettings))
	mux.HandleFunc("POST /settings", s.requireUser(s.handleSettingsSave))
	mux.HandleFunc("POST /settings/delete", s.requireUser(s.handleAccountDelete))

	return s.recoverPanic(s.securityHeaders(s.logRequests(mux)))
}

// Serve runs the HTTP server until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 16,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}

	// Bind before announcing, so a failure to take the port is reported as one
	// rather than following a line claiming the server is up.
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return err
	}
	s.log.Info("listening", "addr", ln.Addr().String(), "base_url", s.cfg.BaseURL.String())

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.store.DB().PingContext(ctx); err != nil {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("ok\n"))
}

func staticCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		next.ServeHTTP(w, r)
	})
}

// Package web serves the user interface. It is server-rendered, form-driven,
// and ships no JavaScript.
package web

import (
	"context"
	"embed"
	"fmt"
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
	assets    *assets

	// notify wakes the delivery loop when a handler queues something, so a
	// retry goes out at once instead of waiting for a timer.
	notify func()

	// sessions memoises session lookups. Resolving one is a query on every
	// authenticated request, and the answer only changes when somebody signs in
	// or out — see sessionCacheTTL.
	sessions sync.Map

	loginLimit *limiter // per client address, for sign-in attempts
	writeLimit *limiter // per user, for actions that reach third parties

	startedAt time.Time

	// /stats is public and its counts are recomputed at most once per statsTTL.
	statsMu      sync.Mutex
	statsBody    []byte
	statsAt      time.Time
	statsRefresh bool
	// statsWait is closed when the in-flight compute finishes, so a caller with
	// nothing worth serving can wait for it instead of starting a second one.
	statsWait chan struct{}

	// healthz is answered from a cached probe rather than a live one, so a
	// check every thirty seconds does not take a database connection with it.
	healthMu sync.Mutex
	healthAt time.Time
	healthOK bool
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
	// Notify wakes the delivery worker. Optional: a server built without one
	// still works, the queue is just drained on its own schedule.
	Notify func()
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
		notify:     d.Notify,
	}
	if s.notify == nil {
		s.notify = func() {}
	}

	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}
	if s.assets, err = loadAssets(static); err != nil {
		return nil, fmt.Errorf("load static assets: %w", err)
	}
	if err := s.parseTemplates(); err != nil {
		return nil, err
	}
	return s, nil
}

// Handler builds the router with middleware applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /static/", s.assets.handler())

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

	mux.HandleFunc("POST /feeds", s.requireUser(s.handleFeedSave))
	mux.HandleFunc("GET /feeds/{id}", s.requireUser(s.handleFeed))
	mux.HandleFunc("POST /feeds/{id}/destinations", s.requireUser(s.handleFeedRoutes))
	mux.HandleFunc("POST /feeds/{id}/pause", s.requireUser(s.handleFeedPause))
	mux.HandleFunc("POST /feeds/{id}/refresh", s.requireUser(s.handleFeedRefresh))
	mux.HandleFunc("POST /feeds/{id}/delete", s.requireUser(s.handleFeedDelete))

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

	// The session cache expires entries lazily, so one that is never asked
	// about again is never dropped. This is what actually collects them.
	go func() {
		t := time.NewTicker(sessionCacheTTL * 10)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.sweepSessions()
			}
		}
	}()

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

// healthTTL is how long a health probe is reused.
//
// The container health check runs every thirty seconds for the life of the
// process and each run took a connection from the writer pool — which is one
// connection wide, so it queued behind whatever was writing and everything else
// queued behind it. Probing at most once every few seconds answers the question
// the check is actually asking, which is whether the process is up and the file
// is reachable, not whether it is reachable at this exact instant.
const healthTTL = 5 * time.Second

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if !s.healthy(r.Context()) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.Write([]byte("ok\n"))
}

func (s *Server) healthy(ctx context.Context) bool {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	if time.Since(s.healthAt) < healthTTL {
		return s.healthOK
	}
	// WithoutCancel, because the caller's context is the wrong lifetime for
	// this. The container check runs with its own timeout, and a client that
	// hangs up cancels r.Context() — which would make Ping return
	// context.Canceled and latch a 503 into the cache for the next five
	// seconds, from a database that was never unhealthy. That is a plausible
	// way to earn a restart loop out of nothing.
	probe, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	ok := s.store.DB().PingContext(probe) == nil

	// Only a success is cached. A failure is the answer worth re-asking: it is
	// the state that changes on its own, and the state where being wrong for
	// five seconds costs the most.
	if ok {
		s.healthOK, s.healthAt = true, time.Now()
	} else {
		s.healthOK, s.healthAt = false, time.Time{}
	}
	return ok
}

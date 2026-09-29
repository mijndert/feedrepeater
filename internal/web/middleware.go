package web

import (
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// baseCSP is the policy for every page. There are no scripts anywhere, so
// almost everything is denied outright.
var baseCSP = []string{
	"default-src 'none'",
	"style-src 'self'",
	"img-src 'self'",
	"base-uri 'none'",
	"frame-ancestors 'none'",
}

// strictCSP forbids submitting a form anywhere but back to this site.
var strictCSP = strings.Join(slices.Concat(baseCSP, []string{"form-action 'self'"}), "; ")

// signInCSP relaxes form-action for the sign-in page only.
//
// Signing in means posting the instance name here and being redirected to that
// instance's authorisation page. Firefox and Safari apply form-action to the
// whole redirect chain, not just the initial target, so 'self' blocks the
// handoff outright: the browser silently refuses to navigate and the person is
// left staring at the form. The instance is chosen by the visitor and can be
// any host, so it cannot be enumerated; https: is the tightest expression of
// "somewhere else, over TLS". The page it applies to carries one form and no
// scripts, so there is nothing on it to abuse the allowance.
var signInCSP = strings.Join(slices.Concat(baseCSP, []string{"form-action 'self' https:"}), "; ")

// securityHeaders applies a policy suited to a site with no scripts at all.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// Handlers that render the sign-in form replace this before writing.
		h.Set("Content-Security-Policy", strictCSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		// The OAuth redirect goes to a server the user named; do not tell it
		// which page they came from.
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), interest-cohort=()")
		if s.cfg.Secure() {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic serving request", "path", r.URL.Path, "panic", rec)
				http.Error(w, "Something went wrong.", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// unlogged are paths whose requests say nothing and arrive constantly. Static
// assets are one line per file per uncached visitor, and the health check is one
// every thirty seconds forever. Failures still get a line: the filter is on the
// status, not on the path.
func unlogged(path string) bool {
	return strings.HasPrefix(path, "/static/") || path == "/healthz" || path == "/favicon.ico"
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		level := slog.LevelInfo
		switch {
		case sw.status >= 500:
			level = slog.LevelError
		case unlogged(r.URL.Path) && sw.status < 400:
			return
		}
		// The path is logged but never the query string, which can carry an
		// OAuth code.
		s.log.Log(r.Context(), level, "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration_ms", time.Since(start).Milliseconds())
	})
}

// clientIP is the address used for rate limiting. Caddy sets X-Forwarded-For and
// is the only thing that can reach this process, so the last entry is the one to
// trust; anything a client sent itself sits before it.
//
// This leans on deploy/Caddyfile setting that header from Cf-Connecting-IP and
// refusing anything that did not arrive through Cloudflare. Forwarding Caddy's
// own peer address instead puts every visitor behind one Cloudflare colo in a
// single rate-limit bucket, which ten sign-in attempts are enough to empty for
// everyone in that geography.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		last := strings.TrimSpace(parts[len(parts)-1])
		if ip := net.ParseIP(last); ip != nil {
			return ip.String()
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// maxFormBytes bounds a form body before anything parses it.
//
// No form this application serves comes close: the longest field is a post
// template, which validation stops at 2000 runes. The cap exists because the
// alternative is net/http's own 10 MB, and a 10 MB form field is not a
// submission — it is a way to make the server allocate. Every parser downstream
// then works on a bounded string, which is what keeps a per-field length check
// from being the only thing between an open signup and the process's memory.
const maxFormBytes = 64 << 10

// limitForm caps the body of a request that is about to be parsed as a form. Past
// the cap the read fails, and the caller reports the same "that form could not be
// read" as any other unparseable body.
func limitForm(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
}

// limiter is a fixed-window counter. It is per-process and resets on restart,
// which is fine for what it protects against: casual hammering, not a
// distributed attack.
// maxLimiterKeys bounds the tracking map. It is the only per-request
// allocation in the process that a stranger can grow.
const maxLimiterKeys = 10_000

type limiter struct {
	mu        sync.Mutex
	limit     int
	window    time.Duration
	seen      map[string]*window
	nextSweep time.Time
}

type window struct {
	count int
	until time.Time
}

func newLimiter(limit int, per time.Duration) *limiter {
	return &limiter{limit: limit, window: per, seen: map[string]*window{}}
}

// allow records an attempt and reports whether it is within the limit.
func (l *limiter) allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	// Sweep expired entries on a timer rather than only once the map is large,
	// so churning through source addresses cannot inflate it for a full window.
	if now.After(l.nextSweep) {
		for k, v := range l.seen {
			if now.After(v.until) {
				delete(l.seen, k)
			}
		}
		l.nextSweep = now.Add(l.window)
	}
	// An entry that already exists is answered without reference to the
	// ceiling: it is being counted, it does not grow the map, and refusing it
	// would mean the limiter stopped tracking exactly the keys it is holding.
	if w, ok := l.seen[key]; ok && !now.After(w.until) {
		if w.count >= l.limit {
			return false
		}
		w.count++
		return true
	}

	// A hard ceiling in case a burst arrives faster than entries expire.
	//
	// Evicting a live window is not an option: deleting an entry resets its
	// count, so at the ceiling — which is exactly where the limiter matters —
	// that hands whoever was evicted a fresh allowance. Failing to shrink is a
	// safe way to fail; failing to limit is not.
	//
	// So: sweep everything expired, and if the map is still full, every one of
	// those keys is a live window and there is no honest room to start
	// tracking another. Refuse rather than grow. The sweep is O(n) but only
	// reachable when full, and it leaves the map far below the ceiling, so the
	// cost is amortised across the window rather than paid per request as the
	// old whole-map scan for a single victim was.
	if len(l.seen) >= maxLimiterKeys {
		for k, v := range l.seen {
			if now.After(v.until) {
				delete(l.seen, k)
			}
		}
		l.nextSweep = now.Add(l.window)
		if len(l.seen) >= maxLimiterKeys {
			return false
		}
	}

	l.seen[key] = &window{count: 1, until: now.Add(l.window)}
	return true
}

package web

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"feedrepeater.com/internal/store"
)

// page carries what every template needs.
type page struct {
	Title  string
	User   *store.User
	CSRF   string
	Notice string
	Error  string
	// Nav names the header item to mark as current: "dashboard", "settings",
	// or empty on pages that are not in the header.
	Nav  string
	Data any
}

// navSection maps a path to the header item that should be marked current.
// Feed and destination pages are reached from the dashboard, so they mark it.
func navSection(path string) string {
	switch {
	case path == "/dashboard" || strings.HasPrefix(path, "/feed") || strings.HasPrefix(path, "/destinations"):
		return "dashboard"
	case strings.HasPrefix(path, "/settings"):
		return "settings"
	}
	return ""
}

// notices maps the codes used in redirect URLs to their messages. Redirects
// carry a code rather than the text itself, so no page ever renders a message
// that came from the query string.
var notices = map[string]string{
	"feed-saved":     "Feed saved.",
	"feed-found":     "That page linked to a feed, which is the one saved.",
	"feed-removed":   "Feed removed.",
	"feed-paused":    "Feed paused.",
	"feed-resumed":   "Feed resumed.",
	"refreshing":     "Checking the feed now.",
	"retrying":       "That post is queued to go out again.",
	"routes-saved":   "Destinations updated.",
	"dest-added":     "Destination added.",
	"dest-saved":     "Destination saved.",
	"dest-removed":   "Destination removed.",
	"test-sent":      "Test post sent.",
	"settings-saved": "Settings saved.",
	"signed-out":     "Signed out.",
	"account-gone":   "Account deleted.",
}

func (s *Server) parseTemplates() error {
	funcs := template.FuncMap{
		"asset":     s.assets.path,
		"ago":       ago,
		"due":       due,
		"localtime": localtime,
		"kindLabel": kindLabel,
		"host":      hostOf,
		"acct":      acct,
	}

	s.templates = map[string]*template.Template{}
	for _, name := range []string{"index", "dashboard", "destination_new", "destination_edit", "settings", "faq", "terms", "logout", "error"} {
		t, err := template.New("layout.html").Funcs(funcs).
			ParseFS(templateFS, "templates/layout.html", "templates/"+name+".html")
		if err != nil {
			return fmt.Errorf("parse template %s: %w", name, err)
		}
		s.templates[name] = t
	}
	return nil
}

// renderBufs reuses the scratch buffers pages are built in. A page is a few
// kilobytes and every request allocated one, grew it a handful of times as the
// template ran, and handed the whole thing to the collector.
var renderBufs = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// maxPooledBuf is the largest buffer worth keeping. One page is not going to be
// megabytes, and holding a grown outlier forever would trade an allocation for
// a permanent leak of the largest response ever rendered.
const maxPooledBuf = 128 << 10

// render writes a page. It renders into a buffer first so a template failure
// produces an error page rather than half a document with a 200 on it.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name string, p page) {
	t, ok := s.templates[name]
	if !ok {
		s.log.Error("unknown template", "name", name)
		http.Error(w, "Something went wrong.", http.StatusInternalServerError)
		return
	}
	if p.User == nil {
		p.User = userFrom(r)
	}
	if p.CSRF == "" {
		if id := sessionIDFrom(r); id != "" {
			p.CSRF = s.keys.CSRFToken(id)
		}
	}
	if p.Notice == "" {
		p.Notice = notices[r.URL.Query().Get("ok")]
	}
	if p.Nav == "" {
		p.Nav = navSection(r.URL.Path)
	}
	// The sign-in form has to be able to hand off to a Mastodon instance.
	if name == "index" {
		w.Header().Set("Content-Security-Policy", signInCSP)
	}

	buf := renderBufs.Get().(*bytes.Buffer)
	buf.Reset()
	defer func() {
		if buf.Cap() <= maxPooledBuf {
			renderBufs.Put(buf)
		}
	}()

	if err := t.ExecuteTemplate(buf, "layout.html", p); err != nil {
		s.log.Error("render template", "name", name, "error", err)
		http.Error(w, "Something went wrong.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

// fail renders a standalone error page.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, status int, message string) {
	s.render(w, r, status, "error", page{Title: "Error", Error: message})
}

// redirect sends the browser to path, optionally with a notice code.
func redirect(w http.ResponseWriter, r *http.Request, path, notice string) {
	if notice != "" {
		path += "?ok=" + url.QueryEscape(notice)
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}

// --- template helpers ------------------------------------------------------

func ago(t any) string {
	var v time.Time
	switch x := t.(type) {
	case time.Time:
		v = x
	case *time.Time:
		if x == nil {
			return "never"
		}
		v = *x
	default:
		return ""
	}
	if v.IsZero() {
		return "never"
	}
	d := time.Since(v)
	switch {
	case d < 0:
		return "in " + short(-d)
	case d < time.Minute:
		return "just now"
	default:
		return short(d) + " ago"
	}
}

// due describes when a feed is next checked.
//
// A time that has already passed is not overdue: the poll loop ticks every 30
// seconds and takes the feed on its next pass, so a feed is briefly past its
// scheduled time on every cycle. At a one minute interval that is half the
// time, which is why this says the check is coming rather than that it is late.
func due(t any) string {
	var v time.Time
	switch x := t.(type) {
	case time.Time:
		v = x
	case *time.Time:
		if x == nil {
			return ""
		}
		v = *x
	default:
		return ""
	}
	if v.IsZero() {
		return ""
	}
	if d := time.Until(v); d > 0 {
		return "next in " + short(d)
	}
	return "checking shortly"
}

func short(d time.Duration) string {
	switch {
	// Whole minutes alone turn anything under a minute into "0m", which reads
	// as a feed being checked constantly rather than one about to be checked.
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// localtime formats an absolute time in the account's own zone, naming the zone
// so a reader can tell which one they are looking at. An unset or unloadable
// name is UTC, which is what store.ParseLocation decides.
func localtime(zone string, t any) string {
	loc := store.ParseLocation(zone)
	switch x := t.(type) {
	case time.Time:
		if x.IsZero() {
			return ""
		}
		return x.In(loc).Format("2006-01-02 15:04 MST")
	case *time.Time:
		if x == nil {
			return ""
		}
		return localtime(zone, *x)
	}
	return ""
}

func kindLabel(kind string) string {
	switch kind {
	case "mastodon":
		return "Mastodon"
	case "bluesky":
		return "Bluesky"
	case "discord":
		return "Discord"
	case "slack":
		return "Slack"
	case "ntfy":
		return "ntfy"
	case "linkding":
		return "linkding"
	case "webhook":
		return "Webhook"
	}
	return kind
}

// acct renders a fully-qualified account name so a proxy in front of us leaves
// it alone.
//
// A Mastodon handle is indistinguishable from an email address to anything
// scanning for one, so Cloudflare's Email Address Obfuscation rewrites the
// acct@host part into a decode script and the words "[email protected]". Our
// own leading @ is not part of the match and survives, so the page ends up
// reading @[email protected]. These markers are how Cloudflare is asked to
// skip a span of HTML.
//
// The markers have to be emitted from here rather than typed into the template:
// html/template elides comments before they reach the output, so a literal
// <!--email_off--> in a template is silently dropped. The name is escaped for
// the same reason any other value would be — acct and host come from the
// instance, not from us.
func acct(s string) template.HTML {
	return template.HTML("<!--email_off-->" + template.HTMLEscapeString(s) + "<!--/email_off-->")
}

// hostOf shortens a URL to its host for display.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return strings.TrimPrefix(u.Host, "www.")
}

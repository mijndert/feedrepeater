package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"feedrepeater.com/internal/config"
	"feedrepeater.com/internal/destination"
	"feedrepeater.com/internal/feed"
	"feedrepeater.com/internal/mastodon"
	"feedrepeater.com/internal/publisher"
	"feedrepeater.com/internal/safehttp"
	"feedrepeater.com/internal/secret"
	"feedrepeater.com/internal/store"
)

type harness struct {
	server  *Server
	handler http.Handler
	store   *store.Store
	keys    *secret.Keyring
}

// allowPrivate lets a test point the server at an httptest server on loopback,
// which the SSRF guard blocks by default.
func allowPrivate(c *config.Config) { c.AllowPrivateNetworks = true }

func newHarness(t *testing.T, opts ...func(*config.Config)) *harness {
	t.Helper()

	base, _ := url.Parse("https://fr.test")
	cfg := &config.Config{
		Addr:            "127.0.0.1:0",
		BaseURL:         base,
		DBPath:          filepath.Join(t.TempDir(), "test.db"),
		MinPollInterval: 15 * time.Minute,
		MaxItemsPerPoll: 5,
		UserAgent:       "test",
	}
	for _, opt := range opts {
		opt(cfg)
	}
	keys, err := secret.NewKeyring(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	hc := safehttp.New(safehttp.Options{UserAgent: "test", AllowPrivate: cfg.AllowPrivateNetworks})
	md := mastodon.New(hc)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	srv, err := NewServer(Deps{
		Config: cfg, Store: st, Keyring: keys, HTTP: hc, Mastodon: md,
		Fetcher: feed.NewFetcher(hc), Publisher: publisher.New(st, keys, hc, md, log), Logger: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{server: srv, handler: srv.Handler(), store: st, keys: keys}
}

// signIn creates a user with a live session and returns its cookie and CSRF token.
func (h *harness) signIn(t *testing.T, acct string) (*store.User, *http.Cookie, string) {
	t.Helper()
	ctx := context.Background()
	user, err := h.store.UpsertUser(ctx, &store.User{
		Host: "example.social", RemoteID: acct + "-id", Acct: acct, AccessToken: []byte("x"),
	})
	if err != nil {
		t.Fatal(err)
	}
	token := secret.Token()
	if err := h.store.CreateSession(ctx, secret.Hash(token), user.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: h.server.sessionCookieName(), Value: token}
	return user, cookie, h.keys.CSRFToken(secret.Hash(token))
}

func (h *harness) do(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

func postForm(path string, values url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://fr.test")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	return req
}

// The next check is the one number on the dashboard that describes the
// schedule, so it has to be readable at every interval. Whole minutes alone
// rendered anything under a minute as "in 0m", which read as a feed being
// checked constantly.
func TestNextCheckIsDescribedInUnitsThatExist(t *testing.T) {
	now := time.Now()
	cases := []struct {
		in   time.Time
		want string
	}{
		{now.Add(45 * time.Second), "next in 44s"},
		{now.Add(13 * time.Minute), "next in 12m"},
		{now.Add(3 * time.Hour), "next in 2h"},
		{now.Add(-time.Second), "checking shortly"},
		{now.Add(-time.Hour), "checking shortly"},
		{time.Time{}, ""},
	}
	for _, c := range cases {
		if got := due(c.in); got != c.want {
			t.Errorf("due(%s) = %q, want %q", c.in.Format(time.TimeOnly), got, c.want)
		}
	}
	if got := due((*time.Time)(nil)); got != "" {
		t.Errorf("due(nil) = %q", got)
	}
}

func TestSignedOutAccessIsRefused(t *testing.T) {
	h := newHarness(t)

	rec := h.do(httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	if rec.Code != http.StatusSeeOther {
		t.Errorf("GET /dashboard signed out = %d, want 303", rec.Code)
	}

	rec = h.do(postForm("/feed", url.Values{"url": {"https://example.com/f.xml"}}))
	if rec.Code != http.StatusForbidden {
		t.Errorf("POST /feed signed out = %d, want 403", rec.Code)
	}
}

// The FAQ and terms are linked from every footer, so they have to render for a
// visitor with no session and keep the header nav for one who has.
func TestInformationPagesAreOpen(t *testing.T) {
	h := newHarness(t)

	for _, path := range []string{"/faq", "/terms"} {
		rec := h.do(httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s signed out = %d, want 200", path, rec.Code)
		}
		body := rec.Body.String()
		if strings.Contains(body, "href=\"/logout\"") {
			t.Errorf("%s shows the signed-in nav to a visitor", path)
		}
		if !strings.Contains(body, "href=\"/terms\"") || !strings.Contains(body, "href=\"/faq\"") {
			t.Errorf("%s is missing the footer links", path)
		}
	}

	_, cookie, _ := h.signIn(t, "alice")
	req := httptest.NewRequest(http.MethodGet, "/faq", nil)
	req.AddCookie(cookie)
	rec := h.do(req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /faq signed in = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "href=\"/dashboard\"") {
		t.Error("/faq drops the nav for a signed-in user")
	}
}

func TestStateChangingRequestsNeedACSRFToken(t *testing.T) {
	h := newHarness(t)
	_, cookie, csrf := h.signIn(t, "alice")

	req := postForm("/feed", url.Values{"url": {"https://example.com/f.xml"}})
	req.AddCookie(cookie)
	if rec := h.do(req); rec.Code != http.StatusForbidden {
		t.Errorf("POST without CSRF token = %d, want 403", rec.Code)
	}

	req = postForm("/feed", url.Values{"url": {"https://example.com/f.xml"}, "csrf": {"wrong"}})
	req.AddCookie(cookie)
	if rec := h.do(req); rec.Code != http.StatusForbidden {
		t.Errorf("POST with wrong CSRF token = %d, want 403", rec.Code)
	}

	// The correct token gets past the CSRF gate; the request then fails on the
	// unreachable feed, which is a different response.
	req = postForm("/feed", url.Values{"url": {"https://example.com/f.xml"}, "csrf": {csrf}})
	req.AddCookie(cookie)
	if rec := h.do(req); rec.Code == http.StatusForbidden {
		t.Error("valid CSRF token was rejected")
	}
}

// A CSRF token minted for one session must not work in another.
func TestCSRFTokenIsSessionBound(t *testing.T) {
	h := newHarness(t)
	_, aliceCookie, _ := h.signIn(t, "alice")
	_, _, mallorysCSRF := h.signIn(t, "mallory")

	req := postForm("/feed", url.Values{"url": {"https://example.com/f.xml"}, "csrf": {mallorysCSRF}})
	req.AddCookie(aliceCookie)
	if rec := h.do(req); rec.Code != http.StatusForbidden {
		t.Errorf("another session's CSRF token was accepted: %d", rec.Code)
	}
}

func TestCrossSiteRequestsAreRefused(t *testing.T) {
	h := newHarness(t)
	_, cookie, csrf := h.signIn(t, "alice")

	req := postForm("/feed", url.Values{"url": {"https://example.com/f.xml"}, "csrf": {csrf}})
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.AddCookie(cookie)
	if rec := h.do(req); rec.Code != http.StatusForbidden {
		t.Errorf("cross-site POST = %d, want 403", rec.Code)
	}

	req = postForm("/feed", url.Values{"url": {"https://example.com/f.xml"}, "csrf": {csrf}})
	req.Header.Del("Sec-Fetch-Site")
	req.Header.Set("Origin", "https://evil.example")
	req.AddCookie(cookie)
	if rec := h.do(req); rec.Code != http.StatusForbidden {
		t.Errorf("foreign Origin POST = %d, want 403", rec.Code)
	}
}

// The highest-value bug in a multi-tenant app: reaching another account's
// destination by id.
func TestDestinationRoutesAreOwnerScoped(t *testing.T) {
	h := newHarness(t)
	alice, _, _ := h.signIn(t, "alice")
	_, mallorysCookie, mallorysCSRF := h.signIn(t, "mallory")

	d := &store.Destination{UserID: alice.ID, Kind: "webhook", Label: "Alice's hook", Template: "{{title}}"}
	if err := h.store.CreateDestination(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(d.ID, 10)

	req := httptest.NewRequest(http.MethodGet, "/destinations/"+id, nil)
	req.AddCookie(mallorysCookie)
	if rec := h.do(req); rec.Code != http.StatusNotFound {
		t.Errorf("GET another user's destination = %d, want 404", rec.Code)
	}

	for _, path := range []string{"/destinations/" + id, "/destinations/" + id + "/delete", "/destinations/" + id + "/test"} {
		req := postForm(path, url.Values{"csrf": {mallorysCSRF}, "label": {"stolen"}})
		req.AddCookie(mallorysCookie)
		if rec := h.do(req); rec.Code != http.StatusNotFound {
			t.Errorf("POST %s as another user = %d, want 404", path, rec.Code)
		}
	}

	got, err := h.store.Destination(context.Background(), alice.ID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Label != "Alice's hook" {
		t.Errorf("destination was modified by another user: %q", got.Label)
	}
}

func TestSessionCookieFlags(t *testing.T) {
	h := newHarness(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	user, _, _ := h.signIn(t, "alice")
	if err := h.server.startSession(rec, req, user.ID); err != nil {
		t.Fatal(err)
	}

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("got %d cookies", len(cookies))
	}
	c := cookies[0]
	if !c.HttpOnly {
		t.Error("session cookie is not HttpOnly")
	}
	if !c.Secure {
		t.Error("session cookie is not Secure on an https base URL")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", c.SameSite)
	}
	if c.Path != "/" {
		t.Errorf("Path = %q", c.Path)
	}

	// The cookie value must not be what is stored server-side.
	var stored string
	if err := h.store.DB().QueryRow(`SELECT id FROM sessions ORDER BY rowid DESC LIMIT 1`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == c.Value {
		t.Error("session token is stored in plaintext")
	}
	if stored != secret.Hash(c.Value) {
		t.Error("stored session id is not the hash of the cookie")
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := newHarness(t)
	rec := h.do(httptest.NewRequest(http.MethodGet, "/", nil))

	want := map[string]string{
		"X-Content-Type-Options":    "nosniff",
		"X-Frame-Options":           "DENY",
		"Referrer-Policy":           "no-referrer",
		"Strict-Transport-Security": "max-age=31536000; includeSubDomains",
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	csp := rec.Header().Get("Content-Security-Policy")
	for _, directive := range []string{"default-src 'none'", "frame-ancestors 'none'", "form-action 'self'", "base-uri 'none'"} {
		if !strings.Contains(csp, directive) {
			t.Errorf("CSP missing %q: %s", directive, csp)
		}
	}
	if strings.Contains(csp, "script-src") && !strings.Contains(csp, "'none'") {
		t.Errorf("CSP allows scripts: %s", csp)
	}
}

// Notices come from a fixed table, so a message cannot be injected via the URL.
func TestNoticeTextCannotComeFromTheQueryString(t *testing.T) {
	h := newHarness(t)
	_, cookie, _ := h.signIn(t, "alice")

	req := httptest.NewRequest(http.MethodGet, "/dashboard?ok=<script>alert(1)</script>", nil)
	req.AddCookie(cookie)
	rec := h.do(req)
	if strings.Contains(rec.Body.String(), "alert(1)") {
		t.Error("query string text was rendered into the page")
	}
}

func TestUnknownDestinationIDsAreRejected(t *testing.T) {
	h := newHarness(t)
	_, cookie, _ := h.signIn(t, "alice")

	for _, id := range []string{"999999", "abc", "1'or'1", "-1"} {
		req := httptest.NewRequest(http.MethodGet, "/destinations/"+url.PathEscape(id), nil)
		req.AddCookie(cookie)
		if rec := h.do(req); rec.Code != http.StatusNotFound {
			t.Errorf("GET /destinations/%s = %d, want 404", id, rec.Code)
		}
	}
}

func TestExpiredSessionIsRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	user, err := h.store.UpsertUser(ctx, &store.User{
		Host: "example.social", RemoteID: "bob-id", Acct: "bob", AccessToken: []byte("x"),
	})
	if err != nil {
		t.Fatal(err)
	}
	token := secret.Token()
	if err := h.store.CreateSession(ctx, secret.Hash(token), user.ID, -time.Hour); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.AddCookie(&http.Cookie{Name: h.server.sessionCookieName(), Value: token})
	if rec := h.do(req); rec.Code != http.StatusSeeOther {
		t.Errorf("expired session reached /dashboard: %d", rec.Code)
	}
}

func TestHealthEndpoint(t *testing.T) {
	h := newHarness(t)
	rec := h.do(httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ok") {
		t.Errorf("healthz = %d %q", rec.Code, rec.Body.String())
	}
}

// Adding a feed must never replay its backlog. This drives the real handler
// against a feed that already has entries and asserts nothing is queued.
func TestAddingAFeedDoesNotPostItsBacklog(t *testing.T) {
	h := newHarness(t, allowPrivate)
	user, cookie, csrf := h.signIn(t, "alice")
	ctx := context.Background()

	if err := h.store.CreateDestination(ctx, &store.Destination{
		UserID: user.ID, Kind: "webhook", Label: "hook", Template: "{{title}}",
	}); err != nil {
		t.Fatal(err)
	}

	body := `<?xml version="1.0"?><rss version="2.0"><channel><title>Backlog</title>` +
		`<item><title>Old one</title><link>https://example.com/1</link><guid>g1</guid></item>` +
		`<item><title>Old two</title><link>https://example.com/2</link><guid>g2</guid></item>` +
		`<item><title>Old three</title><link>https://example.com/3</link><guid>g3</guid></item>` +
		`</channel></rss>`
	feedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Write([]byte(body))
	}))
	defer feedSrv.Close()

	req := postForm("/feed", url.Values{"url": {feedSrv.URL}, "csrf": {csrf}})
	req.AddCookie(cookie)
	if rec := h.do(req); rec.Code != http.StatusSeeOther {
		t.Fatalf("saving the feed = %d, want 303: %s", rec.Code, rec.Body.String())
	}

	if n, err := h.store.PendingCount(ctx, user.ID); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Errorf("adding a feed queued %d deliveries; the backlog would have been posted", n)
	}

	f, err := h.store.FeedByUser(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !f.Primed {
		t.Error("feed was not primed at save time, leaving a window where the backlog could post")
	}
	if f.Title != "Backlog" {
		t.Errorf("feed title = %q", f.Title)
	}

	// The backlog is recorded, so a later poll sees it as already seen.
	res, err := feed.Ingest(ctx, h.store, f.ID, user.ID, []feed.Entry{
		{GUID: "g1", Title: "Old one"}, {GUID: "g2", Title: "Old two"}, {GUID: "g3", Title: "Old three"},
	}, false, 5)
	if err != nil {
		t.Fatal(err)
	}
	if res.Queued != 0 {
		t.Errorf("a later poll queued %d of the backlog entries", res.Queued)
	}
}

func TestFeedRoutingThroughTheHandler(t *testing.T) {
	h := newHarness(t)
	user, cookie, csrf := h.signIn(t, "alice")
	ctx := context.Background()

	f, err := h.store.SetFeed(ctx, user.ID, "https://example.com/feed.xml")
	if err != nil {
		t.Fatal(err)
	}
	keep := &store.Destination{UserID: user.ID, Kind: "webhook", Label: "keep"}
	drop := &store.Destination{UserID: user.ID, Kind: "mastodon", Label: "drop"}
	for _, d := range []*store.Destination{keep, drop} {
		if err := h.store.CreateDestination(ctx, d); err != nil {
			t.Fatal(err)
		}
	}

	req := postForm("/feed/destinations", url.Values{
		"csrf":        {csrf},
		"destination": {strconv.FormatInt(keep.ID, 10)},
	})
	req.AddCookie(cookie)
	if rec := h.do(req); rec.Code != http.StatusSeeOther {
		t.Fatalf("saving routes = %d, want 303", rec.Code)
	}

	routes, err := h.store.FeedRoutes(ctx, user.ID, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0] != keep.ID {
		t.Errorf("routes = %v, want [%d]", routes, keep.ID)
	}

	// The dashboard reflects the choice.
	dash := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	dash.AddCookie(cookie)
	body := h.do(dash).Body.String()
	if !strings.Contains(body, `value="`+strconv.FormatInt(keep.ID, 10)+`" checked`) {
		t.Error("routed destination is not ticked on the dashboard")
	}
	if strings.Contains(body, `value="`+strconv.FormatInt(drop.ID, 10)+`" checked`) {
		t.Error("unrouted destination is ticked on the dashboard")
	}
}

// Routing is where an id from another account would do damage if it were
// trusted, so the handler is checked as well as the store.
func TestFeedRoutingRejectsAnotherAccountsDestination(t *testing.T) {
	h := newHarness(t)
	alice, _, _ := h.signIn(t, "alice")
	mallory, mallorysCookie, mallorysCSRF := h.signIn(t, "mallory")
	ctx := context.Background()

	victim := &store.Destination{UserID: alice.ID, Kind: "webhook", Label: "Alice's hook"}
	if err := h.store.CreateDestination(ctx, victim); err != nil {
		t.Fatal(err)
	}
	f, err := h.store.SetFeed(ctx, mallory.ID, "https://example.com/mallory.xml")
	if err != nil {
		t.Fatal(err)
	}

	req := postForm("/feed/destinations", url.Values{
		"csrf":        {mallorysCSRF},
		"destination": {strconv.FormatInt(victim.ID, 10)},
	})
	req.AddCookie(mallorysCookie)
	h.do(req)

	routes, err := h.store.FeedRoutes(ctx, mallory.ID, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 0 {
		t.Errorf("attached another account's destination: %v", routes)
	}
}

// Signing in means being redirected to an instance chosen by the visitor.
// Firefox and Safari apply form-action to the whole redirect chain, so a
// policy of 'self' silently blocks the handoff: the server logs a 303 and the
// browser refuses to navigate. The sign-in page must permit it.
func TestSignInPageAllowsTheOAuthHandoff(t *testing.T) {
	h := newHarness(t)

	rec := h.do(httptest.NewRequest(http.MethodGet, "/", nil))
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "form-action 'self' https:") {
		t.Errorf("sign-in page cannot hand off to an instance; form-action is %q", csp)
	}
	// The relaxation is limited to form-action.
	for _, directive := range []string{"default-src 'none'", "base-uri 'none'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, directive) {
			t.Errorf("sign-in CSP dropped %q: %s", directive, csp)
		}
	}
}

// Every other page keeps the strict policy.
func TestSignedInPagesKeepStrictFormAction(t *testing.T) {
	h := newHarness(t)
	_, cookie, _ := h.signIn(t, "alice")

	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.AddCookie(cookie)
	csp := h.do(req).Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "form-action 'self'") || strings.Contains(csp, "https:") {
		t.Errorf("dashboard should keep form-action 'self', got %q", csp)
	}
}

// An OAuth client is only reusable while it still matches this service's
// callback. A stored client from a different base URL would make every login
// fail at the instance, with only a 303 in these logs to show for it.
func TestStoredOAuthClientIsBoundToTheCallback(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	sealed, err := h.keys.EncryptString(secret.PurposeClientSecret, "old-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SaveInstance(ctx, &store.Instance{
		Host:         "example.social",
		ClientID:     "old-client",
		ClientSecret: sealed,
		RedirectURI:  "https://old.example/auth/callback",
	}); err != nil {
		t.Fatal(err)
	}

	// The stored client does not match the current callback, so it must not be
	// handed back. Registration then fails because the host is unreachable in
	// tests, which is the correct outcome: anything but reuse.
	if app, err := h.server.oauthApp(ctx, "example.social"); err == nil && app.ClientID == "old-client" {
		t.Error("reused a client registered for a different callback")
	}

	// A client that does match is reused without touching the network.
	if err := h.store.SaveInstance(ctx, &store.Instance{
		Host:         "example.social",
		ClientID:     "current-client",
		ClientSecret: sealed,
		RedirectURI:  h.server.redirectURI(),
	}); err != nil {
		t.Fatal(err)
	}
	app, err := h.server.oauthApp(ctx, "example.social")
	if err != nil {
		t.Fatalf("matching client was not reused: %v", err)
	}
	if app.ClientID != "current-client" || app.ClientSecret != "old-secret" {
		t.Errorf("got client %q", app.ClientID)
	}
}

// Over https the session cookie carries the __Host- prefix, which a browser
// will only accept without a Domain attribute. That is what stops a
// compromised sibling subdomain from planting a session.
func TestSessionCookieUsesHostPrefix(t *testing.T) {
	h := newHarness(t)
	user, _, _ := h.signIn(t, "alice")

	rec := httptest.NewRecorder()
	if err := h.server.startSession(rec, httptest.NewRequest(http.MethodGet, "/", nil), user.ID); err != nil {
		t.Fatal(err)
	}
	c := rec.Result().Cookies()[0]
	if c.Name != "__Host-fr_session" {
		t.Errorf("cookie name = %q, want __Host-fr_session", c.Name)
	}
	if c.Domain != "" {
		t.Errorf("__Host- cookie must not set Domain, got %q", c.Domain)
	}
	if c.Path != "/" || !c.Secure {
		t.Errorf("__Host- requires Path=/ and Secure, got path %q secure %v", c.Path, c.Secure)
	}
}

// Over plain http the prefix would be rejected by the browser, so local
// development keeps the plain name.
func TestSessionCookieDropsPrefixWithoutTLS(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		u, _ := url.Parse("http://localhost:8080")
		c.BaseURL = u
	})
	if got := h.server.sessionCookieName(); got != "fr_session" {
		t.Errorf("cookie name over http = %q, want fr_session", got)
	}
}

// Sign-in has no session and therefore no CSRF token, so the origin check is
// the only control and must not pass a request that carries no origin at all.
func TestLoginRefusesRequestsWithNoOrigin(t *testing.T) {
	h := newHarness(t)

	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("instance=mastodon.social"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// No Origin, no Referer, no Sec-Fetch-Site.
	if rec := h.do(req); rec.Code != http.StatusForbidden {
		t.Errorf("POST /login with no origin information = %d, want 403", rec.Code)
	}

	// A signed-in POST still tolerates it, because the CSRF token covers it.
	_, cookie, csrf := h.signIn(t, "alice")
	req = httptest.NewRequest(http.MethodPost, "/feed/pause",
		strings.NewReader(url.Values{"csrf": {csrf}, "paused": {"1"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	if rec := h.do(req); rec.Code == http.StatusForbidden {
		t.Error("a token-protected POST was refused for lacking origin headers")
	}
}

// Updating a destination re-resolves a handle and opens a session against a
// server the user named, so it must be rate limited like creating one.
func TestDestinationUpdateIsRateLimited(t *testing.T) {
	h := newHarness(t)
	user, cookie, csrf := h.signIn(t, "alice")
	ctx := context.Background()

	d := &store.Destination{UserID: user.ID, Kind: "webhook", Label: "hook", Template: "{{title}}"}
	if err := h.store.CreateDestination(ctx, d); err != nil {
		t.Fatal(err)
	}
	path := "/destinations/" + strconv.FormatInt(d.ID, 10)

	limited := false
	for range 60 {
		req := postForm(path, url.Values{"csrf": {csrf}, "label": {"x"}, "template": {"{{title}}"}})
		req.AddCookie(cookie)
		if h.do(req).Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Error("destination updates are not rate limited; each one can reach a third-party host")
	}
}

// Retrying a delivery is a write scoped to its owner, and a failed one has
// already used its attempts, so requeuing has to hand them back.
func TestFailedDeliveryCanBeRetriedByItsOwnerOnly(t *testing.T) {
	h := newHarness(t)
	alice, cookie, csrf := h.signIn(t, "alice")
	_, mallorysCookie, mallorysCSRF := h.signIn(t, "mallory")
	ctx := context.Background()

	f, err := h.store.SetFeed(ctx, alice.ID, "https://example.com/feed.xml")
	if err != nil {
		t.Fatal(err)
	}
	d := &store.Destination{UserID: alice.ID, Kind: "webhook", Label: "hook"}
	if err := h.store.CreateDestination(ctx, d); err != nil {
		t.Fatal(err)
	}
	item := &store.Item{FeedID: f.ID, GUID: "g1", Title: "An entry", URL: "https://example.com/1"}
	if _, err := h.store.InsertItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.QueueDeliveries(ctx, alice.ID, f.ID, item.ID); err != nil {
		t.Fatal(err)
	}
	views, err := h.store.RecentDeliveries(ctx, alice.ID, 1)
	if err != nil || len(views) != 1 {
		t.Fatalf("queue: %d deliveries, %v", len(views), err)
	}
	id := strconv.FormatInt(views[0].ID, 10)

	// A delivery still in flight is not something to retry.
	req := postForm("/deliveries/"+id+"/retry", url.Values{"csrf": {csrf}})
	req.AddCookie(cookie)
	if rec := h.do(req); rec.Code != http.StatusNotFound {
		t.Errorf("retrying a pending delivery = %d, want 404", rec.Code)
	}

	// Fail it the way the publisher would, using up every attempt.
	for range 6 {
		if err := h.store.MarkDeliveryFailed(ctx, views[0].ID, "the endpoint refused it"); err != nil {
			t.Fatal(err)
		}
	}

	// Another account cannot reach it.
	req = postForm("/deliveries/"+id+"/retry", url.Values{"csrf": {mallorysCSRF}})
	req.AddCookie(mallorysCookie)
	if rec := h.do(req); rec.Code != http.StatusNotFound {
		t.Errorf("retrying another account's delivery = %d, want 404", rec.Code)
	}

	req = postForm("/deliveries/"+id+"/retry", url.Values{"csrf": {csrf}})
	req.AddCookie(cookie)
	if rec := h.do(req); rec.Code != http.StatusSeeOther {
		t.Fatalf("retry = %d, want 303", rec.Code)
	}

	after, err := h.store.RecentDeliveries(ctx, alice.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if after[0].Status != "pending" {
		t.Errorf("status after retry = %q, want pending", after[0].Status)
	}
	// Without this the worker abandons it again on the first attempt.
	if after[0].Attempts != 0 {
		t.Errorf("attempts after retry = %d, want 0", after[0].Attempts)
	}
	if after[0].LastError != "" {
		t.Errorf("the old failure is still shown: %q", after[0].LastError)
	}
}

// For Discord and Slack the webhook URL is the whole authorisation. It is
// sealed like a password, and no page may hand it back: a screenshot of the
// edit form would otherwise be enough to post into someone's channel.
func TestChannelWebhookURLsAreNeverRenderedBack(t *testing.T) {
	h := newHarness(t)
	user, cookie, csrf := h.signIn(t, "alice")
	ctx := context.Background()

	const discordURL = "https://discord.com/api/webhooks/123456789/dISCORDtOKENvalue"
	const slackURL = "https://hooks.slack.com/services/T012AB/B034CD/slackTOKENvalue"

	discord := &store.Destination{UserID: user.ID, Kind: "discord", Label: "Team channel"}
	if err := h.server.setConfig(discord, destination.DiscordConfig{
		WebhookID: "123456789", ChannelID: "222", GuildID: "111", Name: "feed bot",
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.server.setCredentials(discord, destination.DiscordCredentials{URL: discordURL}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.CreateDestination(ctx, discord); err != nil {
		t.Fatal(err)
	}

	slack := &store.Destination{UserID: user.ID, Kind: "slack", Label: "Slack channel"}
	if err := h.server.setConfig(slack, destination.SlackConfig{Team: "T012AB", Hook: "B034CD"}); err != nil {
		t.Fatal(err)
	}
	if err := h.server.setCredentials(slack, destination.SlackCredentials{URL: slackURL}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.CreateDestination(ctx, slack); err != nil {
		t.Fatal(err)
	}

	for _, d := range []*store.Destination{discord, slack} {
		path := "/destinations/" + strconv.FormatInt(d.ID, 10)
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		body := h.do(req).Body.String()
		for _, leak := range []string{"dISCORDtOKENvalue", "slackTOKENvalue"} {
			if strings.Contains(body, leak) {
				t.Errorf("%s renders the stored webhook URL", path)
			}
		}

		// A rejected save must not echo the submitted one either.
		post := postForm(path, url.Values{"csrf": {csrf}, "url": {"https://example.com/not-a-webhook"}})
		post.AddCookie(cookie)
		rec := h.do(post)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST %s with a foreign host = %d, want 400", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "example.com/not-a-webhook") {
			t.Errorf("%s echoed the submitted URL back into the form", path)
		}
	}

	// The dashboard lists them without the credential too.
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.AddCookie(cookie)
	if body := h.do(req).Body.String(); strings.Contains(body, "slackTOKENvalue") || strings.Contains(body, "dISCORDtOKENvalue") {
		t.Error("the dashboard shows a stored webhook URL")
	}
}

// One destination per kind is what the dashboard is drawn from: a row per
// service, connected or not. The handlers have to hold that rule up, or the
// second destination of a kind would have no row to appear in.
func TestOneDestinationPerKind(t *testing.T) {
	h := newHarness(t)
	user, cookie, csrf := h.signIn(t, "alice")

	d := &store.Destination{UserID: user.ID, Kind: "webhook", Label: "Site hook"}
	if err := h.store.CreateDestination(context.Background(), d); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/destinations/new?kind=webhook", nil)
	req.AddCookie(cookie)
	rec := h.do(req)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("GET the form for a connected kind = %d, want 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/destinations/"+strconv.FormatInt(d.ID, 10) {
		t.Errorf("redirected to %q, want the destination that already exists", got)
	}

	// The form is still reachable by hand, so the create path has to refuse a
	// second one itself rather than leaning on the redirect above.
	post := postForm("/destinations", url.Values{
		"csrf": {csrf}, "kind": {"webhook"}, "url": {"https://example.com/hook"},
	})
	post.AddCookie(cookie)
	if rec := h.do(post); rec.Code != http.StatusBadRequest {
		t.Errorf("creating a second webhook = %d, want 400", rec.Code)
	}
	list, err := h.store.DestinationsByUser(context.Background(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Errorf("account holds %d webhooks, want 1", len(list))
	}
}

// The destinations list is the only route to a new destination, so a kind must
// show a button until the account has one and its connected state afterwards.
func TestConnectListMarksTheKindsInUse(t *testing.T) {
	h := newHarness(t)
	user, cookie, _ := h.signIn(t, "alice")

	get := func() string {
		req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
		req.AddCookie(cookie)
		return h.do(req).Body.String()
	}

	body := get()
	for _, kind := range []string{"mastodon", "bluesky", "discord", "slack", "webhook"} {
		if !strings.Contains(body, `href="/destinations/new?kind=`+kind+`"`) {
			t.Errorf("no connect button for %s", kind)
		}
	}
	if strings.Contains(body, "Connected") {
		t.Error("a kind reads as connected with no destinations")
	}

	d := &store.Destination{UserID: user.ID, Kind: "mastodon", Label: "@alice@example.social"}
	if err := h.store.CreateDestination(context.Background(), d); err != nil {
		t.Fatal(err)
	}

	body = get()
	if !strings.Contains(body, `class="item-state ok">Connected`) {
		t.Error("the connected kind is not marked")
	}
	// The way to a destination's settings has to be a control, not a link
	// hidden in the status line, or nobody finds the post template.
	if !strings.Contains(body, `<a class="button" href="/destinations/`+strconv.FormatInt(d.ID, 10)+`">Settings</a>`) {
		t.Error("a connected row has no Settings button")
	}
	if strings.Contains(body, `href="/destinations/new?kind=mastodon"`) {
		t.Error("a connected kind still offers a connect button")
	}
	if !strings.Contains(body, `href="/destinations/new?kind=bluesky"`) {
		t.Error("connecting one kind removed the button for another")
	}
}

// The dashboard's appearance depends on classes the template emits. These are
// cheap to break silently during a restyle, so they are asserted here.
func TestDashboardEmitsStyleHooks(t *testing.T) {
	h := newHarness(t)
	user, cookie, _ := h.signIn(t, "alice")
	ctx := context.Background()

	f, err := h.store.SetFeed(ctx, user.ID, "https://example.com/feed.xml")
	if err != nil {
		t.Fatal(err)
	}
	f.Primed = true
	if err := h.store.RecordFetch(ctx, f); err != nil {
		t.Fatal(err)
	}
	d := &store.Destination{UserID: user.ID, Kind: "mastodon", Label: "@alice@example.social"}
	if err := h.store.CreateDestination(ctx, d); err != nil {
		t.Fatal(err)
	}
	item := &store.Item{FeedID: f.ID, GUID: "g1", Title: "An entry", URL: "https://example.com/1"}
	if _, err := h.store.InsertItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.QueueDeliveries(ctx, user.ID, f.ID, item.ID); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.AddCookie(cookie)
	body := h.do(req).Body.String()

	for _, want := range []string{
		`class="feed-name"`, // feed title carries the display size
		`class="feed-url"`,  // address is set in mono
		`class="state"`,     // status renders as a dot and a word
		`class="items"`,     // destinations list
		`type="checkbox"`,   // still a real checkbox, not a stand-in
		`class="primary"`,   // exactly one filled button per form
		`class="events"`,    // activity is a list, not a table
		`pip-pending`,       // delivery state drives the pip colour
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard is missing %s", want)
		}
	}
	if strings.Contains(body, "<table") {
		t.Error("activity still renders as a table")
	}
}

// A link inside a <label> makes the whole row ambiguous: clicking the
// destination name would both follow the link and toggle the checkbox. The
// label must point at the checkbox by id and wrap only inert text.
func TestDestinationRowSeparatesLinkFromLabel(t *testing.T) {
	h := newHarness(t)
	user, cookie, _ := h.signIn(t, "alice")
	ctx := context.Background()

	if _, err := h.store.SetFeed(ctx, user.ID, "https://example.com/feed.xml"); err != nil {
		t.Fatal(err)
	}
	d := &store.Destination{UserID: user.ID, Kind: "webhook", Label: "Site hook"}
	if err := h.store.CreateDestination(ctx, d); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.AddCookie(cookie)
	body := h.do(req).Body.String()

	id := strconv.FormatInt(d.ID, 10)
	if !strings.Contains(body, `id="dest-`+id+`"`) {
		t.Error("checkbox has no id for its label to reference")
	}
	if !strings.Contains(body, `for="dest-`+id+`"`) {
		t.Error("label does not reference the checkbox")
	}
	// The anchor must not sit inside a label element.
	for _, chunk := range strings.Split(body, "<label") {
		if i := strings.Index(chunk, "</label>"); i >= 0 {
			if strings.Contains(chunk[:i], "<a ") {
				t.Error("a link is nested inside a label; clicking it would toggle the checkbox")
			}
		}
	}
}

// There is no way to edit a feed's address: the only path is remove and add.
// The endpoint has to enforce that itself, or a stale form would silently
// replace the feed and discard the record of what had already been posted.
func TestFeedCannotBeReplacedInPlace(t *testing.T) {
	h := newHarness(t, allowPrivate)
	user, cookie, csrf := h.signIn(t, "alice")
	ctx := context.Background()

	body := `<?xml version="1.0"?><rss version="2.0"><channel><title>First</title>` +
		`<item><title>One</title><guid>g1</guid></item></channel></rss>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Write([]byte(body))
	}))
	defer srv.Close()

	req := postForm("/feed", url.Values{"url": {srv.URL}, "csrf": {csrf}})
	req.AddCookie(cookie)
	if rec := h.do(req); rec.Code != http.StatusSeeOther {
		t.Fatalf("adding the first feed = %d, want 303", rec.Code)
	}

	// A second submission must be refused rather than swapping the feed.
	req = postForm("/feed", url.Values{"url": {srv.URL + "/other.xml"}, "csrf": {csrf}})
	req.AddCookie(cookie)
	rec := h.do(req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("replacing a feed in place = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Remove it before adding") {
		t.Error("the refusal does not say what to do instead")
	}

	f, err := h.store.FeedByUser(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if f.URL != srv.URL {
		t.Errorf("feed was replaced anyway: %q", f.URL)
	}

	// Removing it frees the slot again.
	req = postForm("/feed/delete", url.Values{"csrf": {csrf}})
	req.AddCookie(cookie)
	if rec := h.do(req); rec.Code != http.StatusSeeOther {
		t.Fatalf("removing the feed = %d", rec.Code)
	}
	req = postForm("/feed", url.Values{"url": {srv.URL}, "csrf": {csrf}})
	req.AddCookie(cookie)
	if rec := h.do(req); rec.Code != http.StatusSeeOther {
		t.Errorf("adding a feed after removal = %d, want 303", rec.Code)
	}
}

// Signing out asks first. The confirmation is a page rather than a browser
// dialog because there is no JavaScript, and the act itself stays a
// CSRF-protected POST.
func TestSignOutAsksBeforeEndingTheSession(t *testing.T) {
	h := newHarness(t)
	_, cookie, csrf := h.signIn(t, "alice")

	// The header link only shows the question.
	req := httptest.NewRequest(http.MethodGet, "/logout", nil)
	req.AddCookie(cookie)
	rec := h.do(req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /logout = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Sign out?") {
		t.Error("the confirmation page does not ask anything")
	}
	if !strings.Contains(body, `action="/logout"`) || !strings.Contains(body, `method="post"`) {
		t.Error("the confirmation page has no form to confirm with")
	}
	if !strings.Contains(body, `href="/dashboard"`) {
		t.Error("there is no way to back out")
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == h.server.sessionCookieName() && c.MaxAge < 0 {
			t.Fatal("merely visiting the confirmation page signed the user out")
		}
	}

	// The session is still live.
	req = httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.AddCookie(cookie)
	if rec := h.do(req); rec.Code != http.StatusOK {
		t.Fatalf("session ended by the confirmation page: /dashboard = %d", rec.Code)
	}

	// Confirming ends it.
	req = postForm("/logout", url.Values{"csrf": {csrf}})
	req.AddCookie(cookie)
	if rec := h.do(req); rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /logout = %d, want 303", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.AddCookie(cookie)
	if rec := h.do(req); rec.Code != http.StatusSeeOther {
		t.Error("the session survived a confirmed sign-out")
	}
}

// A GET must never end a session, or a link or prefetch could sign someone out.
func TestSignOutRequiresAPost(t *testing.T) {
	h := newHarness(t)
	_, cookie, _ := h.signIn(t, "alice")

	// Without a CSRF token the POST is refused outright.
	req := postForm("/logout", url.Values{})
	req.AddCookie(cookie)
	if rec := h.do(req); rec.Code != http.StatusForbidden {
		t.Errorf("POST /logout without a token = %d, want 403", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.AddCookie(cookie)
	if rec := h.do(req); rec.Code != http.StatusOK {
		t.Error("a refused sign-out ended the session anyway")
	}
}

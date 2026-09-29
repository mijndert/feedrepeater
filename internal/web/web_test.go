package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
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
		Fetcher: feed.NewFetcher(hc), Publisher: publisher.New(st, keys, md, log), Logger: log,
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
	// Sealed rather than a placeholder: the account's own token is what the
	// Mastodon destination is made from, so a harness that stores something
	// unreadable is a harness where nobody can add a feed.
	sealed, err := h.keys.EncryptString(secret.PurposeUserToken, "token-"+acct)
	if err != nil {
		t.Fatal(err)
	}
	user, err := h.store.UpsertUser(ctx, &store.User{
		Host: "example.social", RemoteID: acct + "-id", Acct: acct, AccessToken: sealed,
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

// /stats is served without a session, so the thing worth testing is not the
// arithmetic but the absence of anything identifying. A count is publishable; the
// handle, instance, feed URL or destination label behind it is not.
func TestStatsPublishesCountsAndNothingIdentifying(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	user, _, _ := h.signIn(t, "alice")
	f, _, err := h.store.Subscribe(ctx, user.ID, "https://private.example/secret-feed.xml", "", nil, store.FetchState{NextFetchAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetFeedTitle(ctx, f.ID, "Alice's Private Notes"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.CreateDestination(ctx, &store.Destination{
		UserID: user.ID, Kind: "mastodon", Label: "@alice@private.example",
		Credentials: []byte("super-secret-token"),
	}); err != nil {
		t.Fatal(err)
	}

	rec := h.do(httptest.NewRequest(http.MethodGet, "/stats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /stats = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	body := rec.Body.String()
	var got statsResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}

	if got.Users != 1 {
		t.Errorf("users = %d, want 1", got.Users)
	}
	if got.Feeds.Total != 1 || got.Feeds.Active != 1 || got.Feeds.Stopped != 0 {
		t.Errorf("feeds = %+v, want total 1 active 1 paused 0", got.Feeds)
	}
	if got.Destinations.Total != 1 || got.Destinations.ByKind["mastodon"] != 1 {
		t.Errorf("destinations = %+v, want total 1 with one mastodon", got.Destinations)
	}
	// The map is built from the registry rather than from what happens to be in
	// the table, so consumers see a stable shape as kinds come and go.
	if len(got.Destinations.ByKind) != 1 {
		t.Errorf("by_kind = %v, want one entry per supported kind", got.Destinations.ByKind)
	}
	if !got.Service.AcceptingSignups {
		t.Error("accepting_signups should be true with no cap configured")
	}

	for _, secret := range []string{
		"alice", "example.social", // handle and instance
		"private.example", "secret-feed", "Alice's Private Notes", // feed identity
		"my-private-channel", "super-secret-webhook", // destination identity
	} {
		if strings.Contains(body, secret) {
			t.Errorf("/stats leaked %q:\n%s", secret, body)
		}
	}
}

// A cap that has been reached has to show as closed, because that is the one
// thing a would-be signup wants to know before trying.
func TestStatsReportsSignupsClosedWhenFull(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.MaxAccounts = 1 })
	h.signIn(t, "alice")

	rec := h.do(httptest.NewRequest(http.MethodGet, "/stats", nil))
	var got statsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Service.AcceptingSignups {
		t.Error("accepting_signups should be false once MaxAccounts is reached")
	}
	// The cap itself is not published, only whether it has been hit.
	if strings.Contains(rec.Body.String(), "max_accounts") {
		t.Error("/stats should not publish the raw signup cap")
	}
}

// A Mastodon handle looks exactly like an email address, so a proxy scanning for
// addresses rewrites the acct@host half and leaves our leading @ in place,
// turning the handle into @[email protected] on the page. The opt-out
// markers that prevent it are HTML comments, and html/template elides comments,
// so they have to survive being rendered — a literal marker in a template is
// dropped without complaint, which is the regression this catches.
func TestAccountHandlesSurviveAnEmailAddressScanner(t *testing.T) {
	tmpl := template.Must(template.New("t").Funcs(template.FuncMap{"acct": acct}).
		Parse(`<dd>{{acct .}}</dd>`))

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, "@alice@mastodon.social"); err != nil {
		t.Fatalf("execute: %v", err)
	}
	got := buf.String()

	if want := "<!--email_off-->@alice@mastodon.social<!--/email_off-->"; !strings.Contains(got, want) {
		t.Errorf("rendered %q, want it to contain %q", got, want)
	}

	// The name reaches us from the instance, so returning HTML must not mean
	// returning it unescaped.
	buf.Reset()
	if err := tmpl.Execute(&buf, `@a<script>alert(1)</script>@evil.example`); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if strings.Contains(buf.String(), "<script>") {
		t.Errorf("handle was not escaped: %s", buf.String())
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

	// No destination is created here: adding the feed makes one, which is what
	// makes "the backlog is not posted" worth asserting at all.
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
	res, err := feed.Ingest(ctx, h.store, f.ID, []feed.Entry{
		{GUID: "g1", Title: "Old one"}, {GUID: "g2", Title: "Old two"}, {GUID: "g3", Title: "Old three"},
	}, false, 5)
	if err != nil {
		t.Fatal(err)
	}
	if res.Queued != 0 {
		t.Errorf("a later poll queued %d of the backlog entries", res.Queued)
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

	f, _, err := h.store.Subscribe(ctx, alice.ID, "https://example.com/feed.xml", "", nil, store.FetchState{NextFetchAt: time.Now().UTC()})
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
	if _, err := h.store.QueueDeliveries(ctx, f.ID, item.ID); err != nil {
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

// The dashboard's appearance depends on classes the template emits. These are
// cheap to break silently during a restyle, so they are asserted here.
func TestDashboardEmitsStyleHooks(t *testing.T) {
	h := newHarness(t)
	user, cookie, _ := h.signIn(t, "alice")
	ctx := context.Background()

	f, _, err := h.store.Subscribe(ctx, user.ID, "https://example.com/feed.xml", "", nil, store.FetchState{NextFetchAt: time.Now().UTC()})
	if err != nil {
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
	if _, err := h.store.QueueDeliveries(ctx, f.ID, item.ID); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	req.AddCookie(cookie)
	body := h.do(req).Body.String()

	for _, want := range []string{
		`class="feed-name"`, // feed title carries the display size
		`class="feed-url"`,  // address is set in mono
		`class="state"`,     // status renders as a dot and a word
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
	// The destination is not chosen, so nothing on this page asks about it. A
	// checkbox here would be one that ticks the only option there is.
	if strings.Contains(body, `type="checkbox"`) {
		t.Error("the dashboard still asks which destinations to publish to")
	}
	if strings.Contains(body, `action="/feed/destinations"`) {
		t.Error("the routing form is still rendered")
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

// Settings is the first page with preferences on it, so the round trip is worth
// asserting: what is saved comes back in the form, and what is rejected is shown
// back rather than discarded.
func TestSettingsSavePreferences(t *testing.T) {
	h := newHarness(t)
	user, cookie, csrf := h.signIn(t, "alice")
	ctx := context.Background()

	post := postForm("/settings", url.Values{
		"csrf":             {csrf},
		"timezone":         {"Europe/Amsterdam"},
		"default_template": {"{{title}} — {{feed_title}}\n{{url}}"},
	})
	post.AddCookie(cookie)
	if rec := h.do(post); rec.Code != http.StatusSeeOther {
		t.Fatalf("saving settings = %d, want 303: %s", rec.Code, rec.Body)
	}

	saved, err := h.store.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Timezone != "Europe/Amsterdam" {
		t.Errorf("stored timezone = %q", saved.Timezone)
	}
	if !strings.Contains(saved.DefaultTemplate, "{{feed_title}}") {
		t.Errorf("stored template = %q", saved.DefaultTemplate)
	}

	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	req.AddCookie(cookie)
	body := h.do(req).Body.String()
	if !strings.Contains(body, `value="Europe/Amsterdam"`) {
		t.Error("the saved timezone does not come back in the form")
	}
	// The preview is the only way to see what a template does before a
	// destination uses it.
	if !strings.Contains(body, `class="preview"`) {
		t.Error("the settings page shows no preview of the default template")
	}
}

func TestSettingsRejectsUnusableValues(t *testing.T) {
	h := newHarness(t)
	user, cookie, csrf := h.signIn(t, "alice")
	ctx := context.Background()

	cases := map[string]url.Values{
		"not a zone":        {"timezone": {"Mars/Olympus"}},
		"the server's zone": {"timezone": {"Local"}},
		"unknown variable":  {"default_template": {"{{title}} {{secret}}"}},
	}
	for name, values := range cases {
		values.Set("csrf", csrf)
		post := postForm("/settings", values)
		post.AddCookie(cookie)
		rec := h.do(post)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, rec.Code)
		}
		// A rejected form has to keep what was typed, or correcting one field
		// means retyping the other.
		for _, v := range values {
			if v[0] != csrf && !strings.Contains(rec.Body.String(), template.HTMLEscapeString(v[0])) {
				t.Errorf("%s: the submitted value %q was discarded", name, v[0])
			}
		}
	}

	saved, err := h.store.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Timezone != "" || saved.DefaultTemplate != "" {
		t.Errorf("a rejected form still wrote something: %+v", saved)
	}
}

// The front page says what it does with the account you sign in with, because
// that is now the whole of the answer: there is nothing to connect afterwards.
func TestFrontPageSaysWhereEntriesGo(t *testing.T) {
	h := newHarness(t)
	body := h.do(httptest.NewRequest(http.MethodGet, "/", nil)).Body.String()

	if !strings.Contains(body, "Mastodon") {
		t.Error("the front page does not name Mastodon")
	}
	// Services that were here and are not any more must not still be advertised
	// on the page somebody decides from.
	for _, gone := range []string{"Bluesky", "Discord", "Slack", "ntfy", "linkding", "Webhook"} {
		if strings.Contains(body, gone) {
			t.Errorf("the front page still offers %s", gone)
		}
	}
}

// Adding a feed connects the account to itself. Nobody picks a destination, so
// the feed has to arrive already publishing somewhere — and to the account that
// signed in, sealed with that account's own token.
func TestAddingAFeedConnectsTheSignedInAccount(t *testing.T) {
	h := newHarness(t, allowPrivate)
	user, cookie, csrf := h.signIn(t, "alice")
	ctx := context.Background()

	feedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Write([]byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>A feed</title></channel></rss>`))
	}))
	defer feedSrv.Close()

	req := postForm("/feed", url.Values{"url": {feedSrv.URL}, "csrf": {csrf}})
	req.AddCookie(cookie)
	if rec := h.do(req); rec.Code != http.StatusSeeOther {
		t.Fatalf("saving the feed = %d, want 303: %s", rec.Code, rec.Body.String())
	}

	list, err := h.store.DestinationsByUser(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("account holds %d destinations after adding a feed, want 1", len(list))
	}
	d := list[0]
	if d.Kind != destination.KindMastodon {
		t.Errorf("destination kind = %q", d.Kind)
	}

	var cfg destination.MastodonConfig
	if err := json.Unmarshal([]byte(d.Config), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Host != user.Host || cfg.Acct != user.Acct {
		t.Errorf("destination points at %+v, want the signed-in account", cfg)
	}
	if cfg.Visibility != "public" {
		t.Errorf("visibility = %q, want public", cfg.Visibility)
	}

	// The token it posts with is the one this account signed in with, sealed
	// under the destination's own key rather than copied in the clear.
	raw, err := h.keys.Decrypt(secret.PurposeDestination, d.Credentials)
	if err != nil {
		t.Fatal(err)
	}
	var creds destination.MastodonCredentials
	if err := json.Unmarshal(raw, &creds); err != nil {
		t.Fatal(err)
	}
	if creds.AccessToken != "token-alice" {
		t.Errorf("stored token = %q, want the account's own", creds.AccessToken)
	}

	// And the feed publishes to it, with nobody having ticked anything.
	f, err := h.store.FeedByUser(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	routes, err := h.store.FeedRoutes(ctx, user.ID, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0] != d.ID {
		t.Errorf("routes = %v, want [%d]; the feed would be polled and posted nowhere", routes, d.ID)
	}
}

// An account whose stored authorisation cannot be read has nothing to post
// with. The feed is refused rather than saved to be checked forever with
// nowhere to send what it finds.
func TestFeedIsRefusedWhenTheAccountCannotBeConnected(t *testing.T) {
	h := newHarness(t, allowPrivate)
	user, cookie, csrf := h.signIn(t, "alice")
	ctx := context.Background()

	user.AccessToken = []byte("not a sealed token")
	if _, err := h.store.UpsertUser(ctx, user); err != nil {
		t.Fatal(err)
	}

	feedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Write([]byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>A feed</title></channel></rss>`))
	}))
	defer feedSrv.Close()

	req := postForm("/feed", url.Values{"url": {feedSrv.URL}, "csrf": {csrf}})
	req.AddCookie(cookie)
	rec := h.do(req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("saving the feed = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Sign out") {
		t.Error("the error does not say what to do about it")
	}
	if _, err := h.store.FeedByUser(ctx, user.ID); !errors.Is(err, store.ErrNotFound) {
		t.Error("the feed was saved even though it had nowhere to publish")
	}
}

// Open signup means any account can post a form, so a form body is bounded
// before it is parsed. Without the cap net/http accepts 10 MB, and every
// per-field length check downstream is then the only thing standing between one
// account and the process's memory.
func TestOversizeFormIsRefusedBeforeItIsParsed(t *testing.T) {
	h := newHarness(t)
	_, cookie, csrf := h.signIn(t, "alice")

	big := strings.Repeat("a ", maxFormBytes) // 128 KB, twice the cap
	post := postForm("/settings", url.Values{"csrf": {csrf}, "default_template": {big}})
	post.AddCookie(cookie)
	if rec := h.do(post); rec.Code != http.StatusBadRequest {
		t.Errorf("an oversize form = %d, want 400", rec.Code)
	}

	// Sign-in is reachable without a session, so it is capped too.
	if rec := h.do(postForm("/login", url.Values{"instance": {big}})); rec.Code != http.StatusBadRequest {
		t.Errorf("an oversize sign-in form = %d, want 400", rec.Code)
	}

	// A form of ordinary size still goes through.
	ok := postForm("/settings", url.Values{"csrf": {csrf}, "timezone": {"Europe/Amsterdam"}})
	ok.AddCookie(cookie)
	if rec := h.do(ok); rec.Code != http.StatusSeeOther {
		t.Errorf("a normal form = %d, want 303: %s", rec.Code, rec.Body)
	}
}

// The timezone field is a dropdown, so the account's own zone has to be one of
// the options and has to be the selected one. The field was a text box that
// accepted any IANA name before it was a select, so an account can hold a zone
// the common list does not carry: a select that omits it has nothing to mark
// selected, the browser falls back to the first option, and saving any unrelated
// preference would silently move that account's dates.
func TestTimezoneDropdownKeepsTheAccountsOwnZone(t *testing.T) {
	h := newHarness(t)
	user, cookie, csrf := h.signIn(t, "alice")
	ctx := context.Background()

	// A zone that is deliberately not in commonZones.
	const unlisted = "Indian/Kerguelen"
	if slices.Contains(commonZones, unlisted) {
		t.Fatalf("%s is in commonZones; pick another for this test", unlisted)
	}
	if err := h.store.SetUserPreferences(ctx, user.ID, unlisted, ""); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	req.AddCookie(cookie)
	body := h.do(req).Body.String()

	if !strings.Contains(body, `<select id="timezone" name="timezone">`) {
		t.Error("the timezone field is not a dropdown")
	}
	if !strings.Contains(body, `<option value="`+unlisted+`" selected>`) {
		t.Errorf("the account's own zone is not carried as the selected option:\n%s", body)
	}
	// Saving without touching the field must not move the account.
	post := postForm("/settings", url.Values{
		"csrf":             {csrf},
		"timezone":         {unlisted},
		"default_template": {""},
	})
	post.AddCookie(cookie)
	if rec := h.do(post); rec.Code != http.StatusSeeOther {
		t.Fatalf("saving settings = %d, want 303: %s", rec.Code, rec.Body)
	}
	saved, err := h.store.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Timezone != unlisted {
		t.Errorf("stored timezone = %q, want %q", saved.Timezone, unlisted)
	}
}

// An account that has set no zone reads dates in UTC, and UTC is the first
// option, so the dropdown shows the right thing without an entry of its own.
func TestTimezoneDropdownDefaultsToUTC(t *testing.T) {
	h := newHarness(t)
	_, cookie, _ := h.signIn(t, "alice")

	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	req.AddCookie(cookie)
	body := h.do(req).Body.String()

	if !strings.Contains(body, `<option value="UTC">`) {
		t.Error("UTC is not offered")
	}
	if strings.Contains(body, ` selected>`) {
		t.Error("an account with no zone set should have nothing marked selected, leaving UTC first")
	}
}

// At its ceiling the limiter must refuse rather than make room by deleting a
// live window — deleting one resets its count, which hands the evicted key a
// fresh allowance exactly when the limiter is under the most pressure.
func TestLimiterCeilingRefusesRatherThanForgetting(t *testing.T) {
	l := newLimiter(2, time.Hour)

	// Fill it with live windows.
	for i := range maxLimiterKeys {
		if !l.allow(fmt.Sprintf("k%d", i)) {
			t.Fatalf("key %d refused while filling", i)
		}
	}

	// A brand-new key finds no room and is refused, rather than displacing one.
	if l.allow("newcomer") {
		t.Error("limiter admitted a new key past its ceiling")
	}
	if len(l.seen) > maxLimiterKeys {
		t.Errorf("map grew past the ceiling to %d", len(l.seen))
	}

	// Keys already being tracked keep being counted correctly — refusing the
	// newcomer must not have disturbed anyone's window.
	if !l.allow("k0") {
		t.Error("second request for a tracked key was refused")
	}
	if l.allow("k0") {
		t.Error("a tracked key exceeded its limit without being refused")
	}
}

// Once entries expire the ceiling clears itself, so a burst does not wedge the
// limiter shut for the rest of the process's life.
func TestLimiterCeilingRecoversAsWindowsExpire(t *testing.T) {
	l := newLimiter(2, time.Hour)
	for i := range maxLimiterKeys {
		l.allow(fmt.Sprintf("k%d", i))
	}
	if l.allow("newcomer") {
		t.Fatal("precondition: expected the limiter to be full")
	}

	// Age everything out.
	past := time.Now().Add(-time.Minute)
	for _, w := range l.seen {
		w.until = past
	}
	if !l.allow("newcomer") {
		t.Error("limiter stayed shut after its windows expired")
	}
	if len(l.seen) >= maxLimiterKeys {
		t.Errorf("expired entries were not swept: %d remain", len(l.seen))
	}
}

// --- activity --------------------------------------------------------------

func deliveryRow(id, item int64, status string, updated time.Time) *store.DeliveryView {
	v := &store.DeliveryView{}
	v.ID, v.ItemID, v.Status, v.UpdatedAt = id, item, status, updated
	return v
}

// One entry posted to several destinations is one line, however many rows the
// deliveries table holds for it.
func TestGroupActivityFoldsDeliveriesIntoEntries(t *testing.T) {
	now := time.Now().UTC()
	rows := []*store.DeliveryView{
		deliveryRow(4, 2, "pending", now),
		deliveryRow(3, 2, "sent", now.Add(-time.Minute)),
		deliveryRow(2, 1, "failed", now.Add(-time.Hour)),
		deliveryRow(1, 1, "sent", now.Add(-2*time.Hour)),
	}

	entries, more := groupActivity(rows, false)
	if more {
		t.Error("a short read reported truncated history")
	}
	if len(entries) != 2 {
		t.Fatalf("grouped %d entries, want 2", len(entries))
	}
	if entries[0].ItemID != 2 || len(entries[0].Deliveries) != 2 {
		t.Errorf("first entry is item %d with %d deliveries, want item 2 with 2",
			entries[0].ItemID, len(entries[0].Deliveries))
	}
	// The line's own state is the worst of the deliveries under it, and its
	// time is the most recent attempt among them rather than the oldest row.
	if got := entries[0].Status(); got != "pending" {
		t.Errorf("entry with a pending delivery reads %q, want pending", got)
	}
	if got := entries[1].Status(); got != "failed" {
		t.Errorf("entry with a failed delivery reads %q, want failed", got)
	}
	if !entries[0].UpdatedAt.Equal(now) {
		t.Errorf("entry time is %v, want the newest attempt %v", entries[0].UpdatedAt, now)
	}
}

// A read that hit its row limit may have cut its oldest entry in half, so that
// entry is dropped rather than shown as having gone to fewer destinations than
// it did.
func TestGroupActivityDropsThePossiblyIncompleteTail(t *testing.T) {
	now := time.Now().UTC()
	rows := []*store.DeliveryView{
		deliveryRow(3, 2, "sent", now),
		deliveryRow(2, 1, "sent", now.Add(-time.Hour)),
	}
	entries, more := groupActivity(rows, true)
	if len(entries) != 1 || entries[0].ItemID != 2 {
		t.Fatalf("kept %d entries, want only the complete one", len(entries))
	}
	if !more {
		t.Error("truncated history was not reported as truncated")
	}
}

// The cap counts entries, so a busy feed on a well-connected account cannot
// push everything else off the list with one afternoon's deliveries.
func TestGroupActivityCapsEntriesNotDeliveries(t *testing.T) {
	now := time.Now().UTC()
	var rows []*store.DeliveryView
	id := int64(activityRows)
	for item := int64(1); item <= int64(activityEntries)+3; item++ {
		for range 3 {
			rows = append(rows, deliveryRow(id, item, "sent", now))
			id--
		}
	}
	entries, more := groupActivity(rows, false)
	if len(entries) != activityEntries {
		t.Errorf("kept %d entries, want the cap of %d", len(entries), activityEntries)
	}
	if !more {
		t.Error("a capped list did not report there was more")
	}
}

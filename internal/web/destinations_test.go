package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"feedrepeater.com/internal/config"
	"feedrepeater.com/internal/destination"
	"feedrepeater.com/internal/mastodon"
	"feedrepeater.com/internal/secret"
	"feedrepeater.com/internal/store"
)

// Every destination route is addressed by id and scoped to its owner, so
// another account's id is a 404 rather than a lever. This is the IDOR that
// per-id routes introduce, so every verb is checked.
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

// A Discord or Slack webhook URL is the whole authorisation for the channel, so
// it is sealed like a password and never rendered back — not on the edit form,
// not on the dashboard, and not into a rejected form either.
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
		rec := h.do(req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, rec.Code)
		}
		body := rec.Body.String()
		for _, leak := range []string{"dISCORDtOKENvalue", "slackTOKENvalue"} {
			if strings.Contains(body, leak) {
				t.Errorf("%s renders the stored webhook URL", path)
			}
		}

		// A rejected save must not echo the submitted one either. The host is
		// wrong, so it is refused before anything is asked of the network.
		post := postForm(path, url.Values{"csrf": {csrf}, "url": {"https://example.com/not-a-webhook"}})
		post.AddCookie(cookie)
		rec = h.do(post)
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

// The account's default template is the starting point for a new destination,
// and only the starting point: it is copied at creation, so editing the default
// afterwards must not rewrite what an existing destination posts.
func TestNewDestinationStartsFromTheAccountDefault(t *testing.T) {
	h := newHarness(t)
	user, cookie, _ := h.signIn(t, "alice")
	ctx := context.Background()

	const tmpl = "New from {{feed_title}}: {{title}}"
	if err := h.store.SetUserPreferences(ctx, user.ID, "", tmpl); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/destinations/new?kind=webhook", nil)
	req.AddCookie(cookie)
	body := h.do(req).Body.String()
	if !strings.Contains(body, template.HTMLEscapeString(tmpl)) {
		t.Error("the form does not start from the account's default template")
	}

	d := &store.Destination{UserID: user.ID, Kind: "webhook", Label: "Hook", Template: "old text {{url}}"}
	if err := h.store.CreateDestination(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetUserPreferences(ctx, user.ID, "", "changed {{title}}"); err != nil {
		t.Fatal(err)
	}
	again, err := h.store.Destination(ctx, user.ID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.Template != "old text {{url}}" {
		t.Errorf("an existing destination's template changed to %q", again.Template)
	}
}

// An ntfy access token is a credential like a Bluesky app password: stored
// sealed, never rendered back, and kept when an edit leaves the field empty.
func TestNtfyTokenIsKeptButNeverShown(t *testing.T) {
	h := newHarness(t)
	user, cookie, csrf := h.signIn(t, "alice")
	ctx := context.Background()

	const token = "tk_nTFYtOKENvalue"
	d := &store.Destination{UserID: user.ID, Kind: "ntfy", Label: "Phone"}
	if err := h.server.setConfig(d, destination.NtfyConfig{
		Server: destination.DefaultNtfyServer, Topic: "my-topic", Priority: 3,
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.server.setCredentials(d, destination.NtfyCredentials{Token: token}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.CreateDestination(ctx, d); err != nil {
		t.Fatal(err)
	}
	path := "/destinations/" + strconv.FormatInt(d.ID, 10)

	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(cookie)
	body := h.do(req).Body.String()
	if strings.Contains(body, token) {
		t.Error("the edit form renders the stored access token")
	}
	// The topic is not a credential, so unlike a channel webhook it is shown.
	if !strings.Contains(body, `value="my-topic"`) {
		t.Error("the topic is not shown back, so it cannot be corrected")
	}

	// A save that changes only the template must keep the token and must not
	// publish a notification to prove anything — the server it would ask is
	// not reachable from here, so a verification would fail this request.
	post := postForm(path, url.Values{
		"csrf": {csrf}, "label": {"Phone"}, "template": {"{{title}}"},
		"server": {destination.DefaultNtfyServer}, "topic": {"my-topic"}, "priority": {"3"},
	})
	post.AddCookie(cookie)
	if rec := h.do(post); rec.Code != http.StatusSeeOther {
		t.Fatalf("saving a template change = %d, want 303: %s", rec.Code, rec.Body)
	}
	saved, err := h.store.Destination(ctx, user.ID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.server.storedNtfyToken(saved); got != token {
		t.Errorf("stored token after an unrelated edit = %q, want it unchanged", got)
	}
}

// The connect form is the only route to a new destination, so each kind's own
// fields have to be on it. ntfy's are a topic, a server and a priority.
func TestNtfyConnectFormOffersItsOwnFields(t *testing.T) {
	h := newHarness(t)
	_, cookie, _ := h.signIn(t, "alice")

	req := httptest.NewRequest(http.MethodGet, "/destinations/new?kind=ntfy", nil)
	req.AddCookie(cookie)
	rec := h.do(req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET the ntfy form = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`name="topic"`,
		`name="server"`,
		`value="` + destination.DefaultNtfyServer + `"`, // the hosted service is the default
		`type="password" id="token"`,                    // the token is entered like a password
		`name="priority"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the ntfy form is missing %s", want)
		}
	}
	// A feed should not decide on its own to buzz a phone at max priority.
	if !strings.Contains(body, `<option value="3" selected>default</option>`) {
		t.Error("the form does not start at ntfy's own default priority")
	}
}

// linkding's own fields are an address, an API token, tags and the unread flag.
// The address has no default, unlike ntfy's: a linkding is somebody's own server
// and there is no hosted one to guess at.
func TestLinkdingConnectFormOffersItsOwnFields(t *testing.T) {
	h := newHarness(t)
	_, cookie, _ := h.signIn(t, "alice")

	req := httptest.NewRequest(http.MethodGet, "/destinations/new?kind=linkding", nil)
	req.AddCookie(cookie)
	rec := h.do(req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET the linkding form = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`name="server"`,
		`type="password" id="token"`, // the API token is entered like a password
		`name="tags"`,
		`name="unread"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the linkding form is missing %s", want)
		}
	}
	// ntfy's default server must not leak across into a field about a different
	// service, which is what a single shared default would do.
	if strings.Contains(body, destination.DefaultNtfyServer) {
		t.Error("the linkding form is pre-filled with ntfy's server")
	}
	// A feed arriving as a bookmark is a reading list, so it starts unread.
	if !strings.Contains(body, `name="unread" value="1" checked`) {
		t.Error("the form does not start with unread ticked")
	}
}

// A linkding API token is a credential like a Bluesky app password: stored
// sealed, never rendered back, and kept when an edit leaves the field empty.
func TestLinkdingTokenIsKeptButNeverShown(t *testing.T) {
	h := newHarness(t)
	user, cookie, csrf := h.signIn(t, "alice")
	ctx := context.Background()

	const token = "tk_lINKDINGtOKENvalue"
	d := &store.Destination{UserID: user.ID, Kind: "linkding", Label: "Bookmarks"}
	if err := h.server.setConfig(d, destination.LinkdingConfig{
		Server: "https://linkding.example", Tags: []string{"feeds"}, Unread: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.server.setCredentials(d, destination.LinkdingCredentials{Token: token}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.CreateDestination(ctx, d); err != nil {
		t.Fatal(err)
	}
	path := "/destinations/" + strconv.FormatInt(d.ID, 10)

	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(cookie)
	body := h.do(req).Body.String()
	if strings.Contains(body, token) {
		t.Error("the edit form renders the stored API token")
	}
	// The address and the tags are not credentials, so they are shown back and
	// can be corrected.
	if !strings.Contains(body, `value="https://linkding.example"`) {
		t.Error("the address is not shown back, so it cannot be corrected")
	}
	if !strings.Contains(body, `value="feeds"`) {
		t.Error("the tags are not shown back, so editing them means retyping them")
	}

	// A save that changes only the template keeps the token, and asks linkding
	// nothing: the address it would ask is a server that does not exist here, so
	// a verification on every save would fail this request.
	post := postForm(path, url.Values{
		"csrf": {csrf}, "label": {"Bookmarks"}, "template": {"{{summary}}"},
		"server": {"https://linkding.example"}, "tags": {"feeds"}, "unread": {"1"},
	})
	post.AddCookie(cookie)
	if rec := h.do(post); rec.Code != http.StatusSeeOther {
		t.Fatalf("saving a template change = %d, want 303: %s", rec.Code, rec.Body)
	}
	saved, err := h.store.Destination(ctx, user.ID, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.server.storedLinkdingToken(saved); got != token {
		t.Errorf("stored token after an unrelated edit = %q, want it unchanged", got)
	}
}

// --- connecting a further Mastodon account ---------------------------------

// Connecting a Mastodon account is an OAuth flow, not a form: the page asks for
// an instance and hands the browser to it. Firefox and Safari apply form-action
// to the whole redirect chain, so the page needs the same relaxed policy the
// sign-in page has, and a rejected submission — which re-renders that same form
// — needs it too, or the second attempt is the one that silently fails.
func TestMastodonConnectFormHandsOffToTheInstance(t *testing.T) {
	h := newHarness(t)
	_, cookie, csrf := h.signIn(t, "alice")

	req := httptest.NewRequest(http.MethodGet, "/destinations/new?kind=mastodon", nil)
	req.AddCookie(cookie)
	rec := h.do(req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET the mastodon connect form = %d, want 200", rec.Code)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "form-action 'self' https:") {
		t.Errorf("connect form cannot hand off to an instance; form-action is %q", csp)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `name="instance"`) {
		t.Error("the form does not ask which instance")
	}
	// Nothing is decided here that the instance decides later.
	for _, field := range []string{`name="label"`, `name="template"`} {
		if strings.Contains(body, field) {
			t.Errorf("the connect form asks for %s before the account is known", field)
		}
	}

	// An instance that cannot be reached is a 400 with the form back, so the
	// address can be corrected, and nothing is left half-started.
	post := postForm("/destinations", url.Values{"csrf": {csrf}, "kind": {"mastodon"}, "instance": {"unreachable.invalid"}})
	post.AddCookie(cookie)
	rec = h.do(post)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("connecting an unreachable instance = %d, want 400: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `name="instance"`) || !strings.Contains(rec.Body.String(), `value="unreachable.invalid"`) {
		t.Error("the rejected form does not come back with the instance typed into it")
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "form-action 'self' https:") {
		t.Errorf("the re-rendered connect form lost the handoff policy: %q", csp)
	}
	var states int
	if err := h.store.DB().QueryRow(`SELECT count(*) FROM oauth_states`).Scan(&states); err != nil {
		t.Fatal(err)
	}
	if states != 0 {
		t.Errorf("%d oauth states recorded for a flow that never started", states)
	}
}

// An instance the operator has refused cannot be connected as a destination any
// more than it can sign in; otherwise the block is a sign-in inconvenience and
// nothing else.
func TestMastodonConnectRefusesABlockedInstance(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.BlockedInstances = map[string]bool{"evil.social": true}
	})
	_, cookie, csrf := h.signIn(t, "alice")

	post := postForm("/destinations", url.Values{"csrf": {csrf}, "kind": {"mastodon"}, "instance": {"Evil.Social"}})
	post.AddCookie(cookie)
	rec := h.do(post)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("connecting a blocked instance = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "cannot be connected") {
		t.Error("the refusal does not say the server is blocked")
	}
	var states int
	if err := h.store.DB().QueryRow(`SELECT count(*) FROM oauth_states`).Scan(&states); err != nil {
		t.Fatal(err)
	}
	if states != 0 {
		t.Errorf("%d oauth states recorded for a blocked instance", states)
	}
}

// The callback trusts nothing from its query string: the state has to match the
// cookie set when the flow began and has to be one this service recorded. A
// callback arriving in a browser that never started a flow is refused before
// anything is asked of an instance.
func TestCallbackRefusesAFlowThatDidNotStartHere(t *testing.T) {
	h := newHarness(t)

	// No cookie at all.
	rec := h.do(httptest.NewRequest(http.MethodGet, "/auth/callback?state=abc&code=xyz", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("callback without the state cookie = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "did not start here") {
		t.Error("the refusal does not say the flow did not start in this browser")
	}

	// A cookie that does not match the state is someone else's flow.
	req := httptest.NewRequest(http.MethodGet, "/auth/callback?state=abc&code=xyz", nil)
	req.AddCookie(&http.Cookie{Name: h.server.oauthCookieName(), Value: "not-abc"})
	if rec := h.do(req); rec.Code != http.StatusBadRequest {
		t.Errorf("callback with a mismatched state cookie = %d, want 400", rec.Code)
	}

	// A matching cookie for a state this service never recorded, or has already
	// consumed, is expired rather than looked up anywhere else.
	req = httptest.NewRequest(http.MethodGet, "/auth/callback?state=abc&code=xyz", nil)
	req.AddCookie(&http.Cookie{Name: h.server.oauthCookieName(), Value: "abc"})
	rec = h.do(req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("callback with an unknown state = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "expired") {
		t.Error("the refusal does not say the link has expired")
	}
	// Every refusal clears the cookie, so a stale one cannot keep tripping.
	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == h.server.oauthCookieName() && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("the state cookie was left in place after a refused callback")
	}
}

// startConnectFlow records a connect flow as startConnect would, with its
// verifier sealed so the callback can read it, and returns the state value the
// browser would carry back. The callback checks who is finishing a connect
// before it asks the instance for anything, which is what lets the check be
// driven end to end without an instance to ask.
func (h *harness) startConnectFlow(t *testing.T, owner *store.User, host string) string {
	t.Helper()
	state := secret.Token()
	sealed, err := h.keys.EncryptString(secret.PurposePKCEVerifier, secret.Token())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.PutOAuthState(context.Background(), secret.Hash(state),
		store.OAuthState{Host: host, Purpose: store.OAuthConnect, UserID: owner.ID}, sealed, time.Minute); err != nil {
		t.Fatal(err)
	}
	return state
}

// The session finishing a connect has to be the one that started it. The state
// cookie ties the callback to the browser, but a browser can hold a different
// session ten minutes later, and a token for somebody's account must not land
// on whoever happens to be signed in then — or on nobody. The refusal comes
// before any token is minted, so there is nothing to revoke and no instance to
// reach, and that is what this drives through the real handler.
func TestCallbackRefusesAConnectFinishedByAnotherSession(t *testing.T) {
	h := newHarness(t)
	alice, _, _ := h.signIn(t, "alice")
	mallory, mallorysCookie, _ := h.signIn(t, "mallory")
	ctx := context.Background()

	callback := func(state string, session *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/auth/callback?state="+state+"&code=xyz", nil)
		req.AddCookie(&http.Cookie{Name: h.server.oauthCookieName(), Value: state})
		if session != nil {
			req.AddCookie(session)
		}
		return h.do(req)
	}

	rec := callback(h.startConnectFlow(t, alice, "other.invalid"), mallorysCookie)
	if rec.Code != http.StatusForbidden {
		t.Errorf("another session finishing alice's connect = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "different account") {
		t.Error("the refusal does not say which account has to finish it")
	}

	rec = callback(h.startConnectFlow(t, alice, "other.invalid"), nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("a signed-out browser finishing a connect = %d, want 403", rec.Code)
	}

	// The state is consumed either way: a refused callback cannot be retried
	// from the right session, it has to be started again.
	state := h.startConnectFlow(t, alice, "other.invalid")
	callback(state, mallorysCookie)
	if _, _, err := h.store.TakeOAuthState(ctx, secret.Hash(state)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a refused connect left its state replayable: %v", err)
	}

	for _, u := range []*store.User{alice, mallory} {
		if list, _ := h.store.DestinationsByUser(ctx, u.ID); len(list) != 0 {
			t.Errorf("%s holds %d destinations after a refused connect, want none", u.Acct, len(list))
		}
	}
}

// An instance blocked after a connect was started is refused at the callback,
// again before any token is minted.
func TestCallbackRefusesAConnectToABlockedInstance(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.BlockedInstances = map[string]bool{"evil.social": true}
	})
	alice, cookie, _ := h.signIn(t, "alice")

	state := h.startConnectFlow(t, alice, "evil.social")
	req := httptest.NewRequest(http.MethodGet, "/auth/callback?state="+state+"&code=xyz", nil)
	req.AddCookie(&http.Cookie{Name: h.server.oauthCookieName(), Value: state})
	req.AddCookie(cookie)
	rec := h.do(req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("finishing a connect to a blocked instance = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "cannot be connected") {
		t.Error("the refusal does not say the server is blocked")
	}
}

// completeConnect is the tail of the callback, after the instance has handed
// back a token. It is driven directly below because the exchange in front of
// it needs a reachable instance and the tests have none. It repeats the
// session check as the guard on a function that stores a token under an
// account, and on that refusal hands the token back rather than keeping it.
//
// connectFlow is a connect started by owner against host.
func connectFlow(owner *store.User, host string) store.OAuthState {
	return store.OAuthState{Host: host, Purpose: store.OAuthConnect, UserID: owner.ID}
}

func TestCompleteConnectGuardsAgainstTheWrongAccount(t *testing.T) {
	h := newHarness(t)
	alice, _, _ := h.signIn(t, "alice")
	mallory, _, _ := h.signIn(t, "mallory")
	ctx := context.Background()

	app := &mastodon.App{ClientID: "client", ClientSecret: "secret"}
	account := &mastodon.Account{ID: "99", Acct: "someone"}

	for name, user := range map[string]*store.User{"another account": mallory, "nobody": nil} {
		rec := httptest.NewRecorder()
		h.server.completeConnect(ctx, rec, httptest.NewRequest(http.MethodGet, "/auth/callback", nil),
			user, connectFlow(alice, "other.invalid"), app, "fresh-token", account)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s finishing alice's connect = %d, want 403", name, rec.Code)
		}
	}

	// Nothing of the token was kept for anybody.
	for _, u := range []*store.User{alice, mallory} {
		list, err := h.store.DestinationsByUser(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 0 {
			t.Errorf("%s holds %d destinations after a refused connect, want none", u.Acct, len(list))
		}
	}
}

// The account that started the flow finishes it and gets a destination for the
// account the instance confirmed, sealed with the token just issued and
// attached to the feeds it already follows. Running the flow again for the
// same account repairs that destination rather than connecting it twice.
func TestConnectAddsTheConfirmedAccountAsADestination(t *testing.T) {
	h := newHarness(t)
	alice, _, _ := h.signIn(t, "alice")
	ctx := context.Background()

	f, _, err := h.store.Subscribe(ctx, alice.ID, "https://example.com/feed.xml", "", nil, store.FetchState{NextFetchAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	app := &mastodon.App{ClientID: "client", ClientSecret: "secret"}
	account := &mastodon.Account{ID: "99", Acct: "alice", DisplayName: "Alice elsewhere"}

	rec := httptest.NewRecorder()
	h.server.completeConnect(ctx, rec, httptest.NewRequest(http.MethodGet, "/auth/callback", nil),
		alice, connectFlow(alice, "other.invalid"), app, "first-token", account)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("completing a connect = %d, want 303: %s", rec.Code, rec.Body)
	}

	list, err := h.store.DestinationsByUser(ctx, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("account holds %d destinations after connecting, want 1", len(list))
	}
	d := list[0]
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/destinations/"+strconv.FormatInt(d.ID, 10)) {
		t.Errorf("redirected to %q, want the new destination's page", loc)
	}
	if d.Kind != destination.KindMastodon || d.Label != "@alice@other.invalid" {
		t.Errorf("destination is %s %q, want a mastodon one named for the connected account", d.Kind, d.Label)
	}
	var cfg destination.MastodonConfig
	if err := json.Unmarshal([]byte(d.Config), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.SameAccount("other.invalid", "99") || cfg.Acct != "alice" {
		t.Errorf("destination points at %+v, want the account the instance confirmed", cfg)
	}
	// The same username on a different instance is a different account: the
	// one connected must not be mistaken for the one that signed in.
	if cfg.SameAccount(alice.Host, alice.RemoteID) {
		t.Error("the connected account matched the sign-in account")
	}
	if got := h.server.storedMastodonToken(d); got != "first-token" {
		t.Errorf("stored token = %q, want the one the instance issued", got)
	}
	if routes, _ := h.store.FeedRoutes(ctx, alice.ID, f.FeedID); len(routes) != 1 || routes[0] != d.ID {
		t.Errorf("routes = %v, want the new destination attached to the existing feed", routes)
	}
	// Connecting somebody else's account does not touch the sign-in token.
	if saved, _ := h.store.UserByID(ctx, alice.ID); string(saved.AccessToken) != string(alice.AccessToken) {
		t.Error("connecting another account replaced the sign-in token")
	}

	// Connecting the same account again re-seals it against the new token.
	rec = httptest.NewRecorder()
	h.server.completeConnect(ctx, rec, httptest.NewRequest(http.MethodGet, "/auth/callback", nil),
		alice, connectFlow(alice, "other.invalid"), app, "second-token", account)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("reconnecting = %d, want 303: %s", rec.Code, rec.Body)
	}
	list, _ = h.store.DestinationsByUser(ctx, alice.ID)
	if len(list) != 1 {
		t.Fatalf("reconnecting the same account produced %d destinations, want still 1", len(list))
	}
	if got := h.server.storedMastodonToken(list[0]); got != "second-token" {
		t.Errorf("stored token after reconnecting = %q, want the new one", got)
	}
}

// Connecting the account the person signed in with is allowed, and renews it:
// the new token becomes the sign-in token as well as the destination's, so
// nothing is left pointing at the one it replaced, and the account still holds
// one destination for itself rather than two.
func TestConnectingTheOwnAccountRenewsTheSignInToken(t *testing.T) {
	h := newHarness(t)
	alice, _, _ := h.signIn(t, "alice")
	ctx := context.Background()

	own, _, err := h.server.ensureMastodonDestination(ctx, alice, "")
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.server.completeConnect(ctx, rec, httptest.NewRequest(http.MethodGet, "/auth/callback", nil),
		alice, connectFlow(alice, alice.Host), &mastodon.App{ClientID: "client", ClientSecret: "secret"},
		"renewed-token", &mastodon.Account{ID: alice.RemoteID, Acct: alice.Acct})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("connecting the own account = %d, want 303: %s", rec.Code, rec.Body)
	}

	list, err := h.store.DestinationsByUser(ctx, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != own.ID {
		t.Fatalf("account holds %d destinations after connecting itself, want the one it had", len(list))
	}
	if got := h.server.storedMastodonToken(list[0]); got != "renewed-token" {
		t.Errorf("destination token = %q, want the renewed one", got)
	}
	saved, err := h.store.UserByID(ctx, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := h.server.storedToken(saved); err != nil || got != "renewed-token" {
		t.Errorf("sign-in token = %q, %v; want the renewed one, or the next feed added would post with a retired token", got, err)
	}
}

// A connect that lands on a full account is refused and stores nothing. The
// limit is checked at the insert, so the flow has to be allowed to run to its
// end and fail there rather than trusting a count taken before the handoff.
func TestConnectIsRefusedOnceTheAccountIsFull(t *testing.T) {
	h := newHarness(t)
	alice, _, _ := h.signIn(t, "alice")
	ctx := context.Background()

	for i := range store.MaxDestinationsPerAccount {
		d := &store.Destination{UserID: alice.ID, Kind: "webhook", Label: fmt.Sprintf("hook %d", i)}
		if err := h.store.CreateDestination(ctx, d); err != nil {
			t.Fatal(err)
		}
	}

	rec := httptest.NewRecorder()
	h.server.completeConnect(ctx, rec, httptest.NewRequest(http.MethodGet, "/auth/callback", nil),
		alice, connectFlow(alice, "other.invalid"),
		&mastodon.App{ClientID: "client", ClientSecret: "secret"}, "fresh-token", &mastodon.Account{ID: "99", Acct: "someone"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("connecting past the limit = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), destinationLimitMessage) {
		t.Error("the refusal does not say the account is full")
	}
	if list, _ := h.store.DestinationsByUser(ctx, alice.ID); len(list) != store.MaxDestinationsPerAccount {
		t.Errorf("account holds %d destinations, want the limit untouched", len(list))
	}
}

// --- deleting ----------------------------------------------------------------

// Removing a connected account removes it and its routes and nothing else: the
// feeds it published stay, and another account's id is a 404 rather than a
// lever.
func TestDeletingAConnectedAccountRemovesItAndItsRoutes(t *testing.T) {
	h := newHarness(t)
	alice, cookie, csrf := h.signIn(t, "alice")
	_, mallorysCookie, mallorysCSRF := h.signIn(t, "mallory")
	ctx := context.Background()

	f, _, err := h.store.Subscribe(ctx, alice.ID, "https://example.com/feed.xml", "", nil, store.FetchState{NextFetchAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	// A client is already registered for the instance, so handing the token
	// back needs no registration round trip; the revocation itself goes to a
	// host that does not exist, which is logged and must not stop the delete.
	sealed, err := h.keys.EncryptString(secret.PurposeClientSecret, "secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SaveInstance(ctx, &store.Instance{
		Host: "other.invalid", ClientID: "client", ClientSecret: sealed, RedirectURI: h.server.redirectURI(),
	}); err != nil {
		t.Fatal(err)
	}
	d, _, err := h.server.connectMastodonDestination(ctx, alice, "other.invalid", &mastodon.Account{ID: "99", Acct: "alice"}, "other-token")
	if err != nil {
		t.Fatal(err)
	}
	if routes, _ := h.store.FeedRoutes(ctx, alice.ID, f.FeedID); len(routes) != 1 {
		t.Fatalf("precondition: routes = %v, want the connected account", routes)
	}
	path := "/destinations/" + strconv.FormatInt(d.ID, 10) + "/delete"

	req := postForm(path, url.Values{"csrf": {mallorysCSRF}})
	req.AddCookie(mallorysCookie)
	if rec := h.do(req); rec.Code != http.StatusNotFound {
		t.Errorf("deleting another account's destination = %d, want 404", rec.Code)
	}
	if _, err := h.store.Destination(ctx, alice.ID, d.ID); err != nil {
		t.Fatalf("another account's attempt removed the destination: %v", err)
	}

	req = postForm(path, url.Values{"csrf": {csrf}})
	req.AddCookie(cookie)
	rec := h.do(req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("deleting a destination = %d, want 303: %s", rec.Code, rec.Body)
	}
	if _, err := h.store.Destination(ctx, alice.ID, d.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("destination after delete: %v, want ErrNotFound", err)
	}
	if routes, _ := h.store.FeedRoutes(ctx, alice.ID, f.FeedID); len(routes) != 0 {
		t.Errorf("routes survived the delete: %v", routes)
	}
	if _, err := h.store.FeedByUser(ctx, alice.ID, f.FeedID); err != nil {
		t.Errorf("deleting a destination took the feed with it: %v", err)
	}
}

// The destination that posts as the signed-in account cannot be removed: every
// sign-in and every feed add would put it back, routed to every feed, so
// content the person had stopped posting would resume on their own timeline
// with nobody asking. Pausing is the way to stop it, and the page says so
// rather than offering a button that silently undoes itself.
func TestTheOwnAccountDestinationCannotBeDeleted(t *testing.T) {
	h := newHarness(t)
	alice, cookie, csrf := h.signIn(t, "alice")
	ctx := context.Background()

	f, _, err := h.store.Subscribe(ctx, alice.ID, "https://example.com/feed.xml", "", nil, store.FetchState{NextFetchAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	own, _, err := h.server.ensureMastodonDestination(ctx, alice, "")
	if err != nil {
		t.Fatal(err)
	}
	// A further account on another server is removable, which is what the
	// missing button below has to be contrasted against.
	other, _, err := h.server.connectMastodonDestination(ctx, alice, "other.invalid", &mastodon.Account{ID: "99", Acct: "alice"}, "other-token")
	if err != nil {
		t.Fatal(err)
	}
	path := func(d *store.Destination) string { return "/destinations/" + strconv.FormatInt(d.ID, 10) }

	req := httptest.NewRequest(http.MethodGet, path(own), nil)
	req.AddCookie(cookie)
	if body := h.do(req).Body.String(); strings.Contains(body, `action="`+path(own)+`/delete"`) {
		t.Error("the own account's page offers a Remove button that would be refused")
	}
	req = httptest.NewRequest(http.MethodGet, path(other), nil)
	req.AddCookie(cookie)
	if body := h.do(req).Body.String(); !strings.Contains(body, `action="`+path(other)+`/delete"`) {
		t.Error("a connected account's page does not offer to remove it")
	}

	req = postForm(path(own)+"/delete", url.Values{"csrf": {csrf}})
	req.AddCookie(cookie)
	rec := h.do(req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("deleting the account's own destination = %d, want 400: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "Pause it") {
		t.Error("the refusal does not say that pausing is the alternative")
	}

	kept, err := h.store.Destination(ctx, alice.ID, own.ID)
	if err != nil {
		t.Fatalf("the own destination was removed anyway: %v", err)
	}
	// Still posting with the token sign-in issued: a refused delete must not
	// have handed it back to the instance either.
	if got := h.server.storedMastodonToken(kept); got != "token-alice" {
		t.Errorf("own destination's token = %q, want the account's own", got)
	}
	if routes, _ := h.store.FeedRoutes(ctx, alice.ID, f.FeedID); len(routes) != 2 {
		t.Errorf("routes = %v, want both destinations still attached", routes)
	}
}

// --- the destination limit ---------------------------------------------------

// The dashboard says how many of the account's slots are used, offers every
// service while one is free, and stops offering once none is. The handler holds
// the rule up behind the page: a full account's POST is refused and says why.
func TestDashboardCountsDestinationsAgainstTheLimit(t *testing.T) {
	h := newHarness(t)
	alice, cookie, csrf := h.signIn(t, "alice")
	ctx := context.Background()

	add := func(n int) {
		t.Helper()
		for i := range n {
			d := &store.Destination{UserID: alice.ID, Kind: "webhook", Label: fmt.Sprintf("hook %d", i)}
			if err := h.store.CreateDestination(ctx, d); err != nil {
				t.Fatal(err)
			}
		}
	}
	dashboard := func() string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
		req.AddCookie(cookie)
		rec := h.do(req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /dashboard = %d", rec.Code)
		}
		return rec.Body.String()
	}
	max := strconv.Itoa(store.MaxDestinationsPerAccount)

	add(3)
	body := dashboard()
	if !strings.Contains(body, "3 of "+max+".") {
		t.Errorf("dashboard does not say 3 of %s destinations are used", max)
	}
	if !strings.Contains(body, `href="/destinations/new?kind=webhook"`) {
		t.Error("a dashboard with room left does not offer to connect anything")
	}

	add(store.MaxDestinationsPerAccount - 3)
	body = dashboard()
	if !strings.Contains(body, max+" of "+max+" destinations. Remove one") {
		t.Errorf("a full dashboard does not say so:\n%s", body)
	}
	if strings.Contains(body, `href="/destinations/new?kind=`) {
		t.Error("a full account is still offered the connect list")
	}

	post := postForm("/destinations", url.Values{"csrf": {csrf}, "kind": {"webhook"}, "url": {"https://example.com/hook"}})
	post.AddCookie(cookie)
	rec := h.do(post)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("connecting past the limit = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), destinationLimitMessage) {
		t.Error("the refusal does not say the account is full")
	}
	if list, _ := h.store.DestinationsByUser(ctx, alice.ID); len(list) != store.MaxDestinationsPerAccount {
		t.Errorf("account holds %d destinations, want the limit untouched", len(list))
	}
}

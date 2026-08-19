package destination

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A linkding profile document, as the real one answers. Two known keys are
// enough to pass looksLikeLinkding, so this carries more than the minimum on
// purpose: the fixture should look like the thing, not like the check.
const linkdingProfile = `{"theme":"auto","bookmark_date_display":"relative",
	"bookmark_link_target":"_blank","tag_search":"lax","enable_sharing":false,
	"web_archive_integration":"disabled"}`

func TestParseLinkdingServer(t *testing.T) {
	// A bare host and a trailing slash are the same address, so two spellings do
	// not read as a move that needs re-verifying.
	for _, raw := range []string{
		"linkding.example", "https://linkding.example", "https://linkding.example/",
		// A host is case-insensitive, so correcting the case of a stored address is
		// not a move that has to prove itself again.
		"https://LinkDing.Example",
	} {
		u, err := ParseLinkdingServer(raw)
		if err != nil {
			t.Fatalf("%q was rejected: %v", raw, err)
		}
		if u.String() != "https://linkding.example" {
			t.Errorf("%q parsed to %q", raw, u)
		}
	}

	// linkding can be served under a prefix, so one is kept rather than dropped.
	u, err := ParseLinkdingServer("https://example.com/linkding/")
	if err != nil {
		t.Fatalf("a context path was rejected: %v", err)
	}
	if u.String() != "https://example.com/linkding" {
		t.Errorf("context path parsed to %q", u)
	}

	bad := map[string]string{
		"empty":              "",
		"plain http":         "http://linkding.example",
		"credentials in URL": "https://user:pass@linkding.example",
		"no host":            "https:///linkding",
		"another scheme":     "ftp://linkding.example",
		"a query":            "https://linkding.example/?x=1",
		"a fragment":         "https://linkding.example/#x",
		// The address wanted is the one linkding is served from. These are pages
		// inside it, which is what someone copying from the browser has.
		"the bookmarks page": "https://linkding.example/bookmarks",
		"the settings page":  "https://linkding.example/settings/integrations",
		"the API itself":     "https://linkding.example/api/bookmarks/",
		// A dot segment is spelled entirely in characters a prefix may contain, and
		// it is the one way the stored address and the address actually requested
		// can differ: a server resolves "/../api/bookmarks/" to "/api/bookmarks/".
		// It also walks past the reserved-page check.
		"a parent segment":     "https://linkding.example/..",
		"a parent then a page": "https://linkding.example/../api",
		"a current segment":    "https://linkding.example/./api",
	}
	for name, raw := range bad {
		if _, err := ParseLinkdingServer(raw); err == nil {
			t.Errorf("%s was accepted: %q", name, raw)
		}
	}
}

// A linkding tag name cannot contain whitespace, so the field is split the way
// linkding splits it: what the form shows back is what the bookmark carries.
func TestParseLinkdingTags(t *testing.T) {
	got, err := ParseLinkdingTags("  feeds, read-later #news  feeds ")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"feeds", "read-later", "news"}
	if len(got) != len(want) {
		t.Fatalf("tags = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tags = %q, want %q", got, want)
			break
		}
	}

	// Case is how linkding matches, so two spellings are one tag and the first
	// one typed is the one kept.
	if got, _ := ParseLinkdingTags("News news NEWS"); len(got) != 1 || got[0] != "News" {
		t.Errorf("tags = %q, want the first spelling only", got)
	}
	if got, err := ParseLinkdingTags("   "); err != nil || len(got) != 0 {
		t.Errorf("an empty field = %q, %v; want no tags and no error", got, err)
	}
	if _, err := ParseLinkdingTags(strings.Repeat("a", 65)); err == nil {
		t.Error("a 65-character tag name was accepted")
	}
	// Counted after the duplicates are folded together, so the limit is on tags
	// rather than on words typed.
	var many []string
	for i := 0; i <= maxLinkdingTags; i++ {
		many = append(many, "tag"+strconv.Itoa(i))
	}
	if _, err := ParseLinkdingTags(strings.Join(many, " ")); err == nil {
		t.Errorf("more than %d tags was accepted", maxLinkdingTags)
	}
	if _, err := ParseLinkdingTags(strings.Join(many[:maxLinkdingTags], " ")); err != nil {
		t.Errorf("exactly %d tags was rejected: %v", maxLinkdingTags, err)
	}

	// A name is shown back in the form as well as sent to linkding, so a character
	// that does not show what it is has no business in one. A bidi override
	// reverses the rest of the rendered list.
	for name, tags := range map[string]string{
		"a NUL":           "feeds\x00news",
		"an escape":       "feeds\x1bnews",
		"a bidi override": "feeds ‮news",
		"a zero-width":    "feeds ​news",
	} {
		if _, err := ParseLinkdingTags(tags); err == nil {
			t.Errorf("%s was accepted: %q", name, tags)
		}
	}
}

// The field is gated before it is split, not after. Without that, a large value
// allocates for as many tags as it has words — the dedupe folds them into one, so
// the count limit never refuses it — and one account can do that repeatedly
// without ever storing a destination.
func TestParseLinkdingTagsRefusesAHugeFieldWithoutSplittingIt(t *testing.T) {
	huge := strings.Repeat("a ", 5<<20) // 10 MB, which r.ParseForm accepts

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	if _, err := ParseLinkdingTags(huge); err == nil {
		t.Fatal("a 10 MB tags field was accepted")
	}
	runtime.ReadMemStats(&after)

	// The gate is a length comparison, so the call must cost about nothing beyond
	// the string it was handed. A generous ceiling: the point is the order of
	// magnitude, not the byte count.
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 1<<20 {
		t.Errorf("parsing allocated %d bytes, want almost none: the field is being split before it is refused", grew)
	}
}

// The token travels in an Authorization header, so anything that cannot appear
// in one is refused at the form rather than at send time.
func TestValidLinkdingToken(t *testing.T) {
	if err := ValidLinkdingToken("2b8c1d0e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b9c"); err != nil {
		t.Errorf("a real-looking token was rejected: %v", err)
	}
	bad := map[string]string{
		"empty":       "",
		"spaces only": "   ",
		"a newline":   "abc\ndef",
		"a space":     "abc def",
		"a tab":       "abc\tdef",
		"non-ASCII":   "abcdé",
		"too long":    strings.Repeat("a", 301),
	}
	for name, token := range bad {
		if err := ValidLinkdingToken(token); err == nil {
			t.Errorf("%s was accepted: %q", name, token)
		}
	}
}

// A bookmark is a link with a title, so those are their own fields and the
// template fills the description. The token goes in the header as linkding's own
// scheme, which is Token rather than Bearer.
func TestLinkdingSendCreatesABookmark(t *testing.T) {
	var got linkdingBookmark
	var auth, path, method string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, path, method = r.Header.Get("Authorization"), r.URL.Path, r.Method
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("body was not JSON: %s", body)
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":7,"url":"https://example.com/1"}`))
	}))
	defer srv.Close()

	target := &linkdingTarget{
		http:  devClient(),
		base:  srv.URL,
		cfg:   LinkdingConfig{Tags: []string{"feeds", "news"}, Unread: true},
		token: "tk_secret",
	}
	res, err := target.Send(context.Background(), Post{
		Text: "The first part of the entry.",
		Item: Item{Title: "An entry — with an em dash", URL: "https://example.com/1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || path != "/api/bookmarks/" {
		t.Errorf("sent %s %q, want POST /api/bookmarks/", method, path)
	}
	if auth != "Token tk_secret" {
		t.Errorf("Authorization = %q, want linkding's Token scheme", auth)
	}
	if got.URL != "https://example.com/1" {
		t.Errorf("bookmarked %q, want the entry's link", got.URL)
	}
	if got.Title != "An entry — with an em dash" {
		t.Errorf("title = %q, want the entry's own title", got.Title)
	}
	if got.Description != "The first part of the entry." {
		t.Errorf("description = %q, want the rendered template", got.Description)
	}
	if len(got.TagNames) != 2 || got.TagNames[0] != "feeds" {
		t.Errorf("tag_names = %q", got.TagNames)
	}
	if !got.Unread {
		t.Error("unread was not sent, so a reading list arrives already read")
	}
	// linkding has no page for one bookmark, so there is nothing to link to.
	if res.URL != "" {
		t.Errorf("result URL = %q, want empty", res.URL)
	}
}

// A context path belongs in front of the API path, not instead of it.
func TestLinkdingSendKeepsTheContextPath(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	target := &linkdingTarget{http: devClient(), base: srv.URL + "/linkding", token: "tk"}
	if _, err := target.Send(context.Background(), Post{
		Text: "text", Item: Item{URL: "https://example.com/1"},
	}); err != nil {
		t.Fatal(err)
	}
	if path != "/linkding/api/bookmarks/" {
		t.Errorf("posted to %q, want the API under the prefix", path)
	}
}

// An entry with no link is nothing a bookmark manager can store, and no number
// of retries will give it one.
func TestLinkdingRefusesAnEntryWithoutALink(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	target := &linkdingTarget{http: devClient(), base: srv.URL, token: "tk"}
	for _, raw := range []string{"", "javascript:alert(1)", "not a url at all"} {
		_, err := target.Send(context.Background(), Post{Text: "text", Item: Item{URL: raw}})
		if err == nil {
			t.Errorf("%q was bookmarked", raw)
		} else if !IsPermanent(err) {
			t.Errorf("%q is retryable, so it would be tried six times: %v", raw, err)
		}
	}
	if calls != 0 {
		t.Errorf("reached linkding %d times for an entry with no link", calls)
	}
}

// A revoked token, an address with no API, and a bookmark linkding validated and
// refused are all settled: retrying cannot fix them.
func TestLinkdingRefusalsArePermanent(t *testing.T) {
	for _, code := range []int{
		http.StatusUnauthorized, http.StatusForbidden,
		http.StatusNotFound, http.StatusBadRequest,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			w.Write([]byte(`{"detail":"nope"}`))
		}))
		target := &linkdingTarget{http: devClient(), base: srv.URL, token: "tk"}
		_, err := target.Send(context.Background(), Post{
			Text: "text", Item: Item{URL: "https://example.com/1"},
		})
		if err == nil {
			t.Errorf("%d reported success", code)
		} else if !IsPermanent(err) {
			t.Errorf("%d from linkding is retryable, so it would be tried six times: %v", code, err)
		}
		srv.Close()
	}
}

func TestLinkdingServerErrorIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "45")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	target := &linkdingTarget{http: devClient(), base: srv.URL, token: "tk"}
	_, err := target.Send(context.Background(), Post{
		Text: "text", Item: Item{URL: "https://example.com/1"},
	})
	if err == nil {
		t.Fatal("a 429 reported success")
	}
	if IsPermanent(err) {
		t.Errorf("429 was treated as permanent: %v", err)
	}
	if d, ok := RetryAfter(err); !ok || d != 45*time.Second {
		t.Errorf("RetryAfter = %v, %v; want 45s", d, ok)
	}
}

// The profile read is the control that keeps this kind from being an open relay,
// so an address that does not answer as linkding must be refused before anything
// is stored — and nothing may be written while proving it.
func TestVerifyLinkdingRequiresALinkding(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"not linkding at all": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("hello from someone else's server"))
		},
		"an empty document": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{}`))
		},
		"some other JSON API": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"ok":true,"version":"1.2.3"}`))
		},
		"a refused token": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"detail":"Invalid token."}`))
		},
		"no API there": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		},
		"an error": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
	}
	for name, profile := range cases {
		writes := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				writes++
				w.WriteHeader(http.StatusCreated)
				return
			}
			profile(w, r)
		}))
		if err := verifyLinkding(context.Background(), devClient(), srv.URL, "tk"); err == nil {
			t.Errorf("%s was accepted as a linkding", name)
		}
		if writes != 0 {
			t.Errorf("%s: wrote %d times to a server that had not proved itself", name, writes)
		}
		srv.Close()
	}
}

// A linkding that answers proves the address and the token together, and unlike
// Slack or ntfy it does so without leaving anything behind.
func TestVerifyLinkdingReadsTheProfileAndWritesNothing(t *testing.T) {
	reads, writes := 0, 0
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes++
			return
		}
		if r.URL.Path != "/api/user/profile/" {
			t.Errorf("read %q, want /api/user/profile/", r.URL.Path)
		}
		reads++
		auth = r.Header.Get("Authorization")
		w.Write([]byte(linkdingProfile))
	}))
	defer srv.Close()

	if err := verifyLinkding(context.Background(), devClient(), srv.URL, "tk_secret"); err != nil {
		t.Fatalf("a linkding was rejected: %v", err)
	}
	if reads != 1 || writes != 0 {
		t.Errorf("read %d times and wrote %d, want 1 and 0", reads, writes)
	}
	if auth != "Token tk_secret" {
		t.Errorf("Authorization = %q, so the token was not proved", auth)
	}
}

// A stored row is not trusted any more than a submitted form: the address, the
// token and the tags are all re-checked when the target is built.
func TestNewLinkdingRejectsUnusableStoredConfig(t *testing.T) {
	creds, _ := json.Marshal(LinkdingCredentials{Token: "tk"})
	bad := map[string]LinkdingConfig{
		"no address":         {},
		"plain http":         {Server: "http://linkding.example"},
		"a page inside":      {Server: "https://linkding.example/bookmarks"},
		"a tag with a space": {Server: "https://linkding.example", Tags: []string{strings.Repeat("a", 65)}},
	}
	for name, cfg := range bad {
		raw, _ := json.Marshal(cfg)
		if _, err := NewLinkding(devClient(), string(raw), creds); err == nil {
			t.Errorf("%s was accepted: %+v", name, cfg)
		} else if !IsPermanent(err) {
			t.Errorf("%s is retryable, so it would be tried forever: %v", name, err)
		}
	}

	// Unlike ntfy's optional access token, a linkding token is the whole
	// authorisation: without one there is nothing to try.
	raw, _ := json.Marshal(LinkdingConfig{Server: "https://linkding.example"})
	if _, err := NewLinkding(devClient(), string(raw), nil); err == nil {
		t.Error("a destination with no token was accepted")
	} else if !IsPermanent(err) {
		t.Errorf("a missing token is retryable: %v", err)
	}

	target, err := NewLinkding(devClient(), string(raw), creds)
	if err != nil {
		t.Fatalf("a usable row was rejected: %v", err)
	}
	if got := target.Limit(context.Background()); got != LinkdingLimit {
		t.Errorf("limit = %d, want %d", got, LinkdingLimit)
	}
}

// A stored tag carrying whitespace is two tags to linkding rather than the one it
// looks like here, so stored names go back through the same split as the form.
func TestNewLinkdingNormalisesStoredTags(t *testing.T) {
	raw, _ := json.Marshal(LinkdingConfig{
		Server: "https://linkding.example",
		Tags:   []string{"read later", "news"},
	})
	creds, _ := json.Marshal(LinkdingCredentials{Token: "tk"})
	target, err := NewLinkding(devClient(), string(raw), creds)
	if err != nil {
		t.Fatal(err)
	}
	got := target.(*linkdingTarget).cfg.Tags
	want := []string{"read", "later", "news"}
	if len(got) != len(want) || got[0] != want[0] || got[2] != want[2] {
		t.Errorf("tags = %q, want %q", got, want)
	}
}

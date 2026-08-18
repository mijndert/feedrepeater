package feed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"feedrepeater.com/internal/safehttp"
)

const atomDoc = `<?xml version="1.0"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>Example</title>
  <entry><id>1</id><title>An entry</title><link href="https://example.com/1"/></entry>
</feed>`

func devFetcher() *Fetcher {
	return NewFetcher(safehttp.New(safehttp.Options{UserAgent: "test", AllowPrivate: true}))
}

func TestDiscoverFeedsReadsLinkTags(t *testing.T) {
	base, _ := url.Parse("https://example.com/blog/")
	page := `<html><head>
	  <link rel="stylesheet" href="/style.css">
	  <link rel="alternate" hreflang="fr" href="/fr/">
	  <link rel="alternate" type="application/rss+xml" title="RSS" href="feed.xml">
	  <link rel="alternate" type="application/atom+xml" href="https://cdn.example.com/atom.xml">
	  <link rel="feed" href="/other">
	</head><body>no</body></html>`

	got := DiscoverFeeds([]byte(page), base)
	want := []string{
		"https://example.com/blog/feed.xml",
		"https://cdn.example.com/atom.xml",
		"https://example.com/other",
	}
	if len(got) != len(want) {
		t.Fatalf("found %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("candidate %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// A page full of links is not an invitation to make a request per link, and
// nothing outside http and https is a feed address.
func TestDiscoverFeedsIsBounded(t *testing.T) {
	base, _ := url.Parse("https://example.com/")
	var b strings.Builder
	for i := range 20 {
		b.WriteString(`<link rel="alternate" type="application/rss+xml" href="/f`)
		b.WriteString(string(rune('a' + i)))
		b.WriteString(`.xml">`)
	}
	if got := DiscoverFeeds([]byte(b.String()), base); len(got) != maxCandidates {
		t.Errorf("found %d candidates, want the cap of %d", len(got), maxCandidates)
	}

	odd := `<link rel="alternate" type="application/rss+xml" href="javascript:alert(1)">
	        <link rel="alternate" type="application/rss+xml" href="data:text/xml,x">
	        <link rel="alternate" type="application/rss+xml" href="ftp://example.com/f.xml">`
	if got := DiscoverFeeds([]byte(odd), base); len(got) != 0 {
		t.Errorf("non-web schemes were offered as feeds: %v", got)
	}

	// Duplicates in a page are one candidate, not several requests.
	dupes := `<link rel="alternate" type="application/rss+xml" href="/f.xml">
	          <link rel="alternate" type="application/rss+xml" href="/f.xml">`
	if got := DiscoverFeeds([]byte(dupes), base); len(got) != 1 {
		t.Errorf("the same address was offered %d times", len(got))
	}
}

// The address people paste is the site, so a page that links to its feed has to
// resolve to that feed, and the address that worked is the one to store.
func TestResolveFollowsAPageToItsFeed(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<html><head><link rel="alternate" type="application/atom+xml" href="/atom.xml"></head></html>`))
		case "/atom.xml":
			w.Header().Set("Content-Type", "application/atom+xml")
			w.Write([]byte(atomDoc))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	got, res, err := devFetcher().Resolve(context.Background(), srv.URL+"/")
	if err != nil {
		t.Fatal(err)
	}
	if got != srv.URL+"/atom.xml" {
		t.Errorf("resolved to %q, want the linked feed", got)
	}
	if res.Title != "Example" || len(res.Entries) != 1 {
		t.Errorf("resolved feed = %q with %d entries", res.Title, len(res.Entries))
	}
	// The page is read once, then the feed. Fetching the page twice would be a
	// second request to someone else's server for nothing.
	if len(paths) != 2 || paths[0] != "/" || paths[1] != "/atom.xml" {
		t.Errorf("requests were %v, want the page then the feed", paths)
	}
}

// A feed address still costs exactly one request and comes back unchanged.
func TestResolveLeavesAFeedAddressAlone(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/atom+xml")
		w.Header().Set("ETag", `"abc"`)
		w.Write([]byte(atomDoc))
	}))
	defer srv.Close()

	got, res, err := devFetcher().Resolve(context.Background(), srv.URL+"/atom.xml")
	if err != nil {
		t.Fatal(err)
	}
	if got != srv.URL+"/atom.xml" {
		t.Errorf("address changed to %q", got)
	}
	if requests != 1 {
		t.Errorf("a plain feed cost %d requests", requests)
	}
	// The conditional-request headers survive the trip through discovery.
	if res.ETag != `"abc"` {
		t.Errorf("ETag = %q, so the next fetch cannot be conditional", res.ETag)
	}
}

// A page with no feed on it reports what the user did wrong, and a page that
// will not load is not searched for links.
func TestResolveGivesUpCleanly(t *testing.T) {
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><title>No feed here</title></head></html>`))
	}))
	defer page.Close()
	if _, _, err := devFetcher().Resolve(context.Background(), page.URL); err == nil {
		t.Error("a page with no feed was accepted")
	}

	gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer gone.Close()
	if _, _, err := devFetcher().Resolve(context.Background(), gone.URL); err == nil {
		t.Error("a 404 was accepted")
	}
}

// A linked feed is an address from someone else's document, so it goes through
// the same guard as one typed into the form.
func TestResolveWillNotFollowALinkToABlockedAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head>
			<link rel="alternate" type="application/rss+xml" href="http://169.254.169.254/latest/meta-data/">
		</head></html>`))
	}))
	defer srv.Close()

	// A client with the guard on, which is how it runs everywhere but a test.
	guarded := NewFetcher(safehttp.New(safehttp.Options{UserAgent: "test"}))
	if _, _, err := guarded.Resolve(context.Background(), srv.URL); err == nil {
		t.Error("discovery followed a link to a link-local address")
	}
}

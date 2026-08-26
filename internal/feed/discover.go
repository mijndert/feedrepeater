package feed

import (
	"bytes"
	"context"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// maxCandidates bounds how many linked feeds are tried before giving up. A page
// that advertises more than a handful is either a directory or hostile, and
// either way one request per link is the cost.
const maxCandidates = 3

// feedTypes are the content types a page uses to advertise a feed.
var feedTypes = map[string]bool{
	"application/rss+xml":   true,
	"application/atom+xml":  true,
	"application/feed+json": true,
	"application/json":      true,
	"text/xml":              true,
	"application/xml":       true,
}

// Resolve finds the feed at an address, or the feed that address links to.
//
// People paste the site, not the feed: it is the first thing to go wrong when
// adding one, and the answer is usually in the page's own <link rel="alternate">
// tags. The address that worked is returned, because that is what gets stored;
// the caller must not assume it is the one it passed in.
//
// The page is fetched once. A document that parses as a feed is used as one,
// and only a document that does not is read as HTML, so the common case costs
// exactly one request and discovery costs one more per candidate tried.
func (f *Fetcher) Resolve(ctx context.Context, raw string) (string, *Result, error) {
	got, err := f.get(ctx, raw, "", "")
	if err != nil {
		// A page that would not load is not a page to look for links in.
		return "", nil, err
	}
	res, feedErr := Parse(got.body, raw)
	if feedErr == nil {
		res.ETag, res.LastModified, res.Hint = got.etag, got.lastModified, got.hint
		return raw, res, nil
	}

	base, err := url.Parse(raw)
	if err != nil {
		return "", nil, feedErr
	}
	for _, candidate := range DiscoverFeeds(got.body, base) {
		// Every candidate comes from a document written by someone else, so it
		// is validated exactly like an address typed into the form.
		u, err := f.http.ParseURL(candidate)
		if err != nil {
			continue
		}
		res, err := f.Fetch(ctx, u.String(), "", "", nil)
		if err != nil {
			continue
		}
		return u.String(), res, nil
	}
	// The original error is the useful one: what they gave us is not a feed.
	return "", nil, feedErr
}

// DiscoverFeeds returns the feeds an HTML document links to, in document order,
// which is the order the page itself puts them in.
func DiscoverFeeds(body []byte, base *url.URL) []string {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil
	}

	var out []string
	seen := map[string]bool{}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if len(out) >= maxCandidates {
			return
		}
		if n.Type == html.ElementNode && n.Data == "link" {
			if href, ok := feedLink(n); ok {
				if resolved, ok := resolveRef(base, href); ok && !seen[resolved] {
					seen[resolved] = true
					out = append(out, resolved)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return out
}

// feedLink reports whether a <link> element advertises a feed, and its href.
func feedLink(n *html.Node) (string, bool) {
	var rel, typ, href string
	for _, a := range n.Attr {
		switch strings.ToLower(a.Key) {
		case "rel":
			rel = strings.ToLower(a.Val)
		case "type":
			typ = strings.ToLower(strings.TrimSpace(a.Val))
		case "href":
			href = strings.TrimSpace(a.Val)
		}
	}
	if href == "" {
		return "", false
	}
	// rel is a space-separated set, and "feed" is the older spelling of the
	// same intent.
	rels := strings.Fields(rel)
	alternate, feedRel := false, false
	for _, r := range rels {
		switch r {
		case "alternate":
			alternate = true
		case "feed":
			feedRel = true
		}
	}
	if !alternate && !feedRel {
		return "", false
	}
	typ, _, _ = strings.Cut(typ, ";")
	typ = strings.TrimSpace(typ)
	switch {
	case feedTypes[typ]:
		return href, true
	case feedRel && typ == "":
		// rel="feed" says what it is without needing a type.
		return href, true
	}
	// A bare rel="alternate" means "another version of this page", which is
	// usually a translation, not a feed.
	return "", false
}

// resolveRef turns a possibly relative href into an absolute http or https URL.
func resolveRef(base *url.URL, href string) (string, bool) {
	ref, err := url.Parse(href)
	if err != nil {
		return "", false
	}
	abs := base.ResolveReference(ref)
	if abs.Scheme != "http" && abs.Scheme != "https" {
		return "", false
	}
	if abs.Host == "" {
		return "", false
	}
	return abs.String(), true
}

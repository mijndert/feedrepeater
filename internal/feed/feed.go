// Package feed fetches and normalises RSS, Atom, and JSON feeds.
package feed

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mmcdole/gofeed"

	"feedrepeater.com/internal/render"
	"feedrepeater.com/internal/safehttp"
)

// Limits on what is kept from a feed, so one entry cannot fill the database.
const (
	MaxTitleLen   = 500
	MaxSummaryLen = 1000
	MaxURLLen     = 2000
	MaxGUIDLen    = 500
	MaxEntries    = 200

	// MaxFeedBytes caps a feed document. The shared client allows five
	// megabytes because an instance API may legitimately answer with one, but a
	// feed that size is a mistake or an attack: gofeed builds
	// the whole document in memory at several times its byte size, so the cap
	// is really a cap on what one poll can allocate, multiplied by however many
	// polls run at once.
	MaxFeedBytes = 2 << 20
)

// Entry is a normalised feed item.
type Entry struct {
	GUID      string
	URL       string
	Title     string
	Summary   string
	Author    string
	Published *time.Time
}

// Result is the outcome of one fetch.
type Result struct {
	NotModified  bool
	Title        string
	ETag         string
	LastModified string
	// Hint is how long the server asked to be left alone, from Cache-Control
	// max-age. Zero when it did not say.
	Hint time.Duration
	// Unchanged reports that the body was byte-for-byte what was fetched last
	// time, so it was not parsed and Entries is empty. Distinct from
	// NotModified, which is the server saying so and costs no body at all.
	Unchanged bool
	// BodyHash identifies the document that was parsed, for the caller to store
	// and hand back on the next fetch.
	BodyHash []byte
	// Entries are ordered oldest first, so they are posted in the order they
	// were published.
	Entries []Entry
}

type Fetcher struct {
	http *safehttp.Client
}

func NewFetcher(hc *safehttp.Client) *Fetcher { return &Fetcher{http: hc} }

// Fetch retrieves a feed, using conditional headers when the caller has them.
//
// prevHash is the digest of the body parsed last time, or nil. Plenty of servers
// support neither ETag nor Last-Modified and answer 200 with identical bytes
// forever; without this that costs a full parse and a few hundred no-op inserts
// on every interval, for a document that has not moved since March. Hashing the
// body is a few microseconds and turns all of that into a comparison.
func (f *Fetcher) Fetch(ctx context.Context, feedURL, etag, lastModified string, prevHash []byte) (*Result, error) {
	got, err := f.get(ctx, feedURL, etag, lastModified)
	if err != nil {
		return nil, err
	}
	if got.notModified {
		return &Result{
			NotModified:  true,
			ETag:         etag,
			LastModified: lastModified,
			Hint:         got.hint,
			BodyHash:     prevHash,
		}, nil
	}

	sum := sha256.Sum256(got.body)
	hash := sum[:]
	if len(prevHash) > 0 && bytes.Equal(prevHash, hash) {
		return &Result{
			Unchanged:    true,
			ETag:         got.etag,
			LastModified: got.lastModified,
			Hint:         got.hint,
			BodyHash:     hash,
		}, nil
	}

	res, err := Parse(got.body, feedURL)
	if err != nil {
		return nil, err
	}
	res.ETag, res.LastModified, res.Hint = got.etag, got.lastModified, got.hint
	res.BodyHash = hash
	return res, nil
}

// response is one retrieved document, before anything has decided what it is.
// Fetch parses it as a feed; Resolve may also read it as a page linking to one,
// which is why the body is handed back rather than parsed on the way through.
type response struct {
	notModified  bool
	body         []byte
	etag         string
	lastModified string
	hint         time.Duration
}

func (f *Fetcher) get(ctx context.Context, feedURL, etag, lastModified string) (*response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feedURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/atom+xml, application/rss+xml, application/feed+json, application/xml;q=0.9, */*;q=0.8")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if lastModified != "" {
		req.Header.Set("If-Modified-Since", lastModified)
	}

	resp, err := f.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return &response{notModified: true, hint: cacheHint(resp.Header.Get("Cache-Control"))}, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// 429 and 503 usually carry Retry-After. Honouring it is the whole
		// difference between backing off and being rude.
		return nil, &FetchError{
			StatusCode: resp.StatusCode,
			RetryAfter: retryAfter(resp.Header.Get("Retry-After")),
		}
	}

	body, err := f.http.ReadLimited(resp, MaxFeedBytes)
	if err != nil {
		return nil, err
	}
	return &response{
		body:         body,
		etag:         trimHeader(resp.Header.Get("ETag"), 200),
		lastModified: trimHeader(resp.Header.Get("Last-Modified"), 100),
		hint:         cacheHint(resp.Header.Get("Cache-Control")),
	}, nil
}

// FetchError is a non-2xx response, carrying the server's own retry request.
type FetchError struct {
	StatusCode int
	RetryAfter time.Duration
}

func (e *FetchError) Error() string {
	return fmt.Sprintf("feed returned %d", e.StatusCode)
}

// retryAfter parses the header in both of its forms: a delay in seconds, or an
// HTTP date.
func retryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// cacheHint extracts max-age from a Cache-Control header. A server that says
// it caches for an hour is telling us not to ask again for an hour.
func cacheHint(v string) time.Duration {
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(strings.ToLower(part))
		if part == "no-cache" || part == "no-store" {
			return 0
		}
		value, ok := strings.CutPrefix(part, "max-age=")
		if !ok {
			continue
		}
		secs, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	return 0
}

// parsers reuses gofeed parsers across polls. Each one carries three format
// parsers and their translators, all of which are allocated by NewParser and
// none of which hold state between documents, so building a fresh set per fetch
// was garbage the collector had to deal with on every poll of every feed.
var parsers = sync.Pool{New: func() any { return gofeed.NewParser() }}

// Parse normalises a feed document. base is the feed URL, used to resolve
// relative entry links.
func Parse(body []byte, base string) (*Result, error) {
	p := parsers.Get().(*gofeed.Parser)
	parsed, err := p.Parse(bytes.NewReader(body))
	parsers.Put(p)
	if err != nil {
		return nil, fmt.Errorf("could not read that feed: %w", err)
	}
	baseURL, _ := url.Parse(base)

	// collapse, not just TrimSpace: the feed's own title reaches the dashboard and
	// post text the same way entry titles do, so it needs the same strip of
	// markup, control characters and bidi overrides. Trimming alone let a feed
	// publish direction-reversed text under the account holder's name.
	res := &Result{Title: clip(collapse(parsed.Title), MaxTitleLen)}
	for _, item := range parsed.Items {
		if e, ok := normalize(item, baseURL); ok {
			res.Entries = append(res.Entries, e)
		}
	}

	// Feeds are conventionally newest first, but not reliably, and some omit
	// dates entirely. Sort by date where available and keep the original
	// document order as the tie-break, reversed so the oldest goes first.
	sort.SliceStable(res.Entries, func(i, j int) bool {
		a, b := res.Entries[i].Published, res.Entries[j].Published
		if a == nil || b == nil {
			return false
		}
		return a.Before(*b)
	})
	if allUndated(res.Entries) {
		reverse(res.Entries)
	}

	// Truncate after sorting, keeping the newest.
	//
	// Cutting the document at MaxEntries before the sort throws away whichever
	// end the publisher happened to put last, so a feed written oldest-first
	// with more than MaxEntries lost exactly the entries worth having. That was
	// survivable while every poll re-read the whole document and might sort
	// differently; with the body hash, an unchanged feed is not parsed again, so
	// the loss is permanent.
	//
	// The count is bounded by MaxFeedBytes long before it reaches here, so
	// normalising the whole document first is not a way to make this allocate.
	if len(res.Entries) > MaxEntries {
		res.Entries = res.Entries[len(res.Entries)-MaxEntries:]
	}
	return res, nil
}

func normalize(item *gofeed.Item, base *url.URL) (Entry, bool) {
	if item == nil {
		return Entry{}, false
	}
	e := Entry{
		Title:  clip(collapse(item.Title), MaxTitleLen),
		Author: clip(collapse(authorOf(item)), 200),
	}

	e.URL = absoluteLink(item.Link, base)
	if e.URL == "" {
		for _, l := range item.Links {
			if e.URL = absoluteLink(l, base); e.URL != "" {
				break
			}
		}
	}

	summary := item.Description
	if strings.TrimSpace(summary) == "" {
		summary = item.Content
	}
	e.Summary = clip(render.StripHTML(summary), MaxSummaryLen)

	if item.PublishedParsed != nil {
		e.Published = utc(item.PublishedParsed)
	} else if item.UpdatedParsed != nil {
		e.Published = utc(item.UpdatedParsed)
	}

	e.GUID = entryGUID(item, e)
	if e.GUID == "" {
		return Entry{}, false
	}
	if e.Title == "" && e.Summary == "" && e.URL == "" {
		return Entry{}, false
	}
	if e.Title == "" {
		e.Title = "(untitled)"
	}
	return e, true
}

// entryGUID picks the most stable identifier available. Falling back to a hash
// of the content means an entry with no id and no link is still only posted
// once, as long as it does not change.
func entryGUID(item *gofeed.Item, e Entry) string {
	if g := strings.TrimSpace(item.GUID); g != "" {
		return clip(g, MaxGUIDLen)
	}
	if e.URL != "" {
		return clip(e.URL, MaxGUIDLen)
	}
	if e.Title == "" && e.Summary == "" {
		return ""
	}
	published := ""
	if e.Published != nil {
		published = e.Published.Format(time.RFC3339)
	}
	sum := sha256.Sum256([]byte(e.Title + "\x00" + published + "\x00" + e.Summary))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// absoluteLink resolves a link against the feed URL and rejects anything that
// is not http(s) — an entry link ends up in post text and in href attributes.
func absoluteLink(raw string, base *url.URL) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > MaxURLLen {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if !u.IsAbs() && base != nil {
		u = base.ResolveReference(u)
	}
	if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return ""
	}
	u.User = nil
	return u.String()
}

func authorOf(item *gofeed.Item) string {
	if item.Author != nil && item.Author.Name != "" {
		return item.Author.Name
	}
	for _, a := range item.Authors {
		if a != nil && a.Name != "" {
			return a.Name
		}
	}
	return ""
}

func allUndated(entries []Entry) bool {
	for _, e := range entries {
		if e.Published != nil {
			return false
		}
	}
	return true
}

func reverse(entries []Entry) {
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	// A feed can claim any date. A far-future date would sort an old entry to
	// the front forever, so clamp it.
	v := t.UTC()
	if v.After(time.Now().UTC().Add(24 * time.Hour)) {
		v = time.Now().UTC()
	}
	return &v
}

func collapse(s string) string {
	return strings.TrimSpace(render.StripHTML(s))
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// trimHeader keeps a response header value short and free of control bytes
// before it is stored and echoed back on the next conditional request.
func trimHeader(v string, n int) string {
	v = strings.TrimSpace(v)
	if strings.ContainsAny(v, "\r\n\x00") {
		return ""
	}
	if len(v) > n {
		return ""
	}
	return v
}

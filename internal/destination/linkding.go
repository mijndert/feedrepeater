package destination

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"feedrepeater.com/internal/safehttp"
)

// LinkdingLimit is the length of the bookmark description this service will
// send, in runes.
//
// Unlike every other kind this is our own limit rather than the service's:
// linkding stores a description as unbounded text. A bookmark list is read at a
// glance though, so anything past this is a page rather than a subtitle, and the
// same budget as ntfy is more than a timeline-shaped template will ever use.
const LinkdingLimit = 1024

// linkdingTitleLimit is linkding's own maximum for a bookmark title.
const linkdingTitleLimit = 512

// maxLinkdingTags bounds how many tags one destination attaches. Tags are a
// label on a bookmark, not a place to put a feed's keywords, and every one of
// them is created in linkding on first use.
const maxLinkdingTags = 10

// LinkdingConfig is the non-secret half of a linkding destination. None of it
// can write a bookmark: the address is public by nature, and the tags and the
// unread flag are choices about what a stored bookmark looks like.
type LinkdingConfig struct {
	Server string   `json:"server"`
	Tags   []string `json:"tags"`
	Unread bool     `json:"unread"`
}

// LinkdingCredentials is the encrypted half: the REST API token from linkding's
// own settings. It grants full access to that account's bookmarks, so it is
// sealed like a Bluesky app password and never rendered back.
type LinkdingCredentials struct {
	Token string `json:"token"`
}

// linkdingPathPattern is the shape a context path may take. linkding can be
// served under a prefix (LD_CONTEXT_PATH), so one is kept rather than dropped,
// but only in the plain form a prefix actually has: with nothing needing escaping
// in it, the stored address and the address requested cannot differ.
var linkdingPathPattern = regexp.MustCompile(`^(/[A-Za-z0-9._~-]+)*$`)

// reservedLinkdingPaths are the app's own routes. The address wanted here is the
// one linkding is served from, and someone reading it off their browser is
// looking at a page inside it instead. Storing that would build every API
// request under a prefix linkding has never heard of, so it is refused with the
// address that was meant rather than accepted and left to 404.
var reservedLinkdingPaths = map[string]bool{
	"admin": true, "api": true, "assets": true, "bookmarks": true,
	"login": true, "logout": true, "settings": true, "static": true,
}

// ParseLinkdingServer checks the server address and reduces it to an origin,
// plus a context path where there is one.
//
// https only, because the API token is a bearer credential: it travels on every
// request and grants read and write over the whole bookmark collection.
func ParseLinkdingServer(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("enter the address your linkding is served from")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("that is not a URL")
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("a linkding address starts with https://")
	}
	if u.User != nil {
		return nil, fmt.Errorf("that URL must not contain credentials")
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("that address has no host")
	}
	// Refused rather than dropped, as with ntfy: discarding part of what someone
	// pasted and saying nothing is how a typo becomes a destination that works by
	// accident.
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("enter the address on its own, with nothing after it")
	}

	path := strings.TrimSuffix(u.EscapedPath(), "/")
	if !linkdingPathPattern.MatchString(path) {
		return nil, fmt.Errorf("enter the address linkding is served from, without a path")
	}
	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if reservedLinkdingPaths[strings.ToLower(segments[0])] {
		return nil, fmt.Errorf("that is a page inside linkding; enter the address it is served from: https://%s", u.Host)
	}
	// A dot segment is made only of characters the pattern allows, and it is the
	// one way a stored prefix and the address actually requested can differ: a
	// server resolves "/../api/bookmarks/" to "/api/bookmarks/", so the prefix the
	// form shows back would not be the one in use. It also walks straight past the
	// reserved check above.
	for _, seg := range segments {
		if seg == "." || seg == ".." {
			return nil, fmt.Errorf("enter the address linkding is served from, without a path")
		}
	}
	// Rebuilt rather than passed through, so nothing outside the host and the
	// prefix survives into what gets stored. The host is lowered because a host is
	// case-insensitive: without this, correcting the case of a stored address would
	// read as a move and ask for the token again.
	return &url.URL{Scheme: "https", Host: strings.ToLower(u.Host), Path: path}, nil
}

// ParseLinkdingTags reads the tags field into the names linkding will store.
//
// Splitting on spaces as well as commas is not a convenience: a linkding tag
// name cannot contain whitespace, so "read later" is two tags there however it
// was meant here. Splitting the same way it does means what the form shows back
// is what the bookmark will carry.
func ParseLinkdingTags(raw string) ([]string, error) {
	// Gated on the raw field before anything is split, because splitting is what a
	// large field is expensive for: ten names of 64 characters plus separators fit
	// in well under this, so anything past it is not a tag list. Every other
	// user-supplied field here has a cheap gate ahead of its O(n) work — the
	// template's rune count, the topic's anchored pattern, the token's length — and
	// this one did not.
	if len(raw) > 1024 {
		return nil, fmt.Errorf("that is more than %d tags", maxLinkdingTags)
	}
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || unicode.IsSpace(r)
	})
	// Sized from the limit rather than from the input, which is the other half of
	// the same problem: a field of a million one-character words dedupes to one tag,
	// so the count below never refuses it, and pre-sizing from the split would have
	// allocated for a million anyway.
	out := make([]string, 0, maxLinkdingTags+1)
	seen := make(map[string]bool, maxLinkdingTags+1)
	for _, f := range fields {
		// linkding writes a tag as #name in its own search box, so a name pasted
		// from there arrives with one. It is not part of the name.
		f = strings.TrimPrefix(f, "#")
		if f == "" {
			continue
		}
		if utf8.RuneCountInString(f) > 64 {
			return nil, fmt.Errorf("a tag name is up to 64 characters: %q", clipRunes(f, 24))
		}
		// Splitting on whitespace leaves the invisible characters that are not
		// whitespace, and a name is shown back in the form as well as sent to
		// linkding: a bidi override reverses the rest of the list on screen. Every
		// other text path here drops these rather than keeping them — see clean() in
		// internal/render.
		if invisible(f) {
			return nil, fmt.Errorf("a tag name cannot contain invisible characters")
		}
		// linkding matches tags case-insensitively, so two spellings of one name
		// are one tag; the first spelling is the one kept.
		if key := strings.ToLower(f); !seen[key] {
			seen[key] = true
			out = append(out, f)
		}
		if len(out) > maxLinkdingTags {
			return nil, fmt.Errorf("that is more than %d tags", maxLinkdingTags)
		}
	}
	return out, nil
}

// invisible reports whether a string carries a character that does not show what
// it is: a control character, a zero-width one, or a direction override. The set
// is the one internal/render strips out of feed text, kept in step with it.
func invisible(s string) bool {
	for _, r := range s {
		switch {
		case r == 0xFEFF, r == 0x200B, r == 0x200C, r == 0x200D, r == 0x2060, r == 0xAD:
			return true
		case r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069:
			return true
		case unicode.IsControl(r):
			return true
		}
	}
	return false
}

// ValidLinkdingToken checks the token's shape.
//
// It travels in an Authorization header, so a value carrying a newline or a
// control character is refused here rather than left for net/http to reject at
// send time, when the answer is a failed delivery instead of a form error.
func ValidLinkdingToken(token string) error {
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("enter the REST API token from linkding's settings")
	}
	if len(token) > 300 {
		return fmt.Errorf("that token is too long")
	}
	for _, r := range token {
		if r < 0x21 || r > 0x7e {
			return fmt.Errorf("that does not look like a linkding API token; copy it from Settings, Integrations")
		}
	}
	return nil
}

type linkdingTarget struct {
	http  *safehttp.Client
	base  string
	cfg   LinkdingConfig
	token string
}

// NewLinkding builds a linkding target from stored configuration. As with every
// other destination the stored address and token are re-checked rather than
// trusted: DNS can change under a stored name, and a row edited by hand must not
// become a request to somewhere else.
func NewLinkding(hc *safehttp.Client, configJSON string, credentials []byte) (Target, error) {
	var cfg LinkdingConfig
	if err := jsonConfig(configJSON, &cfg); err != nil {
		return nil, Permanent(err)
	}
	u, err := ParseLinkdingServer(cfg.Server)
	if err != nil {
		return nil, Permanent(err)
	}
	if err := hc.ValidateURL(u); err != nil {
		return nil, Permanent(err)
	}
	var creds LinkdingCredentials
	if err := json.Unmarshal(credentials, &creds); err != nil || creds.Token == "" {
		return nil, Permanentf("linkding: no API token stored")
	}
	if err := ValidLinkdingToken(creds.Token); err != nil {
		return nil, Permanentf("linkding: stored token is not usable: %v", err)
	}
	// Stored tags go back through the same split as the form, because a stored
	// name carrying a space is two tags to linkding rather than the one it looks
	// like here.
	tags, err := ParseLinkdingTags(strings.Join(cfg.Tags, ","))
	if err != nil {
		return nil, Permanentf("linkding: stored tags are not usable: %v", err)
	}
	cfg.Tags = tags
	return &linkdingTarget{http: hc, base: u.String(), cfg: cfg, token: creds.Token}, nil
}

func (t *linkdingTarget) Limit(context.Context) int { return LinkdingLimit }

// linkdingBookmark is the create-bookmark body.
//
// The entry's address and title are their own fields rather than text in the
// description, because that is what a bookmark is: linkding shows the title as
// the link and the description under it. The post template fills the
// description, which is the one part a person chooses.
type linkdingBookmark struct {
	URL         string   `json:"url"`
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	TagNames    []string `json:"tag_names,omitempty"`
	Unread      bool     `json:"unread"`
}

func (t *linkdingTarget) Send(ctx context.Context, p Post) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// A bookmark is an address, so an entry without a usable one is nothing this
	// destination can store. The check is the same one the ntfy click target
	// gets: this URL is handed to another application rather than fetched here,
	// and only http and https are links.
	link := clickURL(p.Item.URL)
	if link == "" {
		return nil, Permanentf("linkding: that entry has no http link to bookmark")
	}

	err := postLinkdingBookmark(ctx, t.http, t.base, t.token, linkdingBookmark{
		URL:         link,
		Title:       clipRunes(p.Item.Title, linkdingTitleLimit),
		Description: p.Text,
		TagNames:    t.cfg.Tags,
		Unread:      t.cfg.Unread,
	})
	if err != nil {
		return nil, err
	}
	// linkding has no per-bookmark page to link to, so the dashboard shows the
	// delivery without one.
	return &Result{}, nil
}

// postLinkdingBookmark creates one bookmark and maps the outcome onto retry
// semantics.
//
// There is no idempotency key, and none is needed: linkding keys a bookmark on
// its URL and updates the existing one when that URL is already saved. A retry
// after a request that actually arrived rewrites the same bookmark rather than
// making a second one, which is the guarantee Discord and Slack cannot give.
func postLinkdingBookmark(ctx context.Context, hc *safehttp.Client, base, token string, bm linkdingBookmark) error {
	raw, err := json.Marshal(bm)
	if err != nil {
		return Permanent(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/bookmarks/", bytes.NewReader(raw))
	if err != nil {
		return Permanent(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Token "+token)

	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("linkding: %w", err)
	}
	defer resp.Body.Close()
	body, _ := safehttp.ReadLimited(resp.Body, 64<<10)

	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		// The token was revoked, or the account it belonged to is gone. Neither
		// improves by trying again.
		return Permanentf("linkding: that API token was refused; create a new one in linkding's settings%s", detail(body))
	case http.StatusNotFound:
		return Permanentf("linkding: that address has no bookmarks API%s", detail(body))
	case http.StatusBadRequest:
		// linkding validated the bookmark and said no — an address it will not
		// accept, most likely. The same entry will be refused every time.
		return Permanentf("linkding: that bookmark was rejected%s", detail(body))
	case http.StatusTooManyRequests:
		return WithRetryAfter(
			statusError("linkding", resp.StatusCode, detail(body)),
			headerRetryAfter(resp.Header.Get("Retry-After")),
		)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return statusError("linkding", resp.StatusCode, detail(body))
	}
	return nil
}

// VerifyLinkding confirms a linkding destination before it is stored.
//
// The address is supplied by whoever is adding the destination, which is the
// shape that makes a destination an open relay if it is not checked: sign up,
// name a stranger's server, and feedrepeater posts to it on a schedule. linkding
// answers /api/user/profile/ with that account's own preferences, and only for a
// token it recognises, so requiring that answer proves both halves at once — the
// address is a cooperating linkding, and the token works on it.
//
// It is the quietest of the checks here, and it is worth being exact about where
// it sits as a relay barrier. Slack has to post a line and ntfy has to publish a
// notification, because neither can be asked whether a credential works without
// using it; this reads one document and creates nothing, so connecting leaves no
// bookmark behind to delete. But a document shape is forgeable by anyone willing
// to serve it, unlike Discord's and Slack's pinned hosts or the webhook's echo of
// a fresh value, so as a barrier it ranks below those two and above ntfy's health
// probe — stronger than that one only because it proves the credential too.
// What passing it with a forged document buys is one HTTPS POST per entry to a
// server the attacker already controls, which is strictly less than the webhook
// kind grants on purpose.
func VerifyLinkding(ctx context.Context, hc *safehttp.Client, server, token string) error {
	u, err := ParseLinkdingServer(server)
	if err != nil {
		return err
	}
	if err := ValidLinkdingToken(token); err != nil {
		return err
	}
	return verifyLinkding(ctx, hc, u.String(), token)
}

// verifyLinkding takes the address as given. Callers reach it through
// VerifyLinkding, which validates it first.
func verifyLinkding(ctx context.Context, hc *safehttp.Client, base, token string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	endpoint := base + "/api/user/profile/"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Token "+token)

	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach that server: %w", err)
	}
	defer resp.Body.Close()
	body, err := safehttp.ReadLimited(resp.Body, 64<<10)
	if err != nil {
		return fmt.Errorf("that server returned too much data")
	}

	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("linkding refused that API token; copy it again from Settings, Integrations")
	case http.StatusNotFound:
		// Named in full, because the usual cause is an address that is nearly
		// right: a page inside linkding, or a prefix it is not served under.
		return fmt.Errorf("nothing answered at %s, so linkding is not served from that address", endpoint)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("that server answered %d, so it is not a linkding", resp.StatusCode)
	}
	if !looksLikeLinkding(body) {
		return fmt.Errorf("that address answered, but not as a linkding")
	}
	return nil
}

// linkdingProfileFields are keys linkding's own profile document carries.
var linkdingProfileFields = []string{
	"theme", "bookmark_date_display", "bookmark_link_target",
	"tag_search", "enable_sharing", "web_archive_integration",
}

// looksLikeLinkding reports whether a profile document is linkding's own.
//
// Two matching keys rather than one, so a server that answers `{}` or a generic
// JSON API that ignores the path does not pass. Two rather than all of them, so
// a linkding release that renames a preference does not lock everybody out of
// connecting.
func looksLikeLinkding(body []byte) bool {
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) != nil {
		return false
	}
	found := 0
	for _, field := range linkdingProfileFields {
		if _, ok := doc[field]; ok {
			found++
		}
	}
	return found >= 2
}

package destination

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"feedrepeater.com/internal/safehttp"
)

// BlueskyLimit is the post length Bluesky enforces, in graphemes. Rune counts
// are always greater than or equal to grapheme counts, so measuring in runes
// keeps us on the safe side of the limit without a segmentation library.
const BlueskyLimit = 300

// PublicAPI is the AT Protocol appview used for handle resolution. It is a
// fixed, trusted host, unlike the PDS which is derived from user data.
const PublicAPI = "https://public.api.bsky.app"

// PLCDirectory resolves did:plc identifiers.
const PLCDirectory = "https://plc.directory"

// BlueskyConfig is the non-secret half of a Bluesky destination.
type BlueskyConfig struct {
	Handle string `json:"handle"`
	DID    string `json:"did"`
	PDS    string `json:"pds"`
}

// BlueskyCredentials is the encrypted half. Bluesky app passwords are
// long-lived bearer credentials, so they are stored encrypted and never shown
// again after they are saved.
type BlueskyCredentials struct {
	AppPassword string `json:"app_password"`
}

var (
	didPLC = regexp.MustCompile(`^did:plc:[a-z2-7]{24}$`)
	didWeb = regexp.MustCompile(`^did:web:[a-zA-Z0-9.\-]{1,253}$`)
)

// ValidDID reports whether a DID is one of the two methods AT Protocol uses.
func ValidDID(did string) bool {
	return didPLC.MatchString(did) || didWeb.MatchString(did)
}

type blueskyTarget struct {
	http *safehttp.Client
	cfg  BlueskyConfig
	pass string
}

// NewBluesky builds a Bluesky target from stored configuration.
func NewBluesky(hc *safehttp.Client, configJSON string, credentials []byte) (Target, error) {
	var cfg BlueskyConfig
	if err := jsonConfig(configJSON, &cfg); err != nil {
		return nil, Permanent(err)
	}
	if !ValidDID(cfg.DID) {
		return nil, Permanentf("bluesky: stored account identifier is invalid")
	}
	if _, err := publicURL(hc, cfg.PDS); err != nil {
		return nil, Permanentf("bluesky: stored server address is not usable: %v", err)
	}
	var creds BlueskyCredentials
	if err := json.Unmarshal(credentials, &creds); err != nil || creds.AppPassword == "" {
		return nil, Permanentf("bluesky: no app password stored")
	}
	return &blueskyTarget{http: hc, cfg: cfg, pass: creds.AppPassword}, nil
}

func (t *blueskyTarget) Limit(context.Context) int { return BlueskyLimit }

func (t *blueskyTarget) Send(ctx context.Context, p Post) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	token, err := blueskySession(ctx, t.http, t.cfg.PDS, t.cfg.Handle, t.cfg.DID, t.pass)
	if err != nil {
		return nil, err
	}

	record := map[string]any{
		"$type":     "app.bsky.feed.post",
		"text":      p.Text,
		"createdAt": time.Now().UTC().Format(time.RFC3339),
	}
	if f := LinkFacets(p.Text); len(f) > 0 {
		record["facets"] = f
	}
	body := map[string]any{
		"repo":       t.cfg.DID,
		"collection": "app.bsky.feed.post",
		"record":     record,
	}

	var out struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	}
	if err := t.call(ctx, "com.atproto.repo.createRecord", token, body, &out); err != nil {
		return nil, err
	}
	return &Result{URL: blueskyPostURL(t.cfg.Handle, out.URI)}, nil
}

func (t *blueskyTarget) call(ctx context.Context, method, token string, in, out any) error {
	return xrpcPost(ctx, t.http, t.cfg.PDS, method, token, in, out)
}

// --- session cache ---------------------------------------------------------

type cachedSession struct {
	token   string
	expires time.Time
}

var (
	sessionMu    sync.Mutex
	sessionCache = map[string]cachedSession{}
)

// blueskySession returns an access token, reusing a cached one when it is still
// fresh. The cache key includes a hash of the app password so a changed
// password cannot keep using the old session.
func blueskySession(ctx context.Context, hc *safehttp.Client, pds, handle, did, pass string) (string, error) {
	sum := sha256.Sum256([]byte(did + "\x00" + pass))
	key := hex.EncodeToString(sum[:])

	sessionMu.Lock()
	if s, ok := sessionCache[key]; ok && time.Now().Before(s.expires) {
		sessionMu.Unlock()
		return s.token, nil
	}
	sessionMu.Unlock()

	identifier := handle
	if identifier == "" {
		identifier = did
	}
	var out struct {
		AccessJWT string `json:"accessJwt"`
		DID       string `json:"did"`
	}
	err := xrpcPost(ctx, hc, pds, "com.atproto.server.createSession", "",
		map[string]any{"identifier": identifier, "password": pass}, &out)
	if err != nil {
		return "", err
	}
	if out.AccessJWT == "" {
		return "", Permanentf("bluesky: server returned no session")
	}
	// The server must agree about which account this is; if it does not, the
	// post would be written to a repository the user did not configure.
	if out.DID != did {
		return "", Permanentf("bluesky: credentials belong to a different account")
	}

	sessionMu.Lock()
	sessionCache[key] = cachedSession{token: out.AccessJWT, expires: time.Now().Add(50 * time.Minute)}
	if len(sessionCache) > 1000 {
		for k, v := range sessionCache {
			if time.Now().After(v.expires) {
				delete(sessionCache, k)
			}
		}
	}
	sessionMu.Unlock()

	return out.AccessJWT, nil
}

// CheckBlueskyLogin verifies a handle and app password without posting.
func CheckBlueskyLogin(ctx context.Context, hc *safehttp.Client, cfg BlueskyConfig, appPassword string) error {
	if !ValidDID(cfg.DID) {
		return fmt.Errorf("bluesky: invalid account identifier")
	}
	if _, err := publicURL(hc, cfg.PDS); err != nil {
		return err
	}
	_, err := blueskySession(ctx, hc, cfg.PDS, cfg.Handle, cfg.DID, appPassword)
	return err
}

// ForgetBlueskySession drops any cached token for an account.
func ForgetBlueskySession(did, pass string) {
	sum := sha256.Sum256([]byte(did + "\x00" + pass))
	sessionMu.Lock()
	delete(sessionCache, hex.EncodeToString(sum[:]))
	sessionMu.Unlock()
}

// --- identity resolution ---------------------------------------------------

// ResolveBlueskyAccount turns a handle into the DID and PDS endpoint needed to
// post. Both the DID document and the endpoint it names are user-controlled,
// so the endpoint is validated before it is ever fetched.
func ResolveBlueskyAccount(ctx context.Context, hc *safehttp.Client, handle string) (BlueskyConfig, error) {
	var cfg BlueskyConfig

	handle = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(handle), "@"))
	handle = strings.ToLower(handle)
	if handle == "" || !strings.Contains(handle, ".") || len(handle) > 253 {
		return cfg, fmt.Errorf("enter a full handle, for example name.bsky.social")
	}
	for _, r := range handle {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '-') {
			return cfg, fmt.Errorf("that does not look like a Bluesky handle")
		}
	}

	var res struct {
		DID string `json:"did"`
	}
	q := url.Values{"handle": {handle}}
	if err := getJSON(ctx, hc, PublicAPI+"/xrpc/com.atproto.identity.resolveHandle?"+q.Encode(), &res); err != nil {
		return cfg, fmt.Errorf("could not find that handle")
	}
	if !ValidDID(res.DID) {
		return cfg, fmt.Errorf("could not find that handle")
	}

	pds, err := resolvePDS(ctx, hc, res.DID)
	if err != nil {
		return cfg, err
	}
	return BlueskyConfig{Handle: handle, DID: res.DID, PDS: pds}, nil
}

type didDocument struct {
	ID      string `json:"id"`
	Service []struct {
		ID              string `json:"id"`
		Type            string `json:"type"`
		ServiceEndpoint string `json:"serviceEndpoint"`
	} `json:"service"`
}

func resolvePDS(ctx context.Context, hc *safehttp.Client, did string) (string, error) {
	var docURL string
	switch {
	case didPLC.MatchString(did):
		docURL = PLCDirectory + "/" + did
	case didWeb.MatchString(did):
		host := strings.TrimPrefix(did, "did:web:")
		u := &url.URL{Scheme: "https", Host: host, Path: "/.well-known/did.json"}
		if err := hc.ValidateURL(u); err != nil {
			return "", fmt.Errorf("that account's server is not reachable")
		}
		docURL = u.String()
	default:
		return "", fmt.Errorf("unsupported account identifier")
	}

	var doc didDocument
	if err := getJSON(ctx, hc, docURL, &doc); err != nil {
		return "", fmt.Errorf("could not look up that account")
	}
	for _, svc := range doc.Service {
		if !strings.HasSuffix(svc.ID, "atproto_pds") && svc.Type != "AtprotoPersonalDataServer" {
			continue
		}
		u, err := url.Parse(svc.ServiceEndpoint)
		if err != nil || u.Scheme != "https" {
			continue
		}
		// The endpoint comes from a document the account holder controls, so it
		// is checked with the same rules as any other user-supplied URL.
		if err := hc.ValidateURL(u); err != nil {
			return "", fmt.Errorf("that account's server address is not allowed")
		}
		u.Path = strings.TrimSuffix(u.Path, "/")
		u.RawQuery, u.Fragment = "", ""
		return u.String(), nil
	}
	return "", fmt.Errorf("that account has no server to post to")
}

// --- rich text -------------------------------------------------------------

// facet is an AT Protocol rich-text annotation. Its offsets are byte indexes
// into the UTF-8 encoding of the post text, not rune or grapheme indexes.
type facet struct {
	Index    facetIndex         `json:"index"`
	Features []facetLinkFeature `json:"features"`
}

type facetIndex struct {
	ByteStart int `json:"byteStart"`
	ByteEnd   int `json:"byteEnd"`
}

type facetLinkFeature struct {
	Type string `json:"$type"`
	URI  string `json:"uri"`
}

var linkPattern = regexp.MustCompile(`https?://[^\s<>"'` + "`" + `]+`)

// LinkFacets finds links in post text and returns their byte ranges so Bluesky
// renders them as links.
func LinkFacets(text string) []facet {
	var out []facet
	for _, loc := range linkPattern.FindAllStringIndex(text, -1) {
		start, end := loc[0], loc[1]
		// Trailing punctuation is usually sentence punctuation, not part of the
		// URL — except a closing bracket that balances an opening one.
		for end > start {
			last := text[end-1]
			if strings.IndexByte(".,;:!?'\"", last) >= 0 {
				end--
				continue
			}
			if last == ')' && strings.Count(text[start:end], "(") < strings.Count(text[start:end], ")") {
				end--
				continue
			}
			break
		}
		raw := text[start:end]
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			continue
		}
		out = append(out, facet{
			Index:    facetIndex{ByteStart: start, ByteEnd: end},
			Features: []facetLinkFeature{{Type: "app.bsky.richtext.facet#link", URI: raw}},
		})
	}
	return out
}

// blueskyPostURL builds the web URL for an at:// record URI.
func blueskyPostURL(handle, atURI string) string {
	i := strings.LastIndex(atURI, "/")
	if i < 0 || i == len(atURI)-1 {
		return ""
	}
	rkey := atURI[i+1:]
	if handle == "" {
		return ""
	}
	return "https://bsky.app/profile/" + url.PathEscape(handle) + "/post/" + url.PathEscape(rkey)
}

// --- transport -------------------------------------------------------------

func xrpcPost(ctx context.Context, hc *safehttp.Client, base, method, token string, in, out any) error {
	payload, err := json.Marshal(in)
	if err != nil {
		return Permanent(err)
	}
	endpoint := strings.TrimSuffix(base, "/") + "/xrpc/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return Permanent(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("bluesky: %w", err)
	}
	defer resp.Body.Close()

	body, err := safehttp.ReadLimited(resp.Body, 1<<20)
	if err != nil {
		return fmt.Errorf("bluesky: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return statusError("bluesky", resp.StatusCode, xrpcError(body))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("bluesky: unreadable response")
	}
	return nil
}

func getJSON(ctx context.Context, hc *safehttp.Client, endpoint string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := safehttp.ReadLimited(resp.Body, 1<<20)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("request to %s returned %d", req.URL.Host, resp.StatusCode)
	}
	return json.Unmarshal(body, out)
}

func xrpcError(body []byte) string {
	var e struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) != nil || e.Error == "" {
		return ""
	}
	msg := e.Error
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if r := []rune(msg); len(r) > 200 {
		msg = string(r[:200]) + "…"
	}
	return " (" + strings.Map(printable, msg) + ")"
}

func printable(r rune) rune {
	if r < 0x20 || r == 0x7f {
		return ' '
	}
	return r
}

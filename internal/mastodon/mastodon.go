// Package mastodon speaks the subset of the Mastodon API this service needs:
// dynamic app registration, the OAuth code flow, account verification, and
// posting statuses.
//
// Every instance is untrusted. It is chosen by whoever is signing in, it can
// return anything, and it may be hostile to its own users.
package mastodon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"golang.org/x/net/idna"

	"feedrepeater.com/internal/safehttp"
)

// Scopes requested at sign-in: enough to identify the account and to post on
// its behalf, and nothing else. No read of the timeline, no follow graph.
const Scopes = "read:accounts write:statuses"

// DefaultMaxCharacters is Mastodon's stock status length limit, used when an
// instance does not advertise its own.
const DefaultMaxCharacters = 500

type Client struct {
	http *safehttp.Client
}

func New(hc *safehttp.Client) *Client { return &Client{http: hc} }

// NormalizeHost turns whatever a person typed into a bare, punycode hostname.
//
// It accepts "mastodon.social", "https://mastodon.social/about", and
// "@alice@mastodon.social". Unicode is converted to ASCII so that two spellings
// of the same instance cannot become two different rows, and so a homograph
// cannot pose as a well-known instance in the UI.
func NormalizeHost(input string) (string, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return "", fmt.Errorf("enter an instance, for example mastodon.social")
	}
	// A bare handle such as @alice@mastodon.social: take the part after the
	// last @. This only applies when the input is not a URL — in a URL the @
	// may be userinfo or part of a path, and url.Parse is the authority on
	// which part is the host.
	if !strings.Contains(s, "/") {
		if i := strings.LastIndex(s, "@"); i >= 0 {
			s = s[i+1:]
		}
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("that does not look like an instance address")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", fmt.Errorf("that does not look like an instance address")
	}
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("that does not look like an instance address")
	}
	if u.Port() != "" {
		return "", fmt.Errorf("instance address must not include a port")
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return "", fmt.Errorf("instance address must be a domain name")
	}
	host, err = idna.Lookup.ToASCII(strings.TrimSuffix(strings.ToLower(host), "."))
	if err != nil {
		return "", fmt.Errorf("that does not look like an instance address")
	}
	if !strings.Contains(host, ".") || len(host) > 253 {
		return "", fmt.Errorf("that does not look like an instance address")
	}
	// The host is about to be used to build request URLs, so make sure it
	// survived normalisation as a plain hostname and nothing more.
	if strings.ContainsAny(host, "/\\?#@: ") {
		return "", fmt.Errorf("that does not look like an instance address")
	}
	if err := safehttp.ValidateURL(&url.URL{Scheme: "https", Host: host}); err != nil {
		return "", fmt.Errorf("that instance is not reachable")
	}
	return host, nil
}

// App is a registered OAuth client on one instance.
type App struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

// RegisterApp creates an OAuth client on the instance.
func (c *Client) RegisterApp(ctx context.Context, host, redirectURI, website string) (*App, error) {
	form := url.Values{
		"client_name":   {"feedrepeater"},
		"redirect_uris": {redirectURI},
		"scopes":        {Scopes},
		"website":       {website},
	}
	var app App
	if err := c.postForm(ctx, host, "/api/v1/apps", "", form, &app); err != nil {
		return nil, err
	}
	if app.ClientID == "" || app.ClientSecret == "" {
		return nil, fmt.Errorf("mastodon: %s returned an incomplete app registration", host)
	}
	return &app, nil
}

// AuthorizeURL builds the URL the browser is sent to.
func AuthorizeURL(host, clientID, redirectURI, state, challenge string) string {
	q := url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"scope":                 {Scopes},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	u := url.URL{Scheme: "https", Host: host, Path: "/oauth/authorize", RawQuery: q.Encode()}
	return u.String()
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Scope       string `json:"scope"`
}

// ExchangeCode trades an authorization code for an access token.
func (c *Client) ExchangeCode(ctx context.Context, host, clientID, clientSecret, code, redirectURI, verifier string) (string, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
		"scope":         {Scopes},
	}
	var tok tokenResponse
	if err := c.postForm(ctx, host, "/oauth/token", "", form, &tok); err != nil {
		return "", err
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("mastodon: %s returned no access token", host)
	}
	return tok.AccessToken, nil
}

// RevokeToken invalidates an access token at the instance.
//
// Each sign-in issues a new token, and the instance keeps every one it has
// ever issued until told otherwise. Without this, signing in repeatedly leaves
// a trail of live credentials that can post as the user, only the newest of
// which this service still knows about.
func (c *Client) RevokeToken(ctx context.Context, host, clientID, clientSecret, token string) error {
	form := url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"token":         {token},
	}
	return c.postForm(ctx, host, "/oauth/revoke", "", form, nil)
}

// Account is the subset of an account the UI shows. Everything here is
// instance-controlled and must be treated as display data only.
type Account struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	Acct        string `json:"acct"`
	DisplayName string `json:"display_name"`
	Avatar      string `json:"avatar"`
	URL         string `json:"url"`
}

// VerifyCredentials identifies the account behind a token.
func (c *Client) VerifyCredentials(ctx context.Context, host, token string) (*Account, error) {
	var a Account
	if err := c.get(ctx, host, "/api/v1/accounts/verify_credentials", token, &a); err != nil {
		return nil, err
	}
	if a.ID == "" {
		return nil, fmt.Errorf("mastodon: %s returned an account with no id", host)
	}
	if a.Acct == "" {
		a.Acct = a.Username
	}
	// A remote account (acct containing @) cannot be a local account of this
	// instance, so refuse it: the identity would be ambiguous.
	if strings.Contains(a.Acct, "@") {
		return nil, fmt.Errorf("mastodon: %s returned a remote account", host)
	}
	// Only keep an avatar we would be willing to render.
	if u, err := url.Parse(a.Avatar); err != nil || u.Scheme != "https" {
		a.Avatar = ""
	}
	return &a, nil
}

// MaxCharacters asks the instance for its status length limit.
func (c *Client) MaxCharacters(ctx context.Context, host string) int {
	var v2 struct {
		Configuration struct {
			Statuses struct {
				MaxCharacters int `json:"max_characters"`
			} `json:"statuses"`
		} `json:"configuration"`
	}
	if err := c.get(ctx, host, "/api/v2/instance", "", &v2); err == nil {
		// The instance is untrusted, and this number decides how much text is
		// composed on the user's behalf. No real deployment needs more than a
		// few thousand characters.
		if n := v2.Configuration.Statuses.MaxCharacters; n > 0 && n <= 5_000 {
			return n
		}
	}
	return DefaultMaxCharacters
}

// Status is a posted status.
type Status struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// PostStatus publishes a status. idempotencyKey lets a retried delivery
// collapse into the original post instead of duplicating it.
func (c *Client) PostStatus(ctx context.Context, host, token, text, visibility, idempotencyKey string) (*Status, error) {
	form := url.Values{
		"status":     {text},
		"visibility": {visibility},
	}
	var st Status
	if err := c.postForm(ctx, host, "/api/v1/statuses", token, form, &st, header{"Idempotency-Key", idempotencyKey}); err != nil {
		return nil, err
	}
	return &st, nil
}

// --- transport helpers -----------------------------------------------------

// HTTPError is a non-2xx response from an instance. The status code decides
// whether a caller retries.
type HTTPError struct {
	Host       string
	StatusCode int
	Detail     string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("mastodon: %s returned %d%s", e.Host, e.StatusCode, e.Detail)
}

type header struct{ key, value string }

func (c *Client) get(ctx context.Context, host, path, token string, out any) error {
	u := url.URL{Scheme: "https", Host: host, Path: path}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	return c.do(req, token, out)
}

func (c *Client) postForm(ctx context.Context, host, path, token string, form url.Values, out any, headers ...header) error {
	u := url.URL{Scheme: "https", Host: host, Path: path}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, h := range headers {
		if h.value != "" {
			req.Header.Set(h.key, h.value)
		}
	}
	return c.do(req, token, out)
}

func (c *Client) do(req *http.Request, token string, out any) error {
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("mastodon: %s: %w", req.URL.Host, err)
	}
	defer resp.Body.Close()

	body, err := c.http.ReadBody(resp)
	if err != nil {
		return fmt.Errorf("mastodon: %s: %w", req.URL.Host, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &HTTPError{Host: req.URL.Host, StatusCode: resp.StatusCode, Detail: apiError(body)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("mastodon: %s returned an unreadable response", req.URL.Host)
	}
	return nil
}

// apiError extracts the instance's own error message, kept short so a hostile
// instance cannot use it to inject a wall of text into the UI.
func apiError(body []byte) string {
	var e struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if json.Unmarshal(body, &e) != nil || e.Error == "" {
		return ""
	}
	msg := e.Error
	if e.Description != "" {
		msg += ": " + e.Description
	}
	msg = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, msg)
	if r := []rune(msg); len(r) > 200 {
		msg = string(r[:200]) + "…"
	}
	return " (" + msg + ")"
}

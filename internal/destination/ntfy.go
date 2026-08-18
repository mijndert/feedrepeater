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

	"feedrepeater.com/internal/safehttp"
)

// NtfyLimit is the post length this service will send to ntfy, in runes.
//
// ntfy caps a message at 4096 *bytes* by default, and Limit is counted in
// runes, so the budget is set low enough that even text made entirely of
// four-byte runes fits. Nothing is lost in practice: Mastodon allows 500 and
// Bluesky 300, so a template that fits a timeline fits this several times over.
const NtfyLimit = 1024

// DefaultNtfyServer is the hosted service, and what the form starts with.
const DefaultNtfyServer = "https://ntfy.sh"

// NtfyConfig is the non-secret half of an ntfy destination. A topic name is not
// a credential on the hosted service — anyone who knows it can publish to it —
// but it is not a secret this service is keeping either, so it stays here where
// the form can show it back.
type NtfyConfig struct {
	Server   string `json:"server"`
	Topic    string `json:"topic"`
	Priority int    `json:"priority"`
}

// NtfyCredentials is the encrypted half: an access token, for a topic that is
// reserved or a server that requires authentication. It is empty for an open
// topic, which is the common case on ntfy.sh.
type NtfyCredentials struct {
	Token string `json:"token"`
}

// NtfyPriority is one of ntfy's five notification priorities, named as ntfy
// names them.
type NtfyPriority struct {
	Value int
	Label string
}

// NtfyPriorities lists the priorities for the UI, in ntfy's own order.
var NtfyPriorities = []NtfyPriority{
	{1, "min"},
	{2, "low"},
	{3, "default"},
	{4, "high"},
	{5, "max"},
}

// DefaultNtfyPriority is ntfy's own default. A feed should not decide on its own
// to buzz a phone at max.
const DefaultNtfyPriority = 3

// ValidNtfyPriority reports whether a priority is one ntfy accepts.
func ValidNtfyPriority(p int) bool { return p >= 1 && p <= 5 }

// ntfyTopicPattern is the character set ntfy allows in a topic name. Matching it
// here matters beyond a friendly error: the topic is published as a JSON field
// but is also how the topic is addressed, and a name containing a slash or a
// space is a name that means something other than what was typed.
var ntfyTopicPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// reservedNtfyTopics are the paths the ntfy web app and API occupy. Publishing
// to one is not a topic subscription, so it is refused at the form rather than
// stored and left to fail on every entry.
var reservedNtfyTopics = map[string]bool{
	"account": true, "app": true, "docs": true, "file": true, "login": true,
	"metrics": true, "reset-password": true, "settings": true, "signup": true,
	"static": true, "v1": true,
}

// ValidNtfyTopic checks a topic name.
func ValidNtfyTopic(topic string) error {
	topic = strings.TrimSpace(topic)
	if topic == "" {
		return fmt.Errorf("enter the topic name you subscribed to")
	}
	if !ntfyTopicPattern.MatchString(topic) {
		return fmt.Errorf("a topic name is up to 64 letters, digits, dashes and underscores")
	}
	if reservedNtfyTopics[strings.ToLower(topic)] {
		return fmt.Errorf("%q is reserved by ntfy itself; pick another topic name", topic)
	}
	return nil
}

// ParseNtfyServer checks the server address and reduces it to an origin.
//
// https only, because the access token is a bearer credential and the topic
// name travels in the same request. Any path is dropped rather than kept: the
// publish endpoint is the root of the server, and a stored path would be a
// second place for a request to end up.
func ParseNtfyServer(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return url.Parse(DefaultNtfyServer)
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("that is not a URL")
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("an ntfy server address starts with https://")
	}
	if u.User != nil {
		return nil, fmt.Errorf("that URL must not contain credentials")
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("that address has no host")
	}
	if p := strings.Trim(u.EscapedPath(), "/"); p != "" {
		return nil, fmt.Errorf("enter the server address on its own, without the topic: https://%s", u.Host)
	}
	// Refused rather than dropped. Discarding part of what someone pasted and
	// saying nothing is how a typo becomes a destination that works by accident.
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("enter the server address on its own, with nothing after it")
	}
	// Rebuilt rather than passed through, so no query, fragment or path survives
	// into what gets stored.
	return &url.URL{Scheme: "https", Host: u.Host}, nil
}

type ntfyTarget struct {
	http  *safehttp.Client
	base  string
	cfg   NtfyConfig
	token string
}

// NewNtfy builds an ntfy target from stored configuration. As with every other
// destination the stored address is re-checked rather than trusted.
func NewNtfy(hc *safehttp.Client, configJSON string, credentials []byte) (Target, error) {
	var cfg NtfyConfig
	if err := jsonConfig(configJSON, &cfg); err != nil {
		return nil, Permanent(err)
	}
	if err := ValidNtfyTopic(cfg.Topic); err != nil {
		return nil, Permanentf("ntfy: stored topic is not usable: %v", err)
	}
	u, err := ParseNtfyServer(cfg.Server)
	if err != nil {
		return nil, Permanent(err)
	}
	if err := hc.ValidateURL(u); err != nil {
		return nil, Permanent(err)
	}
	// An open topic needs no token, so missing credentials are not an error
	// here the way they are for Bluesky or a channel webhook.
	var creds NtfyCredentials
	if len(credentials) > 0 {
		if err := json.Unmarshal(credentials, &creds); err != nil {
			return nil, Permanentf("ntfy: stored credentials could not be read")
		}
	}
	if !ValidNtfyPriority(cfg.Priority) {
		cfg.Priority = DefaultNtfyPriority
	}
	return &ntfyTarget{http: hc, base: u.String(), cfg: cfg, token: creds.Token}, nil
}

func (t *ntfyTarget) Limit(context.Context) int { return NtfyLimit }

// ntfyMessage is the JSON publish body.
//
// The JSON form is used rather than ntfy's header form on purpose. Headers are
// ASCII, so a title carrying an em dash or any non-Latin script would have to be
// RFC 2047 encoded to survive; the JSON body has no such limit and a feed title
// is not ours to mangle.
type ntfyMessage struct {
	Topic    string `json:"topic"`
	Message  string `json:"message"`
	Title    string `json:"title,omitempty"`
	Priority int    `json:"priority,omitempty"`
	Click    string `json:"click,omitempty"`
}

func (t *ntfyTarget) Send(ctx context.Context, p Post) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	msg := ntfyMessage{
		Topic:    t.cfg.Topic,
		Message:  p.Text,
		Priority: t.cfg.Priority,
		// The feed is the notification's title and the entry is its body, which
		// is why the title is not repeated here: the default template already
		// starts with the entry title.
		Title: clipRunes(p.Item.FeedTitle, 120),
		Click: clickURL(p.Item.URL),
	}
	if err := postNtfy(ctx, t.http, t.base, t.token, msg); err != nil {
		return nil, err
	}
	// ntfy has no permalink for a delivered notification, so there is nothing
	// for the dashboard to link to.
	return &Result{}, nil
}

// clickURL is the address tapping the notification opens.
//
// This value is handed to someone's phone rather than fetched here, so the
// address policy in safehttp is not the test that matters; the scheme is. A feed
// is untrusted input, and a link is only a link if it is http or https —
// anything else is asking a device to hand the URL to some other application.
func clickURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return u.String()
}

// postNtfy publishes one message and maps the outcome onto retry semantics.
func postNtfy(ctx context.Context, hc *safehttp.Client, base, token string, msg ntfyMessage) error {
	raw, err := json.Marshal(msg)
	if err != nil {
		return Permanent(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/", bytes.NewReader(raw))
	if err != nil {
		return Permanent(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("ntfy: %w", err)
	}
	defer resp.Body.Close()
	body, _ := safehttp.ReadLimited(resp.Body, 64<<10)

	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		// Either the topic is reserved and no token was given, or the token no
		// longer grants publish on it. Neither improves by trying again.
		return Permanentf("ntfy: that topic refused the request; it may need an access token%s", detail(body))
	case http.StatusNotFound:
		return Permanentf("ntfy: that server has no publish endpoint%s", detail(body))
	case http.StatusRequestEntityTooLarge:
		return Permanentf("ntfy: that server rejected the message as too large%s", detail(body))
	case http.StatusTooManyRequests:
		return WithRetryAfter(
			statusError("ntfy", resp.StatusCode, detail(body)),
			headerRetryAfter(resp.Header.Get("Retry-After")),
		)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return statusError("ntfy", resp.StatusCode, detail(body))
	}
	return nil
}

// VerifyNtfy confirms an ntfy destination before it is stored, in two steps.
//
// The first is the control that matters. An ntfy server address is supplied by
// whoever is adding the destination, so without a check this kind is the open
// relay the webhook kind is careful not to be: sign up, name a stranger's
// server, and feedrepeater posts to it on a schedule. ntfy answers /v1/health
// with a fixed document, so requiring that answer means the address has to be
// a cooperating ntfy server rather than merely a URL somebody knows. It is a
// weaker proof than the webhook challenge, which echoes a fresh value, and it
// is the strongest one this protocol offers.
//
// The second is a published notification, for the same reason Slack's
// verification posts a line: there is no way to ask ntfy whether a topic and
// token will work without using them. For a notification service the arriving
// notification is also the confirmation the person was looking for.
func VerifyNtfy(ctx context.Context, hc *safehttp.Client, server, topic, token string) error {
	u, err := ParseNtfyServer(server)
	if err != nil {
		return err
	}
	if err := ValidNtfyTopic(topic); err != nil {
		return err
	}
	return verifyNtfy(ctx, hc, u.String(), topic, token)
}

// verifyNtfy takes the server as given. Callers reach it through VerifyNtfy,
// which validates the address first.
func verifyNtfy(ctx context.Context, hc *safehttp.Client, base, topic, token string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	if err := checkNtfyServer(ctx, hc, base); err != nil {
		return err
	}
	return postNtfy(ctx, hc, base, token, ntfyMessage{
		Topic:    topic,
		Title:    "feedrepeater",
		Message:  "Connected. New entries will arrive here.",
		Priority: DefaultNtfyPriority,
	})
}

// checkNtfyServer requires the address to answer ntfy's own health endpoint.
func checkNtfyServer(ctx context.Context, hc *safehttp.Client, base string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/health", nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("could not reach that server: %w", err)
	}
	defer resp.Body.Close()
	body, err := safehttp.ReadLimited(resp.Body, 64<<10)
	if err != nil {
		return fmt.Errorf("that server returned too much data")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("that server answered %d, so it is not an ntfy server", resp.StatusCode)
	}
	var health struct {
		Healthy bool `json:"healthy"`
	}
	if json.Unmarshal(body, &health) != nil || !health.Healthy {
		return fmt.Errorf("that address did not answer as an ntfy server")
	}
	return nil
}

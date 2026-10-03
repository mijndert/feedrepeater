package destination

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"feedrepeater.com/internal/safehttp"
)

// SlackLimit is the practical length of an incoming-webhook message. Slack
// accepts more in the top-level text field, but truncates it into a file
// attachment past this, which is not what anyone wants from a feed.
const SlackLimit = 3000

// slackHost is the only host an incoming webhook lives on. As with Discord the
// URL is a bearer credential, so it is stored encrypted, never shown again, and
// pinned to the one host that can be posting to a Slack channel.
const slackHost = "hooks.slack.com"

// SlackConfig is the non-secret half. These two path segments identify the
// workspace and the hook without being able to post: the third segment, the
// token, is the part held back in credentials.
type SlackConfig struct {
	Team string `json:"team"`
	Hook string `json:"hook"`
}

// SlackCredentials is the encrypted half: the whole webhook URL.
type SlackCredentials struct {
	URL string `json:"url"`
}

type slackTarget struct {
	http *safehttp.Client
	url  string
}

// NewSlack builds a Slack target from stored configuration. As with Discord the
// stored URL is re-checked rather than trusted.
func NewSlack(hc *safehttp.Client, configJSON string, credentials []byte) (Target, error) {
	var cfg SlackConfig
	if err := jsonConfig(configJSON, &cfg); err != nil {
		return nil, Permanent(err)
	}
	var creds SlackCredentials
	if err := json.Unmarshal(credentials, &creds); err != nil || creds.URL == "" {
		return nil, Permanentf("slack: no webhook URL stored")
	}
	u, _, err := ParseSlackWebhookURL(creds.URL)
	if err != nil {
		return nil, Permanent(err)
	}
	if err := hc.ValidateURL(u); err != nil {
		return nil, Permanent(err)
	}
	return &slackTarget{http: hc, url: u.String()}, nil
}

func (t *slackTarget) Limit(context.Context) int { return SlackLimit }

// ParseSlackWebhookURL checks that a URL is a Slack incoming webhook, and
// returns the parts of it that are safe to store in the clear.
func ParseSlackWebhookURL(raw string) (*url.URL, SlackConfig, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, SlackConfig{}, fmt.Errorf("that is not a URL")
	}
	if u.Scheme != "https" {
		return nil, SlackConfig{}, fmt.Errorf("a Slack webhook URL starts with https://")
	}
	if u.User != nil {
		return nil, SlackConfig{}, fmt.Errorf("that URL must not contain credentials")
	}
	if !strings.EqualFold(u.Hostname(), slackHost) {
		return nil, SlackConfig{}, fmt.Errorf("that is not a %s address", slackHost)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, SlackConfig{}, fmt.Errorf("paste the webhook URL on its own, with nothing after it")
	}
	rest, ok := strings.CutPrefix(u.EscapedPath(), "/services/")
	if !ok {
		return nil, SlackConfig{}, fmt.Errorf("that is not an incoming webhook URL: copy it from the Slack app's Incoming Webhooks page")
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || len(parts[2]) < 8 {
		return nil, SlackConfig{}, fmt.Errorf("that webhook URL is not complete")
	}
	clean := &url.URL{
		Scheme: "https",
		Host:   slackHost,
		Path:   "/services/" + parts[0] + "/" + parts[1] + "/" + parts[2],
	}
	return clean, SlackConfig{Team: parts[0], Hook: parts[1]}, nil
}

// VerifySlackWebhook confirms the URL is live. Slack has no way to ask about a
// webhook without using it, so this posts one line into the channel. That is
// worth the noise: the alternative is storing a URL that turns out to be dead
// and only finding out when an entry is silently lost.
func VerifySlackWebhook(ctx context.Context, hc *safehttp.Client, rawURL string) error {
	u, _, err := ParseSlackWebhookURL(rawURL)
	if err != nil {
		return err
	}
	return verifySlack(ctx, hc, u.String())
}

// verifySlack takes the URL as given. Callers reach it through
// VerifySlackWebhook, which pins the host first.
func verifySlack(ctx context.Context, hc *safehttp.Client, rawURL string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	if err := postSlack(ctx, hc, rawURL, "feedrepeater is connected to this channel. New entries will arrive here."); err != nil {
		return err
	}
	return nil
}

type slackMessage struct {
	Text string `json:"text"`
	// Slack parses its own markup in text by default, so a title containing
	// *stars* or <angle brackets> would be reinterpreted. Feed entries are not
	// ours to reinterpret.
	Mrkdwn bool `json:"mrkdwn"`
}

// slackEscape escapes the three characters Slack treats as markup. Everything
// else, including the link, is left as written.
var slackEscape = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func postSlack(ctx context.Context, hc *safehttp.Client, rawURL, text string) error {
	raw, err := json.Marshal(slackMessage{Text: slackEscape.Replace(text), Mrkdwn: false})
	if err != nil {
		return Permanent(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(raw))
	if err != nil {
		return Permanent(err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("slack: %w", err)
	}
	defer resp.Body.Close()
	body, _ := safehttp.ReadLimited(resp.Body, 64<<10)

	// A revoked hook, a deleted channel, or an app removed from the workspace
	// are all answered with a 4xx and a short reason such as no_service or
	// channel_not_found. None of them get better by trying again.
	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusGone, http.StatusForbidden:
		return Permanentf("slack: that webhook is no longer accepted%s", detail(body))
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return statusError("slack", resp.StatusCode, detail(body))
	}
	return nil
}

func (t *slackTarget) Send(ctx context.Context, p Post) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if err := postSlack(ctx, t.http, t.url, p.Text); err != nil {
		return nil, err
	}
	// An incoming webhook answers "ok" and nothing else, so there is no message
	// to link to.
	return &Result{}, nil
}

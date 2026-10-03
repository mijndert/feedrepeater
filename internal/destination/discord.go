package destination

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"feedrepeater.com/internal/safehttp"
)

// DiscordLimit is the length of a message Discord accepts, in characters.
const DiscordLimit = 2000

// discordHosts are the only hosts a Discord webhook may live on.
//
// The webhook URL is a bearer credential: whoever holds it can post to that
// channel, with no account and no further check. Two things follow. It is
// stored encrypted and never shown again, like a Bluesky app password rather
// than like the address of a generic webhook. And the host is pinned here, so
// this destination cannot be turned into a way to aim signed-in traffic at an
// arbitrary server: without the pin, "Discord" would be an unverified webhook
// with none of the challenge that the webhook kind demands.
var discordHosts = map[string]bool{
	"discord.com":    true,
	"discordapp.com": true,
}

// DiscordConfig is the non-secret half of a Discord destination. None of it can
// post anything; it is what the dashboard shows and what builds a link to a
// delivered message.
type DiscordConfig struct {
	WebhookID string `json:"webhook_id"`
	ChannelID string `json:"channel_id"`
	GuildID   string `json:"guild_id"`
	Name      string `json:"name"`
}

// DiscordCredentials is the encrypted half: the whole webhook URL, token and
// all.
type DiscordCredentials struct {
	URL string `json:"url"`
}

type discordTarget struct {
	http *safehttp.Client
	url  string
	cfg  DiscordConfig
}

// NewDiscord builds a Discord target from stored configuration. The stored URL
// is re-checked rather than trusted: a row edited by hand, or a bug that wrote
// the wrong field, must not become a request to somewhere else.
func NewDiscord(hc *safehttp.Client, configJSON string, credentials []byte) (Target, error) {
	var cfg DiscordConfig
	if err := jsonConfig(configJSON, &cfg); err != nil {
		return nil, Permanent(err)
	}
	var creds DiscordCredentials
	if err := json.Unmarshal(credentials, &creds); err != nil || creds.URL == "" {
		return nil, Permanentf("discord: no webhook URL stored")
	}
	u, err := ParseDiscordWebhookURL(creds.URL)
	if err != nil {
		return nil, Permanent(err)
	}
	if err := hc.ValidateURL(u); err != nil {
		return nil, Permanent(err)
	}
	return &discordTarget{http: hc, url: u.String(), cfg: cfg}, nil
}

func (t *discordTarget) Limit(context.Context) int { return DiscordLimit }

// ParseDiscordWebhookURL checks that a URL is a Discord webhook and nothing
// else: the right host, the webhook path, an id and a token, and no query or
// fragment smuggled alongside them.
func ParseDiscordWebhookURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("that is not a URL")
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("a Discord webhook URL starts with https://")
	}
	if u.User != nil {
		return nil, fmt.Errorf("that URL must not contain credentials")
	}
	if !discordHosts[strings.ToLower(u.Hostname())] {
		return nil, fmt.Errorf("that is not a discord.com address")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("paste the webhook URL on its own, with nothing after it")
	}
	rest, ok := strings.CutPrefix(u.EscapedPath(), "/api/webhooks/")
	if !ok {
		return nil, fmt.Errorf("that is not a webhook URL: copy it from Integrations, Webhooks, Copy Webhook URL")
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) != 2 || !isDigits(parts[0]) || len(parts[1]) < 8 {
		return nil, fmt.Errorf("that webhook URL is not complete")
	}
	// Rebuilt rather than passed through, so nothing outside these two segments
	// survives into a stored credential.
	return &url.URL{
		Scheme: "https",
		Host:   strings.ToLower(u.Hostname()),
		Path:   "/api/webhooks/" + parts[0] + "/" + parts[1],
	}, nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// VerifyDiscordWebhook confirms the URL is a live webhook and reports what it
// points at. Discord answers a GET with the webhook's own record, so this
// proves the token without posting anything into the channel.
func VerifyDiscordWebhook(ctx context.Context, hc *safehttp.Client, rawURL string) (DiscordConfig, error) {
	u, err := ParseDiscordWebhookURL(rawURL)
	if err != nil {
		return DiscordConfig{}, err
	}
	return verifyDiscord(ctx, hc, u.String())
}

// verifyDiscord takes the URL as given. Callers reach it through
// VerifyDiscordWebhook, which pins the host first.
func verifyDiscord(ctx context.Context, hc *safehttp.Client, rawURL string) (DiscordConfig, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return DiscordConfig{}, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return DiscordConfig{}, fmt.Errorf("could not reach Discord: %w", err)
	}
	defer resp.Body.Close()
	body, err := safehttp.ReadLimited(resp.Body, 64<<10)
	if err != nil {
		return DiscordConfig{}, fmt.Errorf("Discord returned too much data")
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusNotFound:
		return DiscordConfig{}, fmt.Errorf("Discord does not recognise that webhook; it may have been deleted")
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return DiscordConfig{}, fmt.Errorf("Discord answered %d", resp.StatusCode)
	}

	var hook struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		ChannelID string `json:"channel_id"`
		GuildID   string `json:"guild_id"`
	}
	if err := json.Unmarshal(body, &hook); err != nil || hook.ID == "" {
		return DiscordConfig{}, fmt.Errorf("Discord returned something unexpected for that webhook")
	}
	return DiscordConfig{
		WebhookID: hook.ID,
		ChannelID: hook.ChannelID,
		GuildID:   hook.GuildID,
		Name:      clipRunes(hook.Name, 80),
	}, nil
}

type discordMessage struct {
	Content         string          `json:"content"`
	AllowedMentions allowedMentions `json:"allowed_mentions"`
}

// allowedMentions with an empty parse list turns every mention in the text into
// plain words. Feed entries are not trusted input: a title containing
// @everyone would otherwise ping a whole server on our behalf.
type allowedMentions struct {
	Parse []string `json:"parse"`
}

func (t *discordTarget) Send(ctx context.Context, p Post) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	raw, err := json.Marshal(discordMessage{
		Content:         p.Text,
		AllowedMentions: allowedMentions{Parse: []string{}},
	})
	if err != nil {
		return nil, Permanent(err)
	}

	// wait=true makes Discord answer with the message it created rather than an
	// empty 204, which is what lets the dashboard link to the post.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url+"?wait=true", bytes.NewReader(raw))
	if err != nil {
		return nil, Permanent(err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("discord: %w", err)
	}
	defer resp.Body.Close()
	body, _ := safehttp.ReadLimited(resp.Body, 64<<10)

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusNotFound {
		return nil, Permanentf("discord: that webhook no longer exists; reconnect it")
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		// Discord says how long to wait, in a header and again in the body, and
		// keeps counting the requests that ignore it. Its own number beats the
		// publisher's guess.
		return nil, WithRetryAfter(
			statusError("discord", resp.StatusCode, detail(body)),
			discordRetryAfter(resp.Header.Get("Retry-After"), body),
		)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, statusError("discord", resp.StatusCode, detail(body))
	}

	var msg struct {
		ID        string `json:"id"`
		ChannelID string `json:"channel_id"`
	}
	_ = json.Unmarshal(body, &msg)
	return &Result{URL: discordMessageURL(t.cfg, msg.ID, msg.ChannelID)}, nil
}

// maxRetryAfter bounds how far a service can push a delivery out. Beyond this
// the entry has gone stale anyway, and the queue should not hold a row for a
// day because a header said so.
const maxRetryAfter = time.Hour

// discordRetryAfter reads the delay Discord asked for. The header is in whole
// seconds and the JSON body carries the fractional value, so the body wins when
// both are present and neither is trusted beyond the ceiling.
func discordRetryAfter(header string, body []byte) time.Duration {
	var out time.Duration
	if secs, err := strconv.ParseFloat(strings.TrimSpace(header), 64); err == nil && secs > 0 {
		out = time.Duration(secs * float64(time.Second))
	}
	var payload struct {
		RetryAfter float64 `json:"retry_after"`
	}
	if json.Unmarshal(body, &payload) == nil && payload.RetryAfter > 0 {
		out = time.Duration(payload.RetryAfter * float64(time.Second))
	}
	if out > maxRetryAfter {
		return maxRetryAfter
	}
	return out
}

// discordMessageURL builds a link to a delivered message. Everything it needs
// is an id Discord gave us; if any is missing the delivery is still a success,
// just without a link.
func discordMessageURL(cfg DiscordConfig, messageID, channelID string) string {
	if channelID == "" {
		channelID = cfg.ChannelID
	}
	if cfg.GuildID == "" || channelID == "" || messageID == "" {
		return ""
	}
	if !isDigits(cfg.GuildID) || !isDigits(channelID) || !isDigits(messageID) {
		return ""
	}
	return "https://discord.com/channels/" + cfg.GuildID + "/" + channelID + "/" + messageID
}

// detail turns a service's error body into a short suffix for a message. It is
// third-party text, so it is clipped here and escaped by the templates.
func detail(body []byte) string {
	s := strings.TrimSpace(string(body))
	if s == "" {
		return ""
	}
	return ": " + clipRunes(s, 120)
}

func clipRunes(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

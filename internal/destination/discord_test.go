package destination

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The URL is the entire credential, so what counts as one has to be narrow:
// the right host, the webhook path, and nothing else riding along.
func TestParseDiscordWebhookURL(t *testing.T) {
	good := []string{
		"https://discord.com/api/webhooks/123456789/AbCdEfGh_token-value",
		"https://discordapp.com/api/webhooks/123456789/AbCdEfGh_token-value",
		"  https://DISCORD.com/api/webhooks/123456789/AbCdEfGh_token-value  ",
	}
	for _, raw := range good {
		u, err := ParseDiscordWebhookURL(raw)
		if err != nil {
			t.Errorf("%q was rejected: %v", raw, err)
			continue
		}
		if u.Path != "/api/webhooks/123456789/AbCdEfGh_token-value" {
			t.Errorf("%q parsed to path %q", raw, u.Path)
		}
	}

	bad := map[string]string{
		"another host":        "https://example.com/api/webhooks/123/token",
		"host as a prefix":    "https://discord.com.evil.example/api/webhooks/123/token",
		"host as a suffix":    "https://evildiscord.com/api/webhooks/123/token",
		"plain http":          "http://discord.com/api/webhooks/123/token",
		"credentials in URL":  "https://user:pass@discord.com/api/webhooks/123/token",
		"not the hook path":   "https://discord.com/api/users/@me",
		"no token":            "https://discord.com/api/webhooks/123",
		"short token":         "https://discord.com/api/webhooks/123/abc",
		"non-numeric id":      "https://discord.com/api/webhooks/abc/AbCdEfGh_token",
		"extra path segments": "https://discord.com/api/webhooks/123/AbCdEfGh_token/extra",
		"query smuggled in":   "https://discord.com/api/webhooks/123/AbCdEfGh_token?wait=false",
		"fragment":            "https://discord.com/api/webhooks/123/AbCdEfGh_token#x",
		"empty":               "",
	}
	for name, raw := range bad {
		if _, err := ParseDiscordWebhookURL(raw); err == nil {
			t.Errorf("%s was accepted: %q", name, raw)
		}
	}
}

// A stored row that is not a Discord webhook must not become a request to
// wherever it points, however it got there.
func TestNewDiscordRejectsAStoredNonDiscordURL(t *testing.T) {
	creds, _ := json.Marshal(DiscordCredentials{URL: "https://example.com/api/webhooks/1/token"})
	if _, err := NewDiscord(devClient(), "{}", creds); err == nil {
		t.Fatal("a stored non-Discord URL was accepted")
	} else if !IsPermanent(err) {
		t.Errorf("rejection is retryable, so it would be tried forever: %v", err)
	}
}

func TestDiscordSendPostsTheTextAndDisablesMentions(t *testing.T) {
	var got struct {
		Content         string `json:"content"`
		AllowedMentions struct {
			Parse []string `json:"parse"`
		} `json:"allowed_mentions"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Query().Get("wait") != "true" {
			t.Error("wait=true is missing, so no message comes back to link to")
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("body was not JSON: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"555","channel_id":"222"}`))
	}))
	defer srv.Close()

	target := &discordTarget{
		http: devClient(),
		url:  srv.URL,
		cfg:  DiscordConfig{GuildID: "111", ChannelID: "222"},
	}
	res, err := target.Send(context.Background(), Post{Text: "@everyone An entry https://example.com/1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "@everyone An entry https://example.com/1" {
		t.Errorf("content = %q", got.Content)
	}
	// The text is passed through verbatim; what stops it pinging a whole server
	// is the empty parse list, not rewriting someone's title.
	if got.AllowedMentions.Parse == nil || len(got.AllowedMentions.Parse) != 0 {
		t.Errorf("allowed_mentions.parse = %v, want an empty list", got.AllowedMentions.Parse)
	}
	if res.URL != "https://discord.com/channels/111/222/555" {
		t.Errorf("message link = %q", res.URL)
	}
}

// A webhook that has been deleted will never work again, so retrying it until
// the attempt limit only delays the failure the dashboard should show.
func TestDiscordDeletedWebhookIsPermanent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"Unknown Webhook","code":10015}`))
	}))
	defer srv.Close()

	target := &discordTarget{http: devClient(), url: srv.URL}
	_, err := target.Send(context.Background(), Post{Text: "hello"})
	if err == nil {
		t.Fatal("a deleted webhook reported success")
	}
	if !IsPermanent(err) {
		t.Errorf("404 from Discord is retryable: %v", err)
	}
}

// Rate limiting is the one failure that is worth trying again, and Discord says
// when. Ignoring that and retrying sooner is how a rate limit becomes a ban.
func TestDiscordRateLimitIsRetryableAndCarriesItsDelay(t *testing.T) {
	cases := map[string]struct {
		header string
		body   string
		want   time.Duration
	}{
		"header only":       {header: "2", body: "{}", want: 2 * time.Second},
		"body is finer":     {header: "1", body: `{"retry_after":1.75}`, want: 1750 * time.Millisecond},
		"body alone":        {body: `{"retry_after":4.5}`, want: 4500 * time.Millisecond},
		"nothing said":      {body: "{}", want: 0},
		"absurd is clamped": {header: "999999", body: "{}", want: maxRetryAfter},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if c.header != "" {
					w.Header().Set("Retry-After", c.header)
				}
				w.WriteHeader(http.StatusTooManyRequests)
				w.Write([]byte(c.body))
			}))
			defer srv.Close()

			target := &discordTarget{http: devClient(), url: srv.URL}
			_, err := target.Send(context.Background(), Post{Text: "hello"})
			if err == nil {
				t.Fatal("a 429 reported success")
			}
			if IsPermanent(err) {
				t.Errorf("429 was treated as permanent: %v", err)
			}
			got, ok := RetryAfter(err)
			if c.want == 0 {
				if ok {
					t.Errorf("a delay of %s was invented", got)
				}
				return
			}
			if !ok || got != c.want {
				t.Errorf("RetryAfter = %s (%v), want %s", got, ok, c.want)
			}
		})
	}
}

// Verification reads the webhook's own record, which proves the token without
// putting anything in the channel.
func TestVerifyDiscordReadsTheWebhookWithoutPosting(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("verification used %s, so it posted to the channel", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{\"id\":\"123\",\"name\":\"feed\\u0007bot\",\"channel_id\":\"222\",\"guild_id\":\"111\"}"))
	}))
	defer srv.Close()

	cfg, err := verifyDiscord(context.Background(), devClient(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WebhookID != "123" || cfg.ChannelID != "222" || cfg.GuildID != "111" {
		t.Errorf("verification returned %+v", cfg)
	}
	// The name comes from Discord and lands in the dashboard, so control
	// characters are flattened on the way in.
	if cfg.Name != "feed bot" {
		t.Errorf("stored name = %q, want the control character replaced", cfg.Name)
	}
}

func TestVerifyDiscordRejectsADeadWebhook(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusNotFound, http.StatusInternalServerError} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		if _, err := verifyDiscord(context.Background(), devClient(), srv.URL); err == nil {
			t.Errorf("%d was accepted as a live webhook", code)
		}
		srv.Close()
	}
	// A 200 that is not a webhook record is not a webhook.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>hello</html>"))
	}))
	defer srv.Close()
	if _, err := verifyDiscord(context.Background(), devClient(), srv.URL); err == nil {
		t.Error("an HTML page was accepted as a webhook")
	}
}

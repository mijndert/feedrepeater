package destination

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseNtfyServer(t *testing.T) {
	// A bare host, a trailing slash and the default all reduce to one origin, so
	// two spellings of the same server do not read as a change.
	for _, raw := range []string{"ntfy.sh", "https://ntfy.sh", "https://ntfy.sh/", ""} {
		u, err := ParseNtfyServer(raw)
		if err != nil {
			t.Fatalf("%q was rejected: %v", raw, err)
		}
		if u.String() != DefaultNtfyServer {
			t.Errorf("%q parsed to %q, want %q", raw, u, DefaultNtfyServer)
		}
	}

	bad := map[string]string{
		"plain http":         "http://ntfy.example",
		"credentials in URL": "https://user:pass@ntfy.example",
		"the topic included": "https://ntfy.sh/my-topic",
		"a query":            "https://ntfy.sh/?x=1",
		"no host":            "https:///path",
		"another scheme":     "ftp://ntfy.example",
	}
	for name, raw := range bad {
		if _, err := ParseNtfyServer(raw); err == nil {
			t.Errorf("%s was accepted: %q", name, raw)
		}
	}
}

func TestValidNtfyTopic(t *testing.T) {
	for _, good := range []string{"a", "my-feed_123", strings.Repeat("t", 64)} {
		if err := ValidNtfyTopic(good); err != nil {
			t.Errorf("ValidNtfyTopic(%q) = %v", good, err)
		}
	}
	bad := map[string]string{
		"empty":          "",
		"a slash":        "my/topic",
		"a space":        "my topic",
		"a dot":          "my.topic",
		"path traversal": "..",
		"too long":       strings.Repeat("t", 65),
		"reserved":       "settings",
		"reserved cased": "Docs",
	}
	for name, topic := range bad {
		if err := ValidNtfyTopic(topic); err == nil {
			t.Errorf("%s was accepted: %q", name, topic)
		}
	}
}

// The message is published as JSON rather than through ntfy's header form,
// because a header cannot carry a title that is not ASCII.
func TestNtfySendPublishesJSON(t *testing.T) {
	var got ntfyMessage
	var auth, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, path = r.Header.Get("Authorization"), r.URL.Path
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("body was not JSON: %s", body)
		}
		w.Write([]byte(`{"id":"abc"}`))
	}))
	defer srv.Close()

	target := &ntfyTarget{
		http:  devClient(),
		base:  srv.URL,
		cfg:   NtfyConfig{Topic: "my-topic", Priority: 4},
		token: "tk_secret",
	}
	res, err := target.Send(context.Background(), Post{
		Text: "An entry — with an em dash\n\nhttps://example.com/1",
		Item: Item{FeedTitle: "Ünicode Feed", URL: "https://example.com/1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/" {
		t.Errorf("published to %q, want the server root", path)
	}
	if auth != "Bearer tk_secret" {
		t.Errorf("Authorization = %q", auth)
	}
	if got.Topic != "my-topic" || got.Priority != 4 {
		t.Errorf("message = %+v", got)
	}
	// The feed names the notification and the entry is its body, so the title is
	// not repeated into the message.
	if got.Title != "Ünicode Feed" {
		t.Errorf("title = %q, want the feed title", got.Title)
	}
	if !strings.Contains(got.Message, "em dash") {
		t.Errorf("message = %q", got.Message)
	}
	// Tapping the notification has to reach the entry.
	if got.Click != "https://example.com/1" {
		t.Errorf("click = %q", got.Click)
	}
	// ntfy has no permalink for a notification, so there is nothing to link to.
	if res.URL != "" {
		t.Errorf("result URL = %q, want empty", res.URL)
	}
}

// The click URL comes from the feed and is handed to someone's phone, so a
// scheme that is not http or https is dropped rather than passed on.
func TestNtfyClickURLRejectsOtherSchemes(t *testing.T) {
	for _, raw := range []string{
		"javascript:alert(1)", "file:///etc/passwd", "intent://scan#Intent;end",
		"data:text/html,<script>", "", "not a url at all", "https://",
	} {
		if got := clickURL(raw); got != "" {
			t.Errorf("clickURL(%q) = %q, want empty", raw, got)
		}
	}
	for _, raw := range []string{"https://example.com/a", "http://example.com/a"} {
		if got := clickURL(raw); got != raw {
			t.Errorf("clickURL(%q) = %q", raw, got)
		}
	}
}

// A topic that refuses the request, a server with no publish endpoint and a
// message the server will not take are all settled: retrying cannot fix them.
func TestNtfyRefusalsArePermanent(t *testing.T) {
	for _, code := range []int{
		http.StatusUnauthorized, http.StatusForbidden,
		http.StatusNotFound, http.StatusRequestEntityTooLarge,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			w.Write([]byte(`{"code":40301,"error":"forbidden"}`))
		}))
		target := &ntfyTarget{http: devClient(), base: srv.URL, cfg: NtfyConfig{Topic: "t"}}
		_, err := target.Send(context.Background(), Post{Text: "hello"})
		if err == nil {
			t.Errorf("%d reported success", code)
		} else if !IsPermanent(err) {
			t.Errorf("%d from ntfy is retryable, so it would be tried six times: %v", code, err)
		}
		srv.Close()
	}
}

func TestNtfyRateLimitIsRetryableAndObeysTheHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "90")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	target := &ntfyTarget{http: devClient(), base: srv.URL, cfg: NtfyConfig{Topic: "t"}}
	_, err := target.Send(context.Background(), Post{Text: "hello"})
	if err == nil {
		t.Fatal("a 429 reported success")
	}
	if IsPermanent(err) {
		t.Errorf("429 was treated as permanent: %v", err)
	}
	if d, ok := RetryAfter(err); !ok || d != 90*time.Second {
		t.Errorf("RetryAfter = %v, %v; want 90s", d, ok)
	}
}

// The health probe is the control that keeps this kind from being an open
// relay, so a server that does not answer as ntfy must be refused before
// anything is published to it.
func TestVerifyNtfyRequiresAnNtfyServer(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"not ntfy at all": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("hello from someone else's server"))
		},
		"unhealthy": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"healthy":false}`))
		},
		"an error": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
	}
	for name, health := range cases {
		posts := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				posts++
				w.Write([]byte(`{"id":"abc"}`))
				return
			}
			health(w, r)
		}))
		if err := verifyNtfy(context.Background(), devClient(), srv.URL, "my-topic", ""); err == nil {
			t.Errorf("%s was accepted as an ntfy server", name)
		}
		if posts != 0 {
			t.Errorf("%s: published %d times before proving itself", name, posts)
		}
		srv.Close()
	}
}

// A healthy server is confirmed by publishing, because ntfy offers no way to ask
// whether a topic and token will work without using them.
func TestVerifyNtfyPublishesOnceOnAHealthyServer(t *testing.T) {
	posts, probes := 0, 0
	var got ntfyMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &got)
			w.Write([]byte(`{"id":"abc"}`))
			return
		}
		if r.URL.Path != "/v1/health" {
			t.Errorf("probed %q, want /v1/health", r.URL.Path)
		}
		probes++
		w.Write([]byte(`{"healthy":true}`))
	}))
	defer srv.Close()

	if err := verifyNtfy(context.Background(), devClient(), srv.URL, "my-topic", ""); err != nil {
		t.Fatalf("a healthy server was rejected: %v", err)
	}
	if probes != 1 || posts != 1 {
		t.Errorf("probed %d times and published %d, want 1 and 1", probes, posts)
	}
	if got.Topic != "my-topic" {
		t.Errorf("confirmation went to topic %q", got.Topic)
	}
}

// A stored row is not trusted any more than a submitted form: the topic and
// server are re-checked when the target is built.
func TestNewNtfyRejectsUnusableStoredConfig(t *testing.T) {
	bad := map[string]NtfyConfig{
		"no topic":      {Server: DefaultNtfyServer},
		"topic path":    {Server: DefaultNtfyServer, Topic: "../v1/health"},
		"plain http":    {Server: "http://ntfy.example", Topic: "t"},
		"server + path": {Server: "https://ntfy.sh/some-other-topic", Topic: "t"},
	}
	for name, cfg := range bad {
		raw, _ := json.Marshal(cfg)
		if _, err := NewNtfy(devClient(), string(raw), nil); err == nil {
			t.Errorf("%s was accepted: %+v", name, cfg)
		} else if !IsPermanent(err) {
			t.Errorf("%s is retryable, so it would be tried forever: %v", name, err)
		}
	}

	// An open topic has no token, so absent credentials are not an error.
	raw, _ := json.Marshal(NtfyConfig{Server: DefaultNtfyServer, Topic: "open-topic"})
	target, err := NewNtfy(devClient(), string(raw), nil)
	if err != nil {
		t.Fatalf("an open topic with no token was rejected: %v", err)
	}
	if got := target.Limit(context.Background()); got != NtfyLimit {
		t.Errorf("limit = %d, want %d", got, NtfyLimit)
	}
}

// An unset or out-of-range priority becomes ntfy's own default rather than 0,
// which ntfy would read as "no priority given" from a service that meant to give
// one.
func TestNewNtfyCorrectsAnImpossiblePriority(t *testing.T) {
	for _, p := range []int{0, -1, 9} {
		raw, _ := json.Marshal(NtfyConfig{Server: DefaultNtfyServer, Topic: "t", Priority: p})
		target, err := NewNtfy(devClient(), string(raw), nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := target.(*ntfyTarget).cfg.Priority; got != DefaultNtfyPriority {
			t.Errorf("priority %d became %d, want %d", p, got, DefaultNtfyPriority)
		}
	}
}

func TestHeaderRetryAfter(t *testing.T) {
	if got := headerRetryAfter("30"); got != 30*time.Second {
		t.Errorf("seconds form = %v", got)
	}
	// A service asking for longer than the ceiling gets the ceiling: a queue
	// should not hold a row for a day because a header said so.
	if got := headerRetryAfter("999999"); got != maxRetryAfter {
		t.Errorf("above the cap = %v, want %v", got, maxRetryAfter)
	}
	if got := headerRetryAfter(time.Now().Add(2 * time.Minute).UTC().Format(http.TimeFormat)); got <= 0 {
		t.Errorf("HTTP-date form = %v, want a positive delay", got)
	}
	for _, v := range []string{"", "  ", "0", "-5", "not a number", "Mon, 02 Jan 2006 15:04:05 GMT"} {
		if got := headerRetryAfter(v); got != 0 {
			t.Errorf("headerRetryAfter(%q) = %v, want 0", v, got)
		}
	}
}

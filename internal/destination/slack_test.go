package destination

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseSlackWebhookURL(t *testing.T) {
	u, cfg, err := ParseSlackWebhookURL("  https://hooks.slack.com/services/T012AB/B034CD/xoxb-token-value  ")
	if err != nil {
		t.Fatalf("a well-formed URL was rejected: %v", err)
	}
	if u.Path != "/services/T012AB/B034CD/xoxb-token-value" {
		t.Errorf("parsed to path %q", u.Path)
	}
	// The two identifying segments are safe to keep in the clear; the token is
	// not among them.
	if cfg.Team != "T012AB" || cfg.Hook != "B034CD" {
		t.Errorf("config = %+v", cfg)
	}

	bad := map[string]string{
		"another host":       "https://example.com/services/T/B/token",
		"host as a prefix":   "https://hooks.slack.com.evil.example/services/T/B/tokenvalue",
		"slack but not hook": "https://slack.com/services/T/B/tokenvalue",
		"plain http":         "http://hooks.slack.com/services/T/B/tokenvalue",
		"credentials in URL": "https://u:p@hooks.slack.com/services/T/B/tokenvalue",
		"not the hook path":  "https://hooks.slack.com/api/chat.postMessage",
		"missing a segment":  "https://hooks.slack.com/services/T012AB/B034CD",
		"short token":        "https://hooks.slack.com/services/T012AB/B034CD/abc",
		"query smuggled in":  "https://hooks.slack.com/services/T/B/tokenvalue?x=1",
		"empty":              "",
	}
	for name, raw := range bad {
		if _, _, err := ParseSlackWebhookURL(raw); err == nil {
			t.Errorf("%s was accepted: %q", name, raw)
		}
	}
}

func TestNewSlackRejectsAStoredNonSlackURL(t *testing.T) {
	creds, _ := json.Marshal(SlackCredentials{URL: "https://example.com/services/T/B/tokenvalue"})
	if _, err := NewSlack(devClient(), "{}", creds); err == nil {
		t.Fatal("a stored non-Slack URL was accepted")
	} else if !IsPermanent(err) {
		t.Errorf("rejection is retryable, so it would be tried forever: %v", err)
	}
}

// Slack reads its own markup in message text, so the three characters that
// carry meaning are escaped and the rest is left alone.
func TestSlackSendEscapesSlackMarkupOnly(t *testing.T) {
	var got struct {
		Text   string `json:"text"`
		Mrkdwn bool   `json:"mrkdwn"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("body was not JSON: %s", body)
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	target := &slackTarget{http: devClient(), url: srv.URL}
	if _, err := target.Send(context.Background(), Post{
		Text: "Tom & Jerry <hello> *stars* https://example.com/1",
	}); err != nil {
		t.Fatal(err)
	}
	want := "Tom &amp; Jerry &lt;hello&gt; *stars* https://example.com/1"
	if got.Text != want {
		t.Errorf("text = %q, want %q", got.Text, want)
	}
	if got.Mrkdwn {
		t.Error("mrkdwn is on, so a title with markup would be reinterpreted")
	}
}

// A revoked hook or a deleted channel will not start working again.
func TestSlackDeadWebhookIsPermanent(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusGone, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			w.Write([]byte("no_service"))
		}))
		target := &slackTarget{http: devClient(), url: srv.URL}
		_, err := target.Send(context.Background(), Post{Text: "hello"})
		if err == nil {
			t.Errorf("%d reported success", code)
		} else if !IsPermanent(err) {
			t.Errorf("%d from Slack is retryable: %v", code, err)
		}
		srv.Close()
	}
}

func TestSlackServerErrorIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	target := &slackTarget{http: devClient(), url: srv.URL}
	_, err := target.Send(context.Background(), Post{Text: "hello"})
	if err == nil {
		t.Fatal("a 502 reported success")
	}
	if IsPermanent(err) {
		t.Errorf("502 was treated as permanent: %v", err)
	}
}

// Verification is a real post, because Slack offers nothing else. It has to
// fail when the hook does not work.
func TestVerifySlackPostsAndReportsFailure(t *testing.T) {
	posted := 0
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posted++
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		w.Write([]byte("ok"))
	}))
	defer ok.Close()
	if err := verifySlack(context.Background(), devClient(), ok.URL); err != nil {
		t.Errorf("a working webhook was rejected: %v", err)
	}
	if posted != 1 {
		t.Errorf("verification posted %d times, want 1", posted)
	}

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("no_service"))
	}))
	defer dead.Close()
	if err := verifySlack(context.Background(), devClient(), dead.URL); err == nil {
		t.Error("a dead webhook was accepted")
	}
}

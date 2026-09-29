package mastodon

import (
	"strings"
	"testing"
)

func TestNormalizeHost(t *testing.T) {
	good := map[string]string{
		"mastodon.social":                "mastodon.social",
		"  Mastodon.Social  ":            "mastodon.social",
		"https://mastodon.social":        "mastodon.social",
		"https://mastodon.social/about":  "mastodon.social",
		"@alice@mastodon.social":         "mastodon.social",
		"alice@hachyderm.io":             "hachyderm.io",
		"mastodon.social.":               "mastodon.social",
		"https://citroën.example.social": "xn--citron-tva.example.social",
		// The host of a URL is its host, whatever the path looks like.
		"https://evil.example/@good.social": "evil.example",
		"https://good.social@evil.example":  "evil.example",
	}
	for in, want := range good {
		got, err := NormalizeHost(in)
		if err != nil {
			t.Errorf("NormalizeHost(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("NormalizeHost(%q) = %q, want %q", in, got, want)
		}
	}

	bad := []string{
		"", "   ", "localhost", "mastodon", "127.0.0.1", "[::1]", "10.0.0.1",
		"mastodon.social:8080", "https://mastodon.social:443",
		"javascript:alert(1)", "example.internal", "printer.local",
		strings.Repeat("a", 300) + ".social",
	}
	for _, in := range bad {
		if got, err := NormalizeHost(in); err == nil {
			t.Errorf("NormalizeHost(%q) = %q, want error", in, got)
		}
	}
}

// The host must survive normalisation as a bare hostname, since it is used to
// build request URLs.
func TestNormalizeHostRejectsEmbeddedPath(t *testing.T) {
	got, err := NormalizeHost("evil.example/path")
	if err != nil {
		return // rejected outright, also fine
	}
	if strings.ContainsAny(got, "/?#@") {
		t.Errorf("NormalizeHost leaked path characters: %q", got)
	}
}

func TestAuthorizeURL(t *testing.T) {
	u := AuthorizeURL("mastodon.social", "cid", "https://fr.example/auth/callback", "st4te", "ch4llenge")
	for _, want := range []string{
		"https://mastodon.social/oauth/authorize?",
		"client_id=cid",
		"code_challenge=ch4llenge",
		"code_challenge_method=S256",
		"state=st4te",
		"redirect_uri=https%3A%2F%2Ffr.example%2Fauth%2Fcallback",
		"scope=read%3Aaccounts+write%3Astatuses",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("AuthorizeURL missing %q\ngot %s", want, u)
		}
	}
}

func TestAPIErrorIsBounded(t *testing.T) {
	body := []byte(`{"error":"` + strings.Repeat("x", 5000) + `"}`)
	got := apiError(body)
	if len([]rune(got)) > 250 {
		t.Errorf("apiError returned %d runes", len([]rune(got)))
	}
	if strings.ContainsAny(apiError([]byte("{\"error\":\"a\\nb\\u0000c\"}")), "\n\x00") {
		t.Error("apiError kept control characters")
	}
}

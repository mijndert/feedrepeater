package destination

import (
	"errors"
	"strings"
	"testing"

	"feedrepeater.com/internal/mastodon"
)

// Facet offsets are byte indexes. Any drift between rune and byte counting
// corrupts the link ranges in a post containing non-ASCII text.
func TestLinkFacetsUsesByteOffsets(t *testing.T) {
	text := "héllo wörld https://example.com/a done"
	facets := LinkFacets(text)
	if len(facets) != 1 {
		t.Fatalf("got %d facets, want 1", len(facets))
	}
	f := facets[0]
	if got := text[f.Index.ByteStart:f.Index.ByteEnd]; got != "https://example.com/a" {
		t.Errorf("facet covers %q, want the URL", got)
	}
	if f.Features[0].URI != "https://example.com/a" {
		t.Errorf("feature URI = %q", f.Features[0].URI)
	}
	if f.Features[0].Type != "app.bsky.richtext.facet#link" {
		t.Errorf("feature type = %q", f.Features[0].Type)
	}
}

func TestLinkFacetsTrailingPunctuation(t *testing.T) {
	cases := map[string]string{
		"See https://example.com/a.":            "https://example.com/a",
		"See https://example.com/a, and more":   "https://example.com/a",
		"(https://example.com/a)":               "https://example.com/a",
		"https://en.wikipedia.org/wiki/Go_(pl)": "https://en.wikipedia.org/wiki/Go_(pl)",
	}
	for text, want := range cases {
		facets := LinkFacets(text)
		if len(facets) != 1 {
			t.Fatalf("%q: got %d facets", text, len(facets))
		}
		if got := text[facets[0].Index.ByteStart:facets[0].Index.ByteEnd]; got != want {
			t.Errorf("%q: covers %q, want %q", text, got, want)
		}
	}
}

func TestLinkFacetsMultiple(t *testing.T) {
	text := "a https://one.example b https://two.example"
	if got := len(LinkFacets(text)); got != 2 {
		t.Errorf("got %d facets, want 2", got)
	}
	if got := len(LinkFacets("no links here")); got != 0 {
		t.Errorf("got %d facets, want 0", got)
	}
}

func TestValidDID(t *testing.T) {
	good := []string{"did:plc:abcdefghijklmnopqrstuvwx", "did:web:example.com"}
	for _, d := range good {
		if !ValidDID(d) {
			t.Errorf("ValidDID(%q) = false", d)
		}
	}
	bad := []string{
		"", "did:plc:short", "did:key:abc", "did:web:exa mple.com",
		"did:web:example.com/../evil", "did:plc:ABCDEFGHIJKLMNOPQRSTUVWX",
		"did:web:" + strings.Repeat("a", 300),
	}
	for _, d := range bad {
		if ValidDID(d) {
			t.Errorf("ValidDID(%q) = true", d)
		}
	}
}

func TestBlueskyPostURL(t *testing.T) {
	got := blueskyPostURL("alice.bsky.social", "at://did:plc:abc/app.bsky.feed.post/3kabc")
	want := "https://bsky.app/profile/alice.bsky.social/post/3kabc"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := blueskyPostURL("alice.bsky.social", "nonsense"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestWebhookSignatureCoversTimestampAndBody(t *testing.T) {
	body := []byte(`{"event":"entry.published"}`)
	a := Sign("secret", "1700000000", body)
	if a == Sign("secret", "1700000001", body) {
		t.Error("signature ignores the timestamp")
	}
	if a == Sign("secret", "1700000000", []byte(`{"event":"other"}`)) {
		t.Error("signature ignores the body")
	}
	if a == Sign("other-secret", "1700000000", body) {
		t.Error("signature ignores the secret")
	}
	// A timestamp/body split must not be forgeable by moving the boundary.
	if Sign("s", "12", []byte("34")) == Sign("s", "1", []byte("234")) {
		t.Error("signature is ambiguous across the timestamp boundary")
	}
}

func TestPermanentErrors(t *testing.T) {
	if !IsPermanent(Permanentf("nope")) {
		t.Error("Permanentf is not permanent")
	}
	if IsPermanent(statusError("svc", 503, "")) {
		t.Error("503 treated as permanent")
	}
	if !IsPermanent(statusError("svc", 401, "")) {
		t.Error("401 treated as retryable")
	}
	if IsPermanent(statusError("svc", 429, "")) {
		t.Error("429 treated as permanent")
	}
}

// Whether a Mastodon failure is permanent decides whether the worker spends
// five more attempts on it. A revoked token and a rejected status never become
// valid on their own; a 5xx or a rate limit might.
func TestMastodonErrorsAreClassified(t *testing.T) {
	cases := map[int]bool{401: true, 403: true, 422: true, 429: false, 500: false, 503: false}
	for code, permanent := range cases {
		err := classifyMastodon(&mastodon.HTTPError{Host: "example.social", StatusCode: code})
		if got := IsPermanent(err); got != permanent {
			t.Errorf("%d: IsPermanent = %v, want %v", code, got, permanent)
		}
	}
	// A transport failure never reached the instance, so it says nothing about
	// whether trying again would work.
	if IsPermanent(classifyMastodon(errors.New("connection reset"))) {
		t.Error("a transport failure was treated as permanent")
	}
}

// The delay a service asks for travels with the error, so the publisher can
// wait that long rather than guessing.
func TestRetryAfterTravelsWithTheError(t *testing.T) {
	if _, ok := RetryAfter(errors.New("plain")); ok {
		t.Error("a plain error carried a delay")
	}
	if _, ok := RetryAfter(WithRetryAfter(errors.New("busy"), 0)); ok {
		t.Error("a zero delay was carried")
	}
	d, ok := RetryAfter(WithRetryAfter(errors.New("busy"), 90))
	if !ok || d != 90 {
		t.Errorf("RetryAfter = %v, %v; want 90, true", d, ok)
	}
}

package destination

import (
	"errors"
	"testing"

	"feedrepeater.com/internal/mastodon"
)

// Whether a failure is permanent decides whether the worker spends five more
// attempts on it. A revoked token and a rejected status never become valid on
// their own; a 5xx or a rate limit might.
func TestPermanentErrors(t *testing.T) {
	if !IsPermanent(Permanentf("nope")) {
		t.Error("Permanentf is not permanent")
	}
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

// Package destination defines the interface every publishing target
// implements, and the registry of available kinds.
package destination

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"feedrepeater.com/internal/render"
	"feedrepeater.com/internal/safehttp"
)

// Kinds, as stored in destinations.kind.
const (
	KindMastodon = "mastodon"
	KindBluesky  = "bluesky"
	KindDiscord  = "discord"
	KindSlack    = "slack"
	KindNtfy     = "ntfy"
	KindLinkding = "linkding"
	KindWebhook  = "webhook"
)

// Post is one rendered entry ready to be published.
type Post struct {
	Text string
	Item Item
	// IdempotencyKey is stable across retries of the same delivery.
	IdempotencyKey string
}

// Item is the source entry, passed through to webhooks unrendered.
type Item struct {
	Title     string
	URL       string
	Summary   string
	Author    string
	Published time.Time
	FeedTitle string
	FeedURL   string
}

// Result is what a successful send reports back.
type Result struct {
	// URL of the created post, when the service returns one.
	URL string
}

// Target publishes posts to one service.
type Target interface {
	// Send publishes a post. A returned error that satisfies Permanent should
	// not be retried.
	Send(ctx context.Context, p Post) (*Result, error)
	// Limit is the maximum post length in runes, or 0 for unlimited.
	Limit(ctx context.Context) int
}

// Kind describes a destination type for the UI and knows how to build a Target
// from stored configuration.
type Kind struct {
	Name string
	// Label is the human name shown in the UI.
	Label string
	// Description is one line, no marketing.
	Description string
	// DefaultTemplate is applied to new destinations of this kind.
	DefaultTemplate string
}

// Kinds lists the supported destinations in display order.
var Kinds = []Kind{
	{
		Name:            KindMastodon,
		Label:           "Mastodon",
		Description:     "Post as a status on any Mastodon-compatible server.",
		DefaultTemplate: render.DefaultTemplate,
	},
	{
		Name:            KindBluesky,
		Label:           "Bluesky",
		Description:     "Post to a Bluesky account using an app password.",
		DefaultTemplate: render.DefaultTemplate,
	},
	{
		Name:            KindDiscord,
		Label:           "Discord",
		Description:     "Post to a channel with a Discord webhook URL.",
		DefaultTemplate: render.DefaultTemplate,
	},
	{
		Name:            KindSlack,
		Label:           "Slack",
		Description:     "Post to a channel with a Slack incoming webhook.",
		DefaultTemplate: render.DefaultTemplate,
	},
	{
		Name:            KindNtfy,
		Label:           "ntfy",
		Description:     "Push a notification to a phone through an ntfy topic.",
		DefaultTemplate: render.DefaultTemplate,
	},
	{
		Name:            KindLinkding,
		Label:           "linkding",
		Description:     "Save each entry as a bookmark in your own linkding.",
		DefaultTemplate: render.DefaultTemplate,
	},
	{
		Name:            KindWebhook,
		Label:           "Webhook",
		Description:     "Send a signed JSON request to a URL you control.",
		DefaultTemplate: render.DefaultTemplate,
	},
}

// KindByName returns the kind description, or false if unknown.
func KindByName(name string) (Kind, bool) {
	for _, k := range Kinds {
		if k.Name == name {
			return k, true
		}
	}
	return Kind{}, false
}

// permanentError marks a failure that retrying cannot fix: bad credentials,
// a rejected post, a destination that no longer exists.
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Permanent wraps an error so the worker stops retrying it.
func Permanent(err error) error { return permanentError{err} }

// Permanentf builds a permanent error.
func Permanentf(format string, args ...any) error {
	return permanentError{fmt.Errorf(format, args...)}
}

// retryAfterError carries a delay a service asked for, so the publisher can
// wait that long instead of guessing.
type retryAfterError struct {
	err   error
	after time.Duration
}

func (e retryAfterError) Error() string { return e.err.Error() }
func (e retryAfterError) Unwrap() error { return e.err }

// WithRetryAfter attaches a service's own requested delay to an error. It is
// ignored on a permanent failure, which is not going to be retried at all.
func WithRetryAfter(err error, after time.Duration) error {
	if err == nil || after <= 0 {
		return err
	}
	return retryAfterError{err: err, after: after}
}

// RetryAfter reports the delay a service asked for, if it asked for one.
func RetryAfter(err error) (time.Duration, bool) {
	var r retryAfterError
	if errors.As(err, &r) && r.after > 0 {
		return r.after, true
	}
	return 0, false
}

// headerRetryAfter reads a Retry-After header in both of its forms, a delay in
// seconds or an HTTP date, and bounds it by maxRetryAfter. Services with a
// richer answer than the header — Discord puts a fractional value in the body —
// parse their own.
func headerRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	var out time.Duration
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		out = time.Duration(secs) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		if out = time.Until(t); out <= 0 {
			return 0
		}
	}
	if out > maxRetryAfter {
		return maxRetryAfter
	}
	return out
}

// IsPermanent reports whether an error should end retries.
func IsPermanent(err error) bool {
	var p permanentError
	if errors.As(err, &p) {
		return true
	}
	// A blocked address will stay blocked.
	return errors.Is(err, safehttp.ErrBlocked)
}

// retryableStatus reports whether an HTTP status is worth trying again.
func retryableStatus(code int) bool {
	switch {
	case code == 408, code == 425, code == 429:
		return true
	case code >= 500:
		return true
	}
	return false
}

// statusError converts an HTTP response code into an error with the right
// retry semantics.
func statusError(service string, code int, detail string) error {
	err := fmt.Errorf("%s returned %d%s", service, code, detail)
	if retryableStatus(code) {
		return err
	}
	return Permanent(err)
}

// jsonConfig decodes a destination's non-secret config blob.
func jsonConfig(raw string, out any) error {
	if raw == "" {
		raw = "{}"
	}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		return fmt.Errorf("destination: unreadable config: %w", err)
	}
	return nil
}

// publicURL validates a stored URL before use. Configuration is validated on
// save, but it is re-checked here because DNS can change under a stored name.
func publicURL(hc *safehttp.Client, raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, Permanent(err)
	}
	if err := hc.ValidateURL(u); err != nil {
		return nil, Permanent(err)
	}
	return u, nil
}

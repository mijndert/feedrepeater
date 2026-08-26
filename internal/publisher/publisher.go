// Package publisher turns a queued delivery into a post on a service.
package publisher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"feedrepeater.com/internal/destination"
	"feedrepeater.com/internal/mastodon"
	"feedrepeater.com/internal/render"
	"feedrepeater.com/internal/safehttp"
	"feedrepeater.com/internal/secret"
	"feedrepeater.com/internal/store"
)

// MaxAttempts is how many times a delivery is tried before it is abandoned.
const MaxAttempts = 6

type Publisher struct {
	store    *store.Store
	keys     *secret.Keyring
	http     *safehttp.Client
	mastodon *mastodon.Client
	log      *slog.Logger
}

func New(st *store.Store, keys *secret.Keyring, hc *safehttp.Client, md *mastodon.Client, log *slog.Logger) *Publisher {
	return &Publisher{store: st, keys: keys, http: hc, mastodon: md, log: log}
}

// Target decrypts a destination's credentials and builds its client.
func (p *Publisher) Target(d *store.Destination) (destination.Target, error) {
	creds, err := p.keys.Decrypt(secret.PurposeDestination, d.Credentials)
	if err != nil {
		return nil, destination.Permanentf("stored credentials could not be read")
	}
	switch d.Kind {
	case destination.KindMastodon:
		return destination.NewMastodon(p.mastodon, d.Config, creds)
	case destination.KindBluesky:
		return destination.NewBluesky(p.http, d.Config, creds)
	case destination.KindDiscord:
		return destination.NewDiscord(p.http, d.Config, creds)
	case destination.KindSlack:
		return destination.NewSlack(p.http, d.Config, creds)
	case destination.KindNtfy:
		return destination.NewNtfy(p.http, d.Config, creds)
	case destination.KindLinkding:
		return destination.NewLinkding(p.http, d.Config, creds)
	case destination.KindWebhook:
		return destination.NewWebhook(p.http, d.Config, creds)
	default:
		return nil, destination.Permanentf("unknown destination type %q", d.Kind)
	}
}

// Compose renders the text a destination would post for an entry. loc is the
// timezone {{published}} is rendered in; nil means UTC.
func (p *Publisher) Compose(ctx context.Context, t destination.Target, d *store.Destination, item destination.Item, loc *time.Location) string {
	tmpl := d.Template
	if strings.TrimSpace(tmpl) == "" {
		tmpl = render.DefaultTemplate
	}
	return render.Render(tmpl, render.Vars{
		Title:     item.Title,
		URL:       item.URL,
		Summary:   item.Summary,
		Author:    item.Author,
		FeedTitle: item.FeedTitle,
		Published: item.Published,
		Location:  loc,
	}, t.Limit(ctx))
}

// Send publishes one entry to one destination immediately. It is used both by
// the delivery worker and by the "send a test post" button.
func (p *Publisher) Send(ctx context.Context, d *store.Destination, item destination.Item, loc *time.Location, idempotencyKey string) (*destination.Result, error) {
	target, err := p.Target(d)
	if err != nil {
		return nil, err
	}
	text := p.Compose(ctx, target, d, item, loc)
	if strings.TrimSpace(text) == "" {
		return nil, destination.Permanentf("the template produced an empty post")
	}
	return target.Send(ctx, destination.Post{
		Text:           text,
		Item:           item,
		IdempotencyKey: idempotencyKey,
	})
}

// Deliver processes one queued delivery and records the outcome.
//
// Everything it needs arrived with the row. Resolving a delivery used to mean
// four more queries — the entry, the destination, the feed for its title, the
// account for its timezone — each one a round trip on a pool that was a single
// connection, for every delivery in a burst.
func (p *Publisher) Deliver(ctx context.Context, dl *store.DueDelivery) {
	if dl.Destination.UserID != dl.UserID {
		// Cannot happen without corruption — the query joins on it — but the
		// worker sends under a user's credentials, so verify rather than assume.
		p.log.Error("delivery/destination owner mismatch", "delivery", dl.ID)
		_ = p.store.MarkDeliveryFailed(ctx, dl.ID, "destination does not belong to this account")
		return
	}
	if dl.Destination.Paused {
		_ = p.store.MarkDeliveryFailed(ctx, dl.ID, "destination was paused")
		return
	}

	published := time.Time{}
	if dl.Item.PublishedAt != nil {
		published = *dl.Item.PublishedAt
	}

	dest := dl.Destination
	res, err := p.Send(ctx, &dest, destination.Item{
		Title:     dl.Item.Title,
		URL:       dl.Item.URL,
		Summary:   dl.Item.Summary,
		Author:    dl.Item.Author,
		Published: published,
		FeedTitle: dl.FeedTitle,
		FeedURL:   dl.FeedURL,
	}, store.ParseLocation(dl.Timezone), idempotencyKey(dl))

	if err != nil {
		reason := userMessage(err)
		_ = p.store.RecordDestinationResult(ctx, dest.ID, reason)
		if destination.IsPermanent(err) || dl.Attempts+1 >= MaxAttempts {
			p.log.Warn("delivery failed", "delivery", dl.ID, "kind", dest.Kind, "error", err)
			_ = p.store.MarkDeliveryFailed(ctx, dl.ID, reason)
			return
		}
		p.retry(ctx, dl, reason, err)
		return
	}

	remote := ""
	if res != nil {
		remote = res.URL
	}
	_ = p.store.RecordDestinationResult(ctx, dest.ID, "")
	if err := p.store.MarkDeliverySent(ctx, dl.ID, remote); err != nil {
		p.log.Error("mark delivered", "delivery", dl.ID, "error", err)
	}
}

// retry schedules the next attempt. A service that said how long to wait gets
// what it asked for when that is longer than our own backoff: being told to
// slow down and speeding up instead is how a rate limit becomes a ban.
func (p *Publisher) retry(ctx context.Context, dl *store.DueDelivery, reason string, cause error) {
	delay := backoff(dl.Attempts + 1)
	if asked, ok := destination.RetryAfter(cause); ok && asked > delay {
		delay = asked
	}
	if err := p.store.MarkDeliveryRetry(ctx, dl.ID, time.Now().UTC().Add(delay), reason); err != nil {
		p.log.Error("schedule retry", "delivery", dl.ID, "error", err)
	}
}

// backoff grows from a minute to an hour.
func backoff(attempt int) time.Duration {
	d := time.Duration(math.Pow(3, float64(attempt))) * time.Minute
	if d > time.Hour || d <= 0 {
		return time.Hour
	}
	return d
}

// idempotencyKey is stable for a delivery so a retry that actually reached the
// service does not produce a second post.
func idempotencyKey(dl *store.DueDelivery) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "feedrepeater/%d/%d/%d", dl.ID, dl.ItemID, dl.DestinationID))
	return hex.EncodeToString(sum[:16])
}

// userMessage shortens an error for display. Errors here can contain text from
// a third-party service, so it is length-limited; templates handle escaping.
func userMessage(err error) string {
	msg := err.Error()
	msg = strings.TrimPrefix(msg, "destination: ")
	if r := []rune(msg); len(r) > 300 {
		msg = string(r[:300]) + "…"
	}
	return msg
}

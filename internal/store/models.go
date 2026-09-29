package store

import (
	"sync"
	"time"
)

type User struct {
	ID          int64
	Host        string
	RemoteID    string
	Acct        string
	DisplayName string
	AvatarURL   string
	AccessToken []byte // encrypted
	// Timezone is an IANA name, or empty for UTC.
	Timezone string
	// DefaultTemplate is the post text a new destination starts from, or empty
	// for the built-in one. It is copied onto a destination at creation, so
	// changing it never rewrites what an existing destination posts.
	DefaultTemplate string
	CreatedAt       time.Time
	LastLoginAt     time.Time
}

// locations memoises resolved timezones.
//
// time.LoadLocation caches nothing: every call reads and parses the zone out of
// the embedded tzdata afresh. That is once per delivery, and once per row of the
// activity list — twenty-five reparses of the same zone to render one dashboard.
// The set of zones an account can name is fixed and small, so holding them
// costs a few kilobytes and the map never needs to be swept.
var locations sync.Map

// ParseLocation resolves a stored timezone name.
//
// Anything unusable is UTC. A name is validated when it is saved, so a failure
// here means the zone has since left the tzdata, and a page of dates or a
// pending delivery must not break over that.
func ParseLocation(name string) *time.Location {
	if name == "" {
		return time.UTC
	}
	if loc, ok := locations.Load(name); ok {
		return loc.(*time.Location)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		// Cached as well: a zone that has left the tzdata will not come back
		// before the process restarts, and it is read on every delivery.
		loc = time.UTC
	}
	locations.Store(name, loc)
	return loc
}

// Location is the timezone this account reads dates in.
func (u *User) Location() *time.Location {
	if u == nil {
		return time.UTC
	}
	return ParseLocation(u.Timezone)
}

// Handle renders the fully-qualified account name, e.g. @alice@mastodon.social.
func (u *User) Handle() string {
	if u == nil {
		return ""
	}
	return "@" + u.Acct + "@" + u.Host
}

// Name is the display name if the instance supplied one, otherwise the handle.
func (u *User) Name() string {
	if u == nil {
		return ""
	}
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Handle()
}

type Instance struct {
	Host         string
	ClientID     string
	ClientSecret []byte // encrypted
	// RedirectURI is the callback the client was registered with. A client is
	// only reusable while this still matches the service's own callback.
	RedirectURI string
	CreatedAt   time.Time
}

type Session struct {
	ID        string // SHA-256 of the cookie value
	UserID    int64
	CreatedAt time.Time
	ExpiresAt time.Time
}

// Feed is a document, not an account's copy of one. Anything that varies
// between subscribers lives on Subscription; anything that varies between polls
// lives on FetchState.
type Feed struct {
	ID        int64
	URL       string
	Title     string
	CreatedAt time.Time
}

// FetchState is everything polling knows about a feed. It is a separate row
// from the feed because it is rewritten on every poll and the feed is not: a
// narrow row packs many feeds into the page a fetch dirties, which is most of
// what the replica ends up shipping.
type FetchState struct {
	FeedID       int64
	ETag         string
	LastModified string
	// BodyHash is the SHA-256 of the last body that was parsed. A server with
	// no conditional-request support answers 200 with identical bytes forever,
	// and comparing this is what makes that cost nothing beyond the transfer.
	BodyHash    []byte
	NextFetchAt time.Time
	LastFetchAt *time.Time
	// ChangedAt is when the feed last actually produced an entry, which is what
	// the poll interval widens from. A feed that keeps returning the same
	// entries is cheap to carry and should be asked rarely.
	ChangedAt *time.Time
	LastError string
	Failures  int
	// FailingSince is when the current run of failures began, or nil if the
	// last fetch succeeded. The dead-feed rule reads elapsed time off this
	// rather than inferring it from the failure count, which stopped meaning a
	// fixed span of time once the poll interval started varying.
	FailingSince *time.Time
	// Disabled is the dead-feed rule. It belongs to the document and stops it
	// being polled for everyone; a subscriber's own pause is Subscription.Paused.
	Disabled bool
}

// Subscription is one account's interest in a feed.
type Subscription struct {
	ID     int64
	UserID int64
	FeedID int64
	Paused bool
	// PrimeItemID is the watermark between history and news for this account.
	// Entries at or below it are never delivered, which is what lets a new
	// subscriber join a feed someone else already follows without replaying it.
	PrimeItemID int64
	Primed      bool
	CreatedAt   time.Time
}

// FeedView is an account's feed as the dashboard shows it: the document, the
// polling state, and this subscriber's own settings flattened into one row,
// because that is how they are read and there is no page that wants a third of
// it.
type FeedView struct {
	SubscriptionID int64
	ID             int64
	UserID         int64
	URL            string
	Title          string
	// Paused is this account's own pause. Disabled is the dead-feed rule, which
	// applies to every subscriber at once.
	Paused      bool
	Disabled    bool
	Primed      bool
	NextFetchAt time.Time
	LastFetchAt *time.Time
	ChangedAt   *time.Time
	LastError   string
	Failures    int
	CreatedAt   time.Time
	// Subscribers is how many accounts follow this feed. One fetch serves all
	// of them, which is the whole point of the feed being shared.
	Subscribers int
}

type Item struct {
	ID          int64
	FeedID      int64
	GUID        string
	URL         string
	Title       string
	Summary     string
	Author      string
	PublishedAt *time.Time
	SeenAt      time.Time
}

type Destination struct {
	ID          int64
	UserID      int64
	Kind        string
	Label       string
	Config      string // JSON, no secrets
	Credentials []byte // encrypted JSON
	Template    string
	Paused      bool
	CreatedAt   time.Time
	LastOKAt    *time.Time
	LastError   string
}

const (
	DeliveryPending = "pending"
	DeliverySent    = "sent"
	DeliveryFailed  = "failed"
)

type Delivery struct {
	ID            int64
	UserID        int64
	ItemID        int64
	DestinationID int64
	Status        string
	Attempts      int
	NextAttemptAt time.Time
	LastError     string
	RemoteURL     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// DeliveryView joins a delivery with the item and destination it refers to,
// for rendering the activity list.
type DeliveryView struct {
	Delivery
	ItemTitle       string
	ItemURL         string
	DestinationKind string
	DestinationName string
}

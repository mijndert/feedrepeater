package store

import "time"

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

// ParseLocation resolves a stored timezone name.
//
// Anything unusable is UTC. A name is validated when it is saved, so a failure
// here means the zone has since left the tzdata, and a page of dates or a
// pending delivery must not break over that.
func ParseLocation(name string) *time.Location {
	if name == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
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

type Feed struct {
	ID           int64
	UserID       int64
	URL          string
	Title        string
	ETag         string
	LastModified string
	// Primed reports whether the first fetch has completed. Entries present at
	// that point are recorded as seen but never delivered, so adding a feed does
	// not replay its entire history.
	Primed      bool
	Paused      bool
	NextFetchAt time.Time
	LastFetchAt *time.Time
	// ChangedAt is when the feed's content last actually changed, which is what
	// the adaptive poll interval is derived from. A feed that keeps returning
	// the same entries is cheap to carry and should be checked rarely.
	ChangedAt *time.Time
	LastError string
	Failures  int
	CreatedAt time.Time
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

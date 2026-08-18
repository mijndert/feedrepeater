package destination

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"feedrepeater.com/internal/mastodon"
)

// MastodonConfig is the non-secret half of a Mastodon destination.
type MastodonConfig struct {
	Host       string `json:"host"`
	Acct       string `json:"acct"`
	Visibility string `json:"visibility"`
}

// MastodonCredentials is the encrypted half.
type MastodonCredentials struct {
	AccessToken string `json:"access_token"`
}

// Visibilities are the posting visibilities offered in the UI.
var Visibilities = []string{"public", "unlisted", "private"}

// ValidVisibility reports whether v is one we offer.
func ValidVisibility(v string) bool {
	for _, ok := range Visibilities {
		if ok == v {
			return true
		}
	}
	return false
}

type mastodonTarget struct {
	client *mastodon.Client
	cfg    MastodonConfig
	token  string

	once  sync.Once
	limit int
}

// NewMastodon builds a Mastodon target from stored configuration.
func NewMastodon(client *mastodon.Client, configJSON string, credentials []byte) (Target, error) {
	var cfg MastodonConfig
	if err := jsonConfig(configJSON, &cfg); err != nil {
		return nil, Permanent(err)
	}
	host, err := mastodon.NormalizeHost(cfg.Host)
	if err != nil {
		return nil, Permanentf("mastodon: stored host is invalid: %v", err)
	}
	cfg.Host = host
	if !ValidVisibility(cfg.Visibility) {
		cfg.Visibility = "public"
	}
	var creds MastodonCredentials
	if err := json.Unmarshal(credentials, &creds); err != nil || creds.AccessToken == "" {
		return nil, Permanentf("mastodon: no access token stored")
	}
	return &mastodonTarget{client: client, cfg: cfg, token: creds.AccessToken}, nil
}

func (t *mastodonTarget) Limit(ctx context.Context) int {
	t.once.Do(func() {
		t.limit = t.client.MaxCharacters(ctx, t.cfg.Host)
	})
	if t.limit <= 0 {
		return mastodon.DefaultMaxCharacters
	}
	return t.limit
}

func (t *mastodonTarget) Send(ctx context.Context, p Post) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	st, err := t.client.PostStatus(ctx, t.cfg.Host, t.token, p.Text, t.cfg.Visibility, p.IdempotencyKey)
	if err != nil {
		return nil, classifyMastodon(err)
	}
	return &Result{URL: st.URL}, nil
}

// classifyMastodon decides whether a failure is worth retrying. A revoked
// token or a rejected status never becomes valid on its own; a 5xx might.
func classifyMastodon(err error) error {
	var he *mastodon.HTTPError
	if !errors.As(err, &he) {
		return err // transport failure: worth retrying
	}
	if retryableStatus(he.StatusCode) {
		return err
	}
	return Permanent(err)
}

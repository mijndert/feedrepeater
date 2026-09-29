// Package config loads runtime configuration from the environment.
package config

import (
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// Addr is the listen address for the HTTP server.
	Addr string
	// BaseURL is the public origin, e.g. https://feedrepeater.com. It is used
	// to build the OAuth redirect URI, so it must match what is registered.
	BaseURL *url.URL
	// DBPath is the SQLite file path.
	DBPath string
	// SecretKey is 32 bytes of root key material. All other keys derive from it.
	SecretKey []byte
	// MinPollInterval is the floor on how often any feed is fetched.
	MinPollInterval time.Duration
	// UserAgent is sent on every outbound request.
	UserAgent string
	// MaxItemsPerPoll caps how many new entries a single poll will queue, so a
	// feed that suddenly grows by 500 entries cannot flood a user's timeline.
	MaxItemsPerPoll int
	// AllowPrivateNetworks disables the SSRF guard. Development only.
	AllowPrivateNetworks bool

	// MaxAccounts caps total registrations. Zero means no cap. Signing in to
	// an existing account is never affected — this only closes the door to new
	// ones, which is the lever you want when something is going wrong.
	MaxAccounts int
	// BlockedInstances refuses sign-in from these hosts entirely.
	BlockedInstances map[string]bool

	// Contact and Jurisdiction are the two things in the terms that only the
	// operator of a deployment can fill in. Left unset, the terms page leaves
	// the section out rather than printing a blank where an address belongs.
	Contact      string
	Jurisdiction string
}

// InstanceBlocked reports whether a host has been refused by the operator.
func (c *Config) InstanceBlocked(host string) bool {
	return c.BlockedInstances[strings.ToLower(host)]
}

// Load reads configuration from the environment, applying defaults.
func Load() (*Config, error) {
	c := &Config{
		Addr:             envDefault("FR_ADDR", "127.0.0.1:8080"),
		DBPath:           envDefault("FR_DB_PATH", "feedrepeater.db"),
		UserAgent:        envDefault("FR_USER_AGENT", "feedrepeater/1.0 (+https://feedrepeater.com)"),
		MinPollInterval:  15 * time.Minute,
		MaxItemsPerPoll:  5,
		BlockedInstances: map[string]bool{},
		Contact:          strings.TrimSpace(os.Getenv("FR_CONTACT")),
		Jurisdiction:     strings.TrimSpace(os.Getenv("FR_JURISDICTION")),
	}

	raw := os.Getenv("FR_BASE_URL")
	if raw == "" {
		return nil, fmt.Errorf("FR_BASE_URL is required (e.g. https://feedrepeater.example)")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("FR_BASE_URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("FR_BASE_URL: scheme must be http or https")
	}
	if u.Host == "" {
		return nil, fmt.Errorf("FR_BASE_URL: missing host")
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawQuery, u.Fragment = "", ""
	c.BaseURL = u

	key := os.Getenv("FR_SECRET_KEY")
	if key == "" {
		return nil, fmt.Errorf("FR_SECRET_KEY is required (generate one with: feedrepeater genkey)")
	}
	c.SecretKey, err = hex.DecodeString(strings.TrimSpace(key))
	if err != nil {
		return nil, fmt.Errorf("FR_SECRET_KEY: must be hex: %w", err)
	}
	if len(c.SecretKey) != 32 {
		return nil, fmt.Errorf("FR_SECRET_KEY: must be 32 bytes (64 hex chars), got %d", len(c.SecretKey))
	}

	if v := os.Getenv("FR_MIN_POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("FR_MIN_POLL_INTERVAL: %w", err)
		}
		if d < time.Minute {
			return nil, fmt.Errorf("FR_MIN_POLL_INTERVAL: must be at least 1m")
		}
		c.MinPollInterval = d
	}
	if v := os.Getenv("FR_MAX_ITEMS_PER_POLL"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("FR_MAX_ITEMS_PER_POLL: must be a positive integer")
		}
		c.MaxItemsPerPoll = n
	}
	if os.Getenv("FR_ALLOW_PRIVATE_NETWORKS") == "1" {
		c.AllowPrivateNetworks = true
	}
	if v := os.Getenv("FR_MAX_ACCOUNTS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("FR_MAX_ACCOUNTS: must be a non-negative integer")
		}
		c.MaxAccounts = n
	}
	for _, host := range strings.Split(os.Getenv("FR_BLOCKED_INSTANCES"), ",") {
		if host = strings.ToLower(strings.TrimSpace(host)); host != "" {
			c.BlockedInstances[host] = true
		}
	}
	return c, nil
}

// Secure reports whether cookies should carry the Secure attribute.
func (c *Config) Secure() bool { return c.BaseURL.Scheme == "https" }

// AbsoluteURL joins a path onto the configured base URL.
func (c *Config) AbsoluteURL(path string) string {
	return c.BaseURL.String() + path
}

func envDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

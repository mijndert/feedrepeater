// Package safehttp provides an HTTP client for fetching user-supplied URLs.
//
// Every URL this service touches — feeds, webhooks, Mastodon instances, Bluesky
// PDS endpoints — is chosen by an untrusted user, so the client refuses to
// connect to anything that is not a public address, refuses non-HTTP schemes,
// re-checks every redirect hop, and caps how much it will read.
package safehttp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// ErrBlocked is returned when a destination is not a permitted public address.
var ErrBlocked = errors.New("destination address is not allowed")

// blocked lists ranges that must never be dialled. Anything special-purpose,
// private, or unrouteable belongs here; the check runs on the resolved IP, not
// on the hostname, so DNS entries pointing at these ranges are caught too.
var blocked = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),         // "this network"
	netip.MustParsePrefix("10.0.0.0/8"),        // private
	netip.MustParsePrefix("100.64.0.0/10"),     // CGNAT
	netip.MustParsePrefix("127.0.0.0/8"),       // loopback
	netip.MustParsePrefix("169.254.0.0/16"),    // link-local, incl. cloud metadata
	netip.MustParsePrefix("172.16.0.0/12"),     // private
	netip.MustParsePrefix("192.0.0.0/24"),      // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),      // TEST-NET-1
	netip.MustParsePrefix("192.88.99.0/24"),    // 6to4 relay anycast
	netip.MustParsePrefix("192.168.0.0/16"),    // private
	netip.MustParsePrefix("198.18.0.0/15"),     // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"),   // TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),    // TEST-NET-3
	netip.MustParsePrefix("224.0.0.0/4"),       // multicast
	netip.MustParsePrefix("240.0.0.0/4"),       // reserved, incl. 255.255.255.255
	netip.MustParsePrefix("::/96"),             // IPv4-compatible IPv6, incl. unspecified
	netip.MustParsePrefix("::1/128"),           // loopback
	netip.MustParsePrefix("64:ff9b::/96"),      // NAT64
	netip.MustParsePrefix("64:ff9b:1::/48"),    // local-use NAT64
	netip.MustParsePrefix("100::/64"),          // discard-only
	netip.MustParsePrefix("2001::/32"),         // Teredo
	netip.MustParsePrefix("2001:db8::/32"),     // documentation
	netip.MustParsePrefix("2002::/16"),         // 6to4
	netip.MustParsePrefix("fc00::/7"),          // unique local
	netip.MustParsePrefix("fe80::/10"),         // link-local
	netip.MustParsePrefix("ff00::/8"),          // multicast
	netip.MustParsePrefix("fd00:ec2::254/128"), // EC2 IMDS over IPv6
}

// AllowedIP reports whether an address may be dialled.
func AllowedIP(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	// An IPv4-mapped IPv6 address such as ::ffff:127.0.0.1 must be judged as the
	// IPv4 address it really is.
	addr = addr.Unmap()
	if addr.Zone() != "" {
		return false
	}
	if addr.IsLoopback() || addr.IsUnspecified() || addr.IsMulticast() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		addr.IsInterfaceLocalMulticast() || addr.IsPrivate() {
		return false
	}
	for _, p := range blocked {
		// Prefix.Contains is family-aware and returns false across families.
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

// Client is an HTTP client that will only talk to public hosts.
type Client struct {
	hc           *http.Client
	userAgent    string
	maxBytes     int64
	allowPrivate bool
}

// Options configures a Client.
type Options struct {
	UserAgent string
	// Timeout bounds the whole request including body read.
	Timeout time.Duration
	// MaxBytes caps how much of a response body ReadBody will return.
	MaxBytes int64
	// AllowPrivate disables the address check. Development only.
	AllowPrivate bool
}

// New builds a Client.
func New(opts Options) *Client {
	if opts.Timeout <= 0 {
		opts.Timeout = 20 * time.Second
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = 5 << 20
	}

	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	if !opts.AllowPrivate {
		// Control runs after DNS resolution with the exact address about to be
		// connected, which is what closes the DNS-rebinding window: a name that
		// resolved to a public IP during validation but a private one at connect
		// time is still rejected here.
		dialer.Control = func(network, address string, _ syscall.RawConn) error {
			switch network {
			case "tcp4", "tcp6":
			default:
				return fmt.Errorf("safehttp: %w: network %s", ErrBlocked, network)
			}
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("safehttp: %w: %s", ErrBlocked, address)
			}
			ip, err := netip.ParseAddr(host)
			if err != nil || !AllowedIP(ip) {
				return fmt.Errorf("safehttp: %w: %s", ErrBlocked, host)
			}
			return nil
		}
	}

	// Connection reuse is the single largest cost in polling, and the defaults
	// are sized for a client that talks to a handful of hosts. This one talks to
	// as many hosts as there are feeds: at 32 idle connections a busy cycle
	// evicts each one long before the same feed comes round again, so nearly
	// every fetch paid for a fresh TCP connection and a TLS handshake — far more
	// CPU and latency than reading the 304 it was going for.
	//
	// Two per host is right and stays: polls of one host are spaced deliberately
	// (see the worker), so a third concurrent connection to it would mean the
	// spacing had already failed. What was wrong was the total.
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       5 * time.Minute,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		DisableCompression:    false,
	}

	c := &Client{
		userAgent:    opts.UserAgent,
		maxBytes:     opts.MaxBytes,
		allowPrivate: opts.AllowPrivate,
	}
	c.hc = &http.Client{
		Transport: transport,
		Timeout:   opts.Timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("safehttp: too many redirects")
			}
			// The dialer validates the address of every hop; this rejects a
			// redirect that tries to leave HTTP entirely (file:, gopher:).
			if err := c.ValidateURL(req.URL); err != nil {
				return err
			}
			// Credentials must not follow a redirect to a different origin.
			// Scheme counts: an https -> http hop keeps the host but would put
			// a bearer token on the wire in cleartext.
			if len(via) > 0 && (req.URL.Host != via[0].URL.Host || req.URL.Scheme != via[0].URL.Scheme) {
				req.Header.Del("Authorization")
			}
			return nil
		},
	}
	return c
}

// ValidateURL checks a URL against this client's policy. It is the version
// call sites should use: it honours AllowPrivate, so a development server can
// accept a feed on loopback while production cannot.
func (c *Client) ValidateURL(u *url.URL) error {
	if c.allowPrivate {
		return validateShape(u)
	}
	return ValidateURL(u)
}

// ParseURL validates a user-supplied URL string against this client's policy.
func (c *Client) ParseURL(raw string) (*url.URL, error) {
	u, err := parseShape(raw)
	if err != nil {
		return nil, err
	}
	if err := c.ValidateURL(u); err != nil {
		return nil, err
	}
	return u, nil
}

// ValidateURL applies the full policy: HTTP only, no credentials, and no
// private or special-purpose address. It is a cheap pre-filter and the source
// of the user-facing error message; the dialer is the real control.
func ValidateURL(u *url.URL) error {
	if err := validateShape(u); err != nil {
		return err
	}
	host := u.Hostname()
	// Reject names that only resolve inside a private network. These would be
	// caught at dial time anyway, but failing early gives a clearer message.
	lower := strings.ToLower(strings.TrimSuffix(host, "."))
	for _, suffix := range []string{".local", ".internal", ".localdomain", ".home.arpa"} {
		if strings.HasSuffix(lower, suffix) {
			return fmt.Errorf("safehttp: %w: host %q", ErrBlocked, host)
		}
	}
	if lower == "localhost" {
		return fmt.Errorf("safehttp: %w: host %q", ErrBlocked, host)
	}
	if ip, err := netip.ParseAddr(host); err == nil && !AllowedIP(ip) {
		return fmt.Errorf("safehttp: %w: address %s", ErrBlocked, host)
	}
	return nil
}

// validateShape checks everything that holds regardless of address policy: the
// scheme must be HTTP and the URL must not carry credentials. This is the only
// part that still applies when private networks are allowed, so development
// mode loosens which addresses are reachable and nothing else.
func validateShape(u *url.URL) error {
	if u == nil {
		return fmt.Errorf("safehttp: %w: empty URL", ErrBlocked)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("safehttp: %w: scheme %q", ErrBlocked, u.Scheme)
	}
	if u.User != nil {
		return fmt.Errorf("safehttp: %w: URL must not contain credentials", ErrBlocked)
	}
	if u.Hostname() == "" {
		return fmt.Errorf("safehttp: %w: missing host", ErrBlocked)
	}
	return nil
}

// ParseURL validates and normalises a user-supplied URL string under the full
// policy. Prefer Client.ParseURL, which honours the client's configuration.
func ParseURL(raw string) (*url.URL, error) {
	u, err := parseShape(raw)
	if err != nil {
		return nil, err
	}
	if err := ValidateURL(u); err != nil {
		return nil, err
	}
	return u, nil
}

// parseShape normalises a URL string without applying address policy.
func parseShape(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("safehttp: %w: empty URL", ErrBlocked)
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("safehttp: %w: %s", ErrBlocked, err)
	}
	if err := validateShape(u); err != nil {
		return nil, err
	}
	u.Fragment, u.RawFragment = "", ""
	return u, nil
}

// Do sends a request, setting the user agent and validating the target first.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	if err := c.ValidateURL(req.URL); err != nil {
		return nil, err
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	return c.hc.Do(req)
}

// Get issues a GET.
func (c *Client) Get(ctx context.Context, rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	return c.Do(req)
}

// ReadBody reads at most MaxBytes of a response body. It reports an error if
// the body exceeds the cap rather than silently truncating, so a gzip bomb or
// an endless stream fails loudly.
func (c *Client) ReadBody(resp *http.Response) ([]byte, error) {
	return ReadLimited(resp.Body, c.maxBytes)
}

// ReadLimited reads a response under a cap tighter than the client's own.
//
// The client-wide limit has to accommodate the largest thing anything asks for,
// which is an instance API answering a verification call. A feed does not need
// that much room, and the caller that knows it should say so: the cap is what
// bounds the allocation a parse turns the bytes into.
func (c *Client) ReadLimited(resp *http.Response, max int64) ([]byte, error) {
	if max <= 0 || max > c.maxBytes {
		max = c.maxBytes
	}
	return ReadLimited(resp.Body, max)
}

// ReadLimited reads up to max bytes, erroring if there is more.
func ReadLimited(r io.Reader, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("safehttp: response larger than %d bytes", max)
	}
	return b, nil
}

// MaxBytes reports the configured body cap.
func (c *Client) MaxBytes() int64 { return c.maxBytes }

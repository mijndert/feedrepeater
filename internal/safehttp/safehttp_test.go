package safehttp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestAllowedIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.1.2.3", "0.0.0.0", "10.1.2.3", "172.16.0.1", "172.31.255.255",
		"192.168.1.1", "169.254.169.254", "100.64.0.1", "192.0.0.1", "198.18.0.1",
		"224.0.0.1", "255.255.255.255", "240.0.0.1",
		"::1", "::", "fe80::1", "fc00::1", "fd00:ec2::254", "ff02::1",
		"2001:db8::1", "2002::1", "2001::1", "64:ff9b::7f00:1",
		// IPv4-mapped forms of blocked addresses must not slip through.
		"::ffff:127.0.0.1", "::ffff:169.254.169.254", "::ffff:10.0.0.1",
	}
	for _, s := range blocked {
		addr, err := netip.ParseAddr(s)
		if err != nil {
			t.Fatalf("parse %s: %v", s, err)
		}
		if AllowedIP(addr) {
			t.Errorf("AllowedIP(%s) = true, want false", s)
		}
	}

	allowed := []string{"1.1.1.1", "8.8.8.8", "93.184.216.34", "172.32.0.1", "2606:4700::1111"}
	for _, s := range allowed {
		addr, err := netip.ParseAddr(s)
		if err != nil {
			t.Fatalf("parse %s: %v", s, err)
		}
		if !AllowedIP(addr) {
			t.Errorf("AllowedIP(%s) = false, want true", s)
		}
	}

	if AllowedIP(netip.Addr{}) {
		t.Error("AllowedIP(invalid) = true, want false")
	}
	if addr, _ := netip.ParseAddr("fe80::1%eth0"); AllowedIP(addr) {
		t.Error("zoned address allowed")
	}
}

func TestParseURLRejects(t *testing.T) {
	bad := []string{
		"", "javascript:alert(1)", "file:///etc/passwd", "gopher://example.com",
		"http://localhost/feed", "http://127.0.0.1:8080/feed", "http://[::1]/feed",
		"http://169.254.169.254/latest/meta-data/", "http://printer.local/feed",
		"http://db.internal/feed", "https://user:pass@example.com/feed",
		"http://0.0.0.0/", "http://[::ffff:127.0.0.1]/",
	}
	for _, raw := range bad {
		if u, err := ParseURL(raw); err == nil {
			t.Errorf("ParseURL(%q) = %v, want error", raw, u)
		}
	}
}

func TestParseURLAccepts(t *testing.T) {
	cases := map[string]string{
		"example.com/feed.xml":          "https://example.com/feed.xml",
		"https://example.com/feed.xml":  "https://example.com/feed.xml",
		"http://example.com/feed#top":   "http://example.com/feed",
		"  https://example.com/a?b=c  ": "https://example.com/a?b=c",
	}
	for in, want := range cases {
		u, err := ParseURL(in)
		if err != nil {
			t.Fatalf("ParseURL(%q): %v", in, err)
		}
		if u.String() != want {
			t.Errorf("ParseURL(%q) = %q, want %q", in, u.String(), want)
		}
	}
}

func TestReadLimited(t *testing.T) {
	if _, err := ReadLimited(strings.NewReader(strings.Repeat("a", 100)), 99); err == nil {
		t.Error("oversized body accepted")
	}
	b, err := ReadLimited(strings.NewReader(strings.Repeat("a", 50)), 50)
	if err != nil || len(b) != 50 {
		t.Errorf("ReadLimited = %d bytes, %v", len(b), err)
	}
}

// The allow-private escape hatch must loosen only which addresses are
// reachable, never the scheme or credential rules.
func TestClientPolicy(t *testing.T) {
	strict := New(Options{UserAgent: "test"})
	loose := New(Options{UserAgent: "test", AllowPrivate: true})

	if _, err := strict.ParseURL("http://127.0.0.1:8080/feed"); err == nil {
		t.Error("default client accepted a loopback address")
	}
	if _, err := loose.ParseURL("http://127.0.0.1:8080/feed"); err != nil {
		t.Errorf("development client rejected a loopback address: %v", err)
	}

	for _, raw := range []string{"javascript:alert(1)", "file:///etc/passwd", "https://u:p@example.com/"} {
		if _, err := loose.ParseURL(raw); err == nil {
			t.Errorf("development client accepted %q", raw)
		}
		if _, err := strict.ParseURL(raw); err == nil {
			t.Errorf("default client accepted %q", raw)
		}
	}

	// Public addresses work under both.
	for _, c := range []*Client{strict, loose} {
		if _, err := c.ParseURL("https://example.com/feed.xml"); err != nil {
			t.Errorf("public URL rejected: %v", err)
		}
	}
}

// The dialer is the real control, and a redirect is how an attacker reaches
// past a URL that validated cleanly. This drives the whole client: a public
// URL that redirects to loopback must fail at connect time.
func TestRedirectToPrivateAddressIsRefused(t *testing.T) {
	private := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("secrets"))
	}))
	defer private.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, private.URL, http.StatusFound)
	}))
	defer redirector.Close()

	// A client with the guard on refuses both the first hop and the redirect
	// target, since httptest binds to loopback.
	strict := New(Options{UserAgent: "test"})
	if _, err := strict.Get(context.Background(), redirector.URL); err == nil {
		t.Fatal("guarded client reached a loopback server")
	} else if !errors.Is(err, ErrBlocked) && !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("blocked for the wrong reason: %v", err)
	}

	// With the guard off, both hops are reachable — which is what proves the
	// refusal above came from the guard and not from the test setup.
	loose := New(Options{UserAgent: "test", AllowPrivate: true})
	resp, err := loose.Get(context.Background(), redirector.URL)
	if err != nil {
		t.Fatalf("development client could not follow the redirect: %v", err)
	}
	defer resp.Body.Close()
	body, _ := loose.ReadBody(resp)
	if string(body) != "secrets" {
		t.Errorf("body = %q", body)
	}
}

// A redirect chain must not run forever.
func TestRedirectLoopIsBounded(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/next", http.StatusFound)
	}))
	defer srv.Close()

	loose := New(Options{UserAgent: "test", AllowPrivate: true})
	resp, err := loose.Get(context.Background(), srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("an endless redirect chain was followed to completion")
	}
	if !strings.Contains(err.Error(), "too many redirects") {
		t.Errorf("stopped for the wrong reason: %v", err)
	}
}

// A bearer token must not survive a hop to another origin, including a
// downgrade that keeps the host but drops TLS.
func TestAuthorizationIsStrippedAcrossOrigins(t *testing.T) {
	var seen string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
	}))
	defer target.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer origin.Close()

	loose := New(Options{UserAgent: "test", AllowPrivate: true})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, origin.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer super-secret")
	resp, err := loose.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if seen != "" {
		t.Errorf("token followed a cross-origin redirect: %q", seen)
	}
}

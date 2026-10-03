package destination

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"feedrepeater.com/internal/safehttp"
)

func devClient() *safehttp.Client {
	return safehttp.New(safehttp.Options{UserAgent: "test", AllowPrivate: true})
}

// An endpoint that answers the challenge is accepted, in either of the two
// shapes a receiver might find easiest.
func TestVerifyEndpointAcceptsAnEchoedChallenge(t *testing.T) {
	for name, reply := range map[string]func(w http.ResponseWriter, challenge string){
		"bare body": func(w http.ResponseWriter, c string) { w.Write([]byte(c)) },
		"json field": func(w http.ResponseWriter, c string) {
			json.NewEncoder(w).Encode(map[string]string{"challenge": c})
		},
		"with whitespace": func(w http.ResponseWriter, c string) { w.Write([]byte("\n  " + c + "\n")) },
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var in struct{ Challenge string }
				body, _ := io.ReadAll(r.Body)
				if err := json.Unmarshal(body, &in); err != nil {
					t.Errorf("verification body was not JSON: %s", body)
				}
				if r.Header.Get(HeaderSignature) == "" {
					t.Error("verification request was not signed")
				}
				reply(w, in.Challenge)
			}))
			defer srv.Close()

			if err := VerifyEndpoint(context.Background(), devClient(), srv.URL, "s3cret"); err != nil {
				t.Errorf("a correct echo was rejected: %v", err)
			}
		})
	}
}

// The whole point: an address the user does not control cannot be added.
func TestVerifyEndpointRejectsStrangers(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"200 with no echo":    func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("OK")) },
		"200 empty":           func(w http.ResponseWriter, r *http.Request) {},
		"wrong challenge":     func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("deadbeef")) },
		"404":                 func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) },
		"500":                 func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) },
		"html home page":      func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html><body>hi</body></html>")) },
		"redirect to nothing": func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/gone", 302) },
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			defer srv.Close()
			if err := VerifyEndpoint(context.Background(), devClient(), srv.URL, "s3cret"); err == nil {
				t.Error("an endpoint that did not prove itself was accepted")
			}
		})
	}
}

// The guard still applies: verification must not be a way to probe private
// addresses either.
func TestVerifyEndpointRefusesPrivateAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Challenge string }
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &in)
		w.Write([]byte(in.Challenge))
	}))
	defer srv.Close()

	strict := safehttp.New(safehttp.Options{UserAgent: "test"})
	err := VerifyEndpoint(context.Background(), strict, srv.URL, "s3cret")
	if err == nil {
		t.Fatal("verification reached a loopback address")
	}
	if !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("blocked for the wrong reason: %v", err)
	}
}

// A body large enough to be a problem is refused rather than read.
func TestVerifyEndpointBoundsTheResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for range 4096 {
			w.Write([]byte(strings.Repeat("x", 1024)))
		}
	}))
	defer srv.Close()

	if err := VerifyEndpoint(context.Background(), devClient(), srv.URL, "s3cret"); err == nil {
		t.Error("an oversized response was accepted")
	}
}
